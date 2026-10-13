package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func (c *Client) binaryRequest(ctx context.Context, method, suffix string, data []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.config.URL+"/v1/boards/"+c.config.Board+suffix, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.config.Token)
	if method == "PUT" {
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("X-Chunk-SHA256", digest(data))
	}
	client := *c.http
	client.Timeout = 2 * time.Minute
	return client.Do(req)
}
func responseError(resp *http.Response) error {
	defer resp.Body.Close()
	var value struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&value)
	return &APIError{Status: resp.StatusCode, Message: value.Error}
}

func (c *Client) readChunk(ctx context.Context, o Offer, index int, chunk int64) ([]byte, error) {
	expected, err := chunkLength(o, index, chunk)
	if err != nil {
		return nil, err
	}
	device, err := deviceID()
	if err != nil {
		return nil, err
	}
	if device == o.Device {
		file, err := openOfferedFile(o, index)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		data := make([]byte, expected)
		n, err := file.ReadAt(data, chunk*chunkSize)
		if err != nil && !(err == io.EOF && int64(n) == expected) {
			return nil, err
		}
		record, err := ownedOffer(o.ID)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil || fingerprint(info) != record.Bindings[index].Stat {
			return nil, fmt.Errorf("source changed during local read")
		}
		return data, nil
	}
	if c.http == nil {
		return nil, fmt.Errorf("configure the relay endpoint to read a remote offer")
	}
	cache := chunkBase(filepath.Join(stateDir(), "chunks", digest([]byte(c.config.URL + "\n" + c.config.Board))[:20], o.ID), index, chunk)
	if data := c.cachedChunk(cache); data != nil {
		return data, nil
	}
	var receipt ChunkReceipt
	if loadJSON(cache+".receipt.json", &receipt) == nil {
		if data, err := os.ReadFile(cache + ".bin"); err == nil && int64(len(data)) == expected && digest(data) == receipt.Hash && receipt.Size == expected {
			c.rememberChunk(cache, data)
			return data, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	c.prefetchChunks(ctx, o, index, chunk)
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("waiting for source device: %w", err)
		}
		resp, err := c.binaryRequest(ctx, "GET", chunkURL(o.ID, index, chunk, device), nil)
		if err != nil {
			failures++
			if failures >= 5 {
				return nil, err
			}
		} else {
			switch resp.StatusCode {
			case 200:
				data, err := io.ReadAll(io.LimitReader(resp.Body, chunkSize+1))
				hash := resp.Header.Get("X-Chunk-SHA256")
				resp.Body.Close()
				if err != nil || int64(len(data)) != expected || len(hash) != 64 || digest(data) != hash {
					failures++
					if failures >= 5 {
						return nil, fmt.Errorf("chunk transfer failed integrity check")
					}
					break
				}
				if err = saveBytes(cache+".bin", data); err != nil {
					return nil, err
				}
				if err = saveJSON(cache+".receipt.json", ChunkReceipt{Hash: hash, Size: expected}); err != nil {
					return nil, err
				}
				c.rememberChunk(cache, data)
				return data, nil
			case 202:
				resp.Body.Close()
				failures = 0
			default:
				return nil, responseError(resp)
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (c *Client) cachedChunk(key string) []byte {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	return c.chunkCache[key]
}
func (c *Client) rememberChunk(key string, data []byte) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if c.chunkCache == nil {
		c.chunkCache = map[string][]byte{}
	}
	if _, ok := c.chunkCache[key]; ok {
		return
	}
	if len(c.chunkOrder) >= 8 {
		delete(c.chunkCache, c.chunkOrder[0])
		c.chunkOrder = c.chunkOrder[1:]
	}
	c.chunkOrder = append(c.chunkOrder, key)
	c.chunkCache[key] = data
}

// Only a content read starts this bounded three-chunk lookahead. Clipboard
// observation, file attributes and directory listings never reach this path.
func (c *Client) prefetchChunks(ctx context.Context, o Offer, index int, chunk int64) {
	device, err := deviceID()
	if err != nil {
		return
	}
	for next := chunk + 1; next <= chunk+3; next++ {
		if _, err := chunkLength(o, index, next); err != nil {
			break
		}
		key := chunkURL(o.ID, index, next, device)
		c.cacheMu.Lock()
		if c.prefetched == nil {
			c.prefetched = map[string]bool{}
		}
		seen := c.prefetched[key]
		c.prefetched[key] = true
		c.cacheMu.Unlock()
		if seen {
			continue
		}
		go func(suffix string) {
			response, err := c.binaryRequest(ctx, "HEAD", suffix, nil)
			if err == nil {
				response.Body.Close()
			}
		}(key)
	}
}

func (c *Client) uploadJob(ctx context.Context, job ChunkJob) error {
	record, err := ownedOffer(job.Offer.ID)
	if err != nil {
		return c.failJob(ctx, job)
	}
	if record.Offer.Device != job.Offer.Device {
		return c.failJob(ctx, job)
	}
	data, err := c.readChunk(ctx, record.Offer, job.Index, job.Chunk)
	if err != nil {
		return c.failJob(ctx, job)
	}
	// readChunk verifies the original file's fingerprint before and after every
	// chunk; no request may direct the source outside its registered bindings.
	resp, err := c.binaryRequest(ctx, "PUT", chunkURL(job.Offer.ID, job.Index, job.Chunk, job.Offer.Device), data)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return responseError(resp)
	}
	defer resp.Body.Close()
	var receipt ChunkReceipt
	if err = json.NewDecoder(resp.Body).Decode(&receipt); err != nil {
		return err
	}
	if receipt.Hash != digest(data) || receipt.Size != int64(len(data)) {
		return fmt.Errorf("relay receipt checksum mismatch")
	}
	return nil
}
func (c *Client) failJob(ctx context.Context, job ChunkJob) error {
	resp, err := c.binaryRequest(ctx, "POST", chunkURL(job.Offer.ID, job.Index, job.Chunk, job.Offer.Device), nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return responseError(resp)
	}
	resp.Body.Close()
	return nil
}
func agentKey(c *Client, device string) string {
	return digest([]byte(c.config.URL + "\n" + c.config.Board + "\n" + device))[:20]
}
func (c *Client) agentStep(ctx context.Context) error {
	device, err := deviceID()
	if err != nil {
		return err
	}
	var jobs []ChunkJob
	if err = c.request(ctx, "GET", "/jobs?device="+device+"&wait=1", nil, &jobs); err != nil {
		return err
	}
	for _, job := range jobs {
		if err = c.uploadJob(ctx, job); err != nil {
			return err
		}
	}
	return nil
}
func (c *Client) runAgent(ctx context.Context) error {
	device, err := deviceID()
	if err != nil {
		return err
	}
	lock, err := lockFile(filepath.Join(stateDir(), "agent-"+agentKey(c, device)+".lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	pidPath := filepath.Join(stateDir(), "agent-"+agentKey(c, device)+".pid.json")
	if err = saveJSON(pidPath, struct {
		PID int `json:"pid"`
	}{os.Getpid()}); err != nil {
		return err
	}
	defer os.Remove(pidPath)
	fmt.Fprintln(os.Stderr, "source agent active; file/clipboard bytes are served only for remote read requests")
	for {
		if err = c.agentStep(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "source agent waiting:", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Millisecond):
		}
	}
}
func (c *Client) childEnv() []string {
	return append(os.Environ(), "COPIER_URL="+c.config.URL, "COPIER_TOKEN="+c.config.Token, "COPIER_BOARD="+c.config.Board)
}
func (c *Client) ensureAgent() error {
	if os.Getenv("COPIER_NO_AUTOSTART") == "1" {
		return nil
	}
	device, err := deviceID()
	if err != nil {
		return err
	}
	key := agentKey(c, device)
	lock, err := lockFile(filepath.Join(stateDir(), "agent-"+key+".lock"))
	if err != nil {
		return nil
	}
	lock.Close()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"agent"}
	if c.allowHTTP {
		args = append(args, "--allow-http")
	}
	log, err := os.OpenFile(filepath.Join(stateDir(), "agent-"+key+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(executable, args...)
	cmd.Env = c.childEnv()
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func (c *Client) copyEntry(ctx context.Context, o Offer, index int, dst io.Writer) error {
	e := o.Entries[index]
	if e.Dir {
		return fmt.Errorf("entry is a directory")
	}
	if e.Size == 0 {
		_, err := c.readChunk(ctx, o, index, 0)
		return err
	}
	for chunk := int64(0); chunk < (e.Size+chunkSize-1)/chunkSize; chunk++ {
		data, err := c.readChunk(ctx, o, index, chunk)
		if err != nil {
			return err
		}
		if _, err = dst.Write(data); err != nil {
			return err
		}
	}
	return nil
}
func (c *Client) extractOffer(ctx context.Context, o Offer, destination string) ([]string, error) {
	if o.Kind != "files" {
		return nil, fmt.Errorf("clipboard offer is not a file selection")
	}
	full, err := filepath.Abs(destination)
	if err != nil {
		return nil, err
	}
	if _, err = os.Lstat(full); !isNotExist(err) {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("destination already exists")
	}
	if err = os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		return nil, err
	}
	temp, err := os.MkdirTemp(filepath.Dir(full), ".copier-offer-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temp)
	root, err := os.OpenRoot(temp)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	for i, e := range o.Entries {
		if e.Dir {
			if err = root.Mkdir(e.Path, 0700); err != nil {
				return nil, err
			}
			continue
		}
		f, err := root.OpenFile(e.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(e.Mode))
		if err != nil {
			return nil, err
		}
		err = c.copyEntry(ctx, o, i, f)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	for i := len(o.Entries) - 1; i >= 0; i-- {
		if o.Entries[i].Dir {
			f, err := root.Open(o.Entries[i].Path)
			if err != nil {
				return nil, err
			}
			err = f.Sync()
			f.Close()
			if err != nil {
				return nil, err
			}
		}
	}
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	err = f.Sync()
	f.Close()
	if err != nil {
		return nil, err
	}
	if err = renameNoReplace(temp, full); err != nil {
		return nil, err
	}
	parent, err := os.Open(filepath.Dir(full))
	if err != nil {
		return nil, err
	}
	err = parent.Sync()
	parent.Close()
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(o.Roots))
	for i, name := range o.Roots {
		paths[i] = filepath.Join(full, name)
	}
	return paths, nil
}
func (c *Client) resolveOffer(ctx context.Context, o Offer) (Packet, error) {
	if o.Kind != "clipboard" {
		return Packet{}, fmt.Errorf("use paste --files or paste --local for file objects")
	}
	formats := map[string]Format{}
	for i, e := range o.Entries {
		var out boundedBuffer
		out.max = maxClip
		if err := c.copyEntry(ctx, o, i, &out); err != nil {
			return Packet{}, err
		}
		formats[e.Name] = Format{Data: out.Bytes(), Type: e.Type, Bits: e.Bits}
	}
	f := formats[o.MIME]
	return Packet{MIME: o.MIME, Data: f.Data, Formats: formats}, nil
}
func (c *Client) streamOfferFormat(ctx context.Context, o Offer, name string, w io.Writer) error {
	if o.Kind != "clipboard" {
		return fmt.Errorf("use paste --files for a file selection")
	}
	if name == "" {
		name = o.MIME
	}
	for i, e := range o.Entries {
		if e.Name == name {
			return c.copyEntry(ctx, o, i, w)
		}
	}
	return fmt.Errorf("clipboard type %q is unavailable", name)
}

func parseOfferInput(r io.Reader) (Offer, error) {
	data, err := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	if err != nil {
		return Offer{}, err
	}
	return packetOffer(Packet{MIME: offerMIME, Data: data})
}
