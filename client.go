package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Config struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	Board string `json:"board"`
}
type Client struct {
	config     Config
	http       *http.Client
	allowHTTP  bool
	cacheMu    sync.Mutex
	chunkCache map[string][]byte
	chunkOrder []string
	prefetched map[string]bool
}
type Conflict struct{ Current Clip }

func (c *Conflict) Error() string {
	return fmt.Sprintf("clipboard changed to revision %d; copy was not applied", c.Current.Revision)
}

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("relay HTTP %d: %s", e.Status, e.Message) }

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func equalBytes(a, b []byte) bool { return bytes.Equal(a, b) }
func digest(b []byte) string      { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func configDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	return filepath.Join(base, "copier")
}
func stateDir() string {
	if s := os.Getenv("XDG_STATE_HOME"); s != "" {
		return filepath.Join(s, "copier")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".local", "state", "copier")
}
func readConfig() (Config, error) {
	c := Config{Board: "default"}
	if err := loadJSON(filepath.Join(configDir(), "config.json"), &c); err != nil && !isNotExist(err) {
		return c, err
	}
	for k, p := range map[string]*string{"COPIER_URL": &c.URL, "COPIER_TOKEN": &c.Token, "COPIER_BOARD": &c.Board} {
		if v, ok := os.LookupEnv(k); ok {
			*p = v
		}
	}
	return c, nil
}
func newClient(c Config, allowHTTP bool) (*Client, error) {
	u, err := url.Parse(c.URL)
	if err != nil || u == nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("set a valid http(s) relay URL using join or COPIER_URL")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	if u.Scheme == "http" && !loopback && !allowHTTP {
		return nil, fmt.Errorf("remote HTTP requires --allow-http for a trusted VPN; use HTTPS or an SSH tunnel")
	}
	if len(c.Token) < 16 {
		return nil, fmt.Errorf("token must have at least 16 characters; use COPIER_TOKEN or join --token-file")
	}
	if !namePattern.MatchString(c.Board) {
		return nil, fmt.Errorf("board must contain 1–64 letters, digits, underscores or hyphens")
	}
	c.URL = strings.TrimRight(c.URL, "/")
	return &Client{config: c, allowHTTP: allowHTTP, http: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) request(ctx context.Context, method, suffix string, body any, out any) error {
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			// Crypto-random jitter prevents all reconnecting machines retrying together.
			var j [1]byte
			_, _ = rand.Read(j[:])
			wait := time.Duration(200*(1<<attempt)+int(j[0])) * time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		req, e := http.NewRequestWithContext(ctx, method, c.config.URL+"/v1/boards/"+c.config.Board+suffix, bytes.NewReader(b))
		if e != nil {
			return e
		}
		req.Header.Set("Authorization", "Bearer "+c.config.Token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, e := c.http.Do(req)
		if e != nil {
			err = e
			continue
		}
		data, e := io.ReadAll(io.LimitReader(resp.Body, 180<<20))
		resp.Body.Close()
		if e != nil {
			err = e
			continue
		}
		if resp.StatusCode == 409 {
			var current Clip
			if e = json.Unmarshal(data, &current); e != nil {
				return e
			}
			return &Conflict{current}
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out == nil {
				return nil
			}
			return json.Unmarshal(data, out)
		}
		var msg struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &msg)
		err = &APIError{resp.StatusCode, msg.Error}
		if resp.StatusCode != 429 && resp.StatusCode < 500 {
			return err
		}
	}
	return err
}
func (c *Client) get(ctx context.Context, rev uint64) (Clip, error) {
	var out Clip
	suffix := ""
	if rev > 0 {
		suffix = fmt.Sprintf("/%d", rev)
	}
	err := c.request(ctx, "GET", suffix, nil, &out)
	return out, err
}
func (c *Client) put(ctx context.Context, w Write) (Clip, error) {
	var out Clip
	err := c.request(ctx, "PUT", "", w, &out)
	return out, err
}
func (c *Client) history(ctx context.Context) ([]Clip, error) {
	var out []Clip
	err := c.request(ctx, "GET", "/history", nil, &out)
	return out, err
}
func origin() string {
	if v := os.Getenv("COPIER_DEVICE"); v != "" {
		return v
	}
	h, _ := os.Hostname()
	return h
}

type Pending struct {
	URL   string `json:"url"`
	Board string `json:"board"`
	Write Write  `json:"write"`
}

// A CLI copy is persisted before its first network attempt. Failure or an
// ambiguous response leaves exactly the same operation available for replay.
func (c *Client) copy(ctx context.Context, data []byte, mime string, base *uint64) (Clip, error) {
	return c.copyPacket(ctx, Packet{MIME: mime, Data: data}, base)
}
func (c *Client) copyPacket(ctx context.Context, packet Packet, base *uint64) (Clip, error) {
	prepared, err := announcePacket(ctx, packet)
	if err != nil {
		return Clip{}, err
	}
	packet = prepared
	if packet.MIME == localOnlyMIME {
		return Clip{}, fmt.Errorf("clipboard is local-only; explicitly copy its contents to share")
	}
	if !packet.valid() {
		return Clip{}, fmt.Errorf("invalid clipboard bundle (32 MiB maximum)")
	}
	cur, err := c.get(ctx, 0)
	if err != nil {
		return Clip{}, err
	}
	if packet.MIME == offerMIME {
		offer, _ := packetOffer(packet)
		device, err := deviceID()
		if err != nil {
			return Clip{}, err
		}
		if device == offer.Device {
			if err = c.ensureAgent(); err != nil {
				return Clip{}, fmt.Errorf("could not start source agent: %w", err)
			}
		}
	}
	rev := cur.Revision
	if base != nil {
		rev = *base
	}
	p := Pending{c.config.URL, c.config.Board, Write{Epoch: cur.Epoch, ID: newID(), Base: rev, Origin: origin(), MIME: packet.MIME, Data: packet.Data, Formats: packet.Formats}}
	path := filepath.Join(stateDir(), "pending", p.Write.ID+".json")
	if err := saveJSON(path, p); err != nil {
		return Clip{}, err
	}
	out, err := c.put(ctx, p.Write)
	if err != nil {
		return Clip{}, fmt.Errorf("%w; retained at %s (replay with copier retry --file PATH)", err, path)
	}
	if err = os.Remove(path); err != nil {
		return out, fmt.Errorf("copied revision %d, but could not remove pending receipt %s: %w", out.Revision, path, err)
	}
	return out, nil
}

func retryPending(ctx context.Context, c *Client, path string, force bool) error {
	lock, err := lockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	var p Pending
	if err = loadJSON(path, &p); err != nil {
		return err
	}
	if p.URL != c.config.URL || p.Board != c.config.Board {
		return errors.New("pending copy belongs to another relay/board")
	}
	if p.Write.MIME == offerMIME {
		if err = c.ensureAgent(); err != nil {
			return err
		}
	}
	if force {
		cur, err := c.get(ctx, 0)
		if err != nil {
			return err
		}
		// A force retry intentionally promotes the retained data to head with a
		// new operation ID; it can create a new revision even if the old ID committed.
		p.Write.ID = newID()
		p.Write.Base = cur.Revision
		p.Write.Epoch = cur.Epoch
		if err = saveJSON(path, p); err != nil {
			return err
		}
	}
	out, err := c.put(ctx, p.Write)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "copied revision %d\n", out.Revision)
	return os.Remove(path)
}
