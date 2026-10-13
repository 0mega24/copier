package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:embed native_clipboard.py
var nativeHelper string

//go:embed wayland_clipboard.py
var waylandHelper string

//go:embed protocols/wlr-data-control.xml
var wlrProtocol string

//go:embed protocols/ext-data-control.xml
var extProtocol string

type Clipboard interface {
	ReadPacket(context.Context) (Packet, error)
	WritePacket(context.Context, Packet) error
}
type NativeClipboard struct {
	Backend, Selection, FileDir string
	Client                      *Client
}

func nativeClipboard(backend, selection string) (*NativeClipboard, error) {
	if selection != "clipboard" && selection != "primary" {
		return nil, fmt.Errorf("selection must be clipboard or primary")
	}
	if backend == "auto" {
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			backend = "wayland"
		} else if os.Getenv("DISPLAY") != "" {
			backend = "x11"
		} else {
			return nil, fmt.Errorf("no desktop display; use copy/paste over SSH without desktop flags")
		}
	}
	if backend != "wayland" && backend != "x11" {
		return nil, fmt.Errorf("backend must be auto, wayland or x11")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		return nil, fmt.Errorf("native clipboard requires Python3 and GTK3 runtime libraries")
	}
	return &NativeClipboard{Backend: backend, Selection: selection}, nil
}

type boundedBuffer struct {
	bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		return 0, fmt.Errorf("clipboard exceeds %d bytes", b.max)
	}
	return b.Buffer.Write(p)
}
func (c *NativeClipboard) command(ctx context.Context, mode string) *exec.Cmd {
	xml, _ := json.Marshal([]string{wlrProtocol, extProtocol})
	code := "PROTOCOL_XMLS = " + string(xml) + "\n" + waylandHelper + "\n" + nativeHelper
	cmd := exec.CommandContext(ctx, "python3", "-c", code, mode, c.Selection)
	backend := "x11"
	if c.Backend == "wayland" {
		backend = "wayland"
	}
	cmd.Env = append(os.Environ(), "GDK_BACKEND="+backend)
	return cmd
}
func (c *NativeClipboard) ReadRawPacket(ctx context.Context) (Packet, error) {
	return c.readNative(ctx, "read")
}
func (c *NativeClipboard) ReadForCopy(ctx context.Context) (Packet, error) {
	return c.readNative(ctx, "read-publish")
}
func (c *NativeClipboard) readNative(ctx context.Context, mode string) (Packet, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := c.command(ctx, mode)
	out := &boundedBuffer{max: 46 << 20}
	var diagnostic bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = &diagnostic
	if err := cmd.Run(); err != nil {
		return Packet{}, fmt.Errorf("native read: %w: %s", err, diagnostic.String())
	}
	var packet Packet
	if err := json.Unmarshal(out.Bytes(), &packet); err != nil {
		return packet, err
	}
	if !packet.valid() {
		return packet, fmt.Errorf("invalid native bundle")
	}
	return packet, nil
}
func (c *NativeClipboard) ReadPacket(ctx context.Context) (Packet, error) {
	raw, err := c.ReadRawPacket(ctx)
	if err != nil {
		return raw, err
	}
	return announcePacket(ctx, raw)
}
func (c *NativeClipboard) WritePacket(ctx context.Context, packet Packet) error {
	return c.writeNative(ctx, packet, false)
}
func (c *NativeClipboard) WriteLocalPacket(ctx context.Context, packet Packet) error {
	return c.writeNative(ctx, packet, true)
}
func (c *NativeClipboard) writeNative(ctx context.Context, packet Packet, private bool) error {
	if !packet.valid() {
		return fmt.Errorf("invalid clipboard bundle")
	}
	prepared, err := c.PreparePacket(ctx, packet)
	if err != nil {
		return err
	}
	packet = prepared
	if private {
		if len(packet.Formats) == 0 {
			packet = desktopProjection(packet)
		}
		packet.Formats[localOnlyMIME] = Format{Data: []byte(newID()), Type: localOnlyMIME, Bits: 8}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var value any = packet
	if marker, ok := packet.Formats[offerMIME]; ok {
		offer, err := packetOffer(Packet{MIME: offerMIME, Data: marker.Data})
		if err != nil {
			return err
		}
		if offer.Kind == "clipboard" {
			executable, err := os.Executable()
			if err != nil {
				return err
			}
			client, err := c.offerClient()
			if err != nil {
				return err
			}
			value = map[string]any{"mime": packet.MIME, "data": packet.Data, "formats": packet.Formats, "lazy": map[string]any{"offer": offer, "executable": executable, "allow_http": client.allowHTTP}}
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	cmd := c.command(ctx, "write")
	if c.Client != nil {
		cmd.Env = append(cmd.Env, "COPIER_URL="+c.Client.config.URL, "COPIER_TOKEN="+c.Client.config.Token, "COPIER_BOARD="+c.Client.config.Board)
	}
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (c *NativeClipboard) PreparePacket(ctx context.Context, packet Packet) (Packet, error) {
	if packet.MIME == offerMIME {
		o, err := packetOffer(packet)
		if err != nil {
			return Packet{}, err
		}
		client, err := c.offerClient()
		if err != nil {
			return Packet{}, err
		}
		var prepared Packet
		if o.Kind == "files" {
			paths, err := client.lazyOfferPaths(ctx, o, c.FileDir)
			if err != nil {
				return Packet{}, err
			}
			prepared = nativeFilePacket(paths)
		} else {
			prepared = Packet{MIME: o.MIME, Data: []byte{}, Formats: map[string]Format{}}
			for _, e := range o.Entries {
				prepared.Formats[e.Name] = Format{Data: []byte{}, Type: e.Type, Bits: e.Bits}
			}
		}
		prepared.Formats[offerMIME] = Format{Data: packet.Data, Type: offerMIME, Bits: 8}
		return prepared, nil
	}
	return prepareDesktopFilesIn(ctx, packet, c.FileDir)
}
func (c *NativeClipboard) offerClient() (*Client, error) {
	if c.Client != nil {
		return c.Client, nil
	}
	config, err := readConfig()
	if err != nil {
		return nil, err
	}
	client, err := newClient(config, false)
	if err != nil {
		return &Client{}, nil
	}
	return client, nil
}

func prepareClipboardWrite(ctx context.Context, cb Clipboard, packet Packet) (Packet, error) {
	if preparer, ok := cb.(interface {
		PreparePacket(context.Context, Packet) (Packet, error)
	}); ok {
		return preparer.PreparePacket(ctx, packet)
	}
	return packet, nil
}
func packetTypes(p Packet) []string {
	if len(p.Formats) == 0 {
		return []string{p.MIME}
	}
	names := make([]string, 0, len(p.Formats))
	for n := range p.Formats {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Match the representations and preferred target produced by the native owner,
// so aliases added for plain CLI text do not get copied back as a new event.
func desktopProjection(p Packet) Packet {
	// Native reads repackage materialized files as the same deterministic archive,
	// rather than comparing machine-specific URI paths or advertising ZIP data.
	if isFileBundle(p) || p.MIME == offerMIME {
		return p
	}
	if len(p.Formats) == 0 {
		p.Formats = map[string]Format{p.MIME: {Data: p.Data, Type: p.MIME, Bits: 8}}
		if strings.HasPrefix(p.MIME, "text/plain") {
			for _, name := range []string{"UTF8_STRING", "text/plain;charset=utf-8", "text/plain"} {
				p.Formats[name] = Format{Data: p.Data, Type: name, Bits: 8}
			}
		}
	}
	names := packetTypes(p)
	preferred := names[0]
	for _, name := range []string{"text/plain;charset=utf-8", "UTF8_STRING", "text/plain", "image/png", "text/html"} {
		if _, ok := p.Formats[name]; ok {
			preferred = name
			break
		}
	}
	p.MIME = preferred
	p.Data = p.Formats[preferred].Data
	return p
}

type SyncState struct {
	Epoch    string `json:"epoch"`
	Ready    bool   `json:"ready"`
	Revision uint64 `json:"revision"`
	Last     Packet `json:"last"`
	Pending  *Write `json:"pending,omitempty"`
}
type Syncer struct {
	client    *Client
	clipboard Clipboard
	path      string
	state     SyncState
}

func syncPath(c *Client, backend, selection string) string {
	return filepath.Join(stateDir(), "sync-"+digest([]byte(c.config.URL + "\n" + c.config.Board + "\n" + backend + "\n" + selection))[:20]+".json")
}
func (s *Syncer) save() error { return saveJSON(s.path, s.state) }

func (s *Syncer) step(ctx context.Context) error {
	local, err := s.clipboard.ReadPacket(ctx)
	if err != nil {
		return err
	}
	if local.MIME == localOnlyMIME {
		return nil
	}
	if !s.state.Ready {
		remote, err := s.client.get(ctx, 0)
		if err != nil {
			return err
		}
		// First start observes both sides, without publishing or replacing an old
		// desktop clipboard. Only subsequent local changes are automatically sent.
		s.state = SyncState{Epoch: remote.Epoch, Ready: true, Revision: remote.Revision, Last: local}
		return s.save()
	}
	newPending := false
	if s.state.Pending == nil && !equalPackets(local, s.state.Last) {
		s.state.Pending = &Write{Epoch: s.state.Epoch, ID: newID(), Base: s.state.Revision, Origin: origin(), MIME: local.MIME, Data: local.Data, Formats: local.Formats}
		if err := s.save(); err != nil {
			return err
		}
		newPending = true
	}
	if s.state.Pending != nil {
		if s.state.Pending.MIME == offerMIME {
			if err := s.client.ensureAgent(); err != nil {
				return err
			}
		}
		// A prior attempt to persist may have failed while the pending operation
		// remained in memory. Never send until this snapshot is durably saved.
		if err := s.save(); err != nil {
			return err
		}
		if newPending {
			// A local CLI copy may have already updated both the relay and the
			// desktop. Adopt that matching head instead of creating a false conflict
			// or echoing it. Existing pending operations always retain their IDs.
			head, err := s.client.get(ctx, 0)
			if err != nil {
				return err
			}
			if head.Epoch == s.state.Epoch && head.Revision >= s.state.Revision && head.Revision > 0 && equalPackets(local, desktopProjection(head.packet())) {
				s.state.Revision = head.Revision
				s.state.Last = local
				s.state.Pending = nil
				return s.save()
			}
		}
		pending := *s.state.Pending
		remote, err := s.client.put(ctx, pending)
		if err != nil {
			return err
		}
		s.state.Revision = remote.Revision
		s.state.Last = pending.packet()
		s.state.Pending = nil
		if err := s.save(); err != nil {
			return err
		}
		// Sample again on the next iteration, because the desktop could have changed
		// while the queued operation was in flight.
		return nil
	}
	remote, err := s.client.get(ctx, 0)
	if err != nil {
		return err
	}
	if remote.Epoch != s.state.Epoch {
		return fmt.Errorf("relay database changed; use a new board/state after preserving local content")
	}
	if remote.Revision < s.state.Revision {
		return fmt.Errorf("relay revision went backwards; restore the original database or use a new board")
	}
	if remote.Revision == s.state.Revision {
		return nil
	}
	// Materialize file bundles before the final clipboard recheck, so extracting
	// a directory tree does not extend the desktop ownership race window.
	pastePacket, err := prepareClipboardWrite(ctx, s.clipboard, remote.packet())
	if err != nil {
		return err
	}
	// Recheck after network I/O. If an application copied something during the
	// fetch, keep that local copy and let the next iteration attempt its CAS write.
	now, err := s.clipboard.ReadPacket(ctx)
	if err != nil {
		return err
	}
	if !equalPackets(local, now) {
		return nil
	}
	if err = s.clipboard.WritePacket(ctx, pastePacket); err != nil {
		return err
	}
	s.state.Revision = remote.Revision
	s.state.Last = desktopProjection(remote.packet())
	s.state.Epoch = remote.Epoch
	return s.save()
}

func runSync(ctx context.Context, c *Client, cb Clipboard, path string, interval time.Duration, resolve string) error {
	lock, err := lockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	s := &Syncer{client: c, clipboard: cb, path: path}
	if err = loadJSON(path, &s.state); err != nil && !isNotExist(err) {
		return err
	}
	if resolve != "" {
		if resolve != "local" && resolve != "remote" {
			return fmt.Errorf("resolve must be local or remote")
		}
		if s.state.Pending == nil {
			return fmt.Errorf("no pending conflict to resolve in %s", path)
		}
		remote, err := c.get(ctx, 0)
		if err != nil {
			return err
		}
		if resolve == "local" {
			s.state.Pending.ID = newID()
			s.state.Pending.Base = remote.Revision
			s.state.Pending.Epoch = remote.Epoch
			s.state.Epoch = remote.Epoch
		} else {
			// Explicit resolution allows replacement; archive the pending local content.
			archive := filepath.Join(stateDir(), "pending", s.state.Pending.ID+".json")
			if err = saveJSON(archive, Pending{c.config.URL, c.config.Board, *s.state.Pending}); err != nil {
				return err
			}
			pastePacket, err := prepareClipboardWrite(ctx, cb, remote.packet())
			if err != nil {
				return err
			}
			local, err := cb.ReadPacket(ctx)
			if err != nil {
				return err
			}
			if !equalPackets(local, s.state.Pending.packet()) {
				return fmt.Errorf("desktop changed since conflict; save it with copy --from-clipboard before resolving remote")
			}
			if remote.Revision > 0 {
				if err = cb.WritePacket(ctx, pastePacket); err != nil {
					return err
				}
			}
			s.state.Pending = nil
			s.state.Revision = remote.Revision
			s.state.Last = desktopProjection(remote.packet())
			s.state.Epoch = remote.Epoch
		}
		if err = s.save(); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "sync active; durable state: %s\n", path)
	for {
		err = s.step(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			var conflict *Conflict
			var api *APIError
			if errors.As(err, &conflict) {
				return fmt.Errorf("%w; local content retained in %s; restart sync with --resolve local or --resolve remote", err, path)
			}
			if errors.As(err, &api) && api.Status < 500 && api.Status != 429 {
				return err
			}
			// Retry transient outages without discarding the durable pending operation.
			fmt.Fprintf(os.Stderr, "sync waiting: %v\n", err)
		}
		delay := interval
		if err != nil && delay < 2*time.Second {
			delay = 2 * time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

func readContent(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxClip+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxClip {
		return nil, fmt.Errorf("content exceeds 32 MiB")
	}
	return b, nil
}
