package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

type fakeClipboard struct {
	packet Packet
	reads  int
	onRead func(int)
	writes int
}

func (f *fakeClipboard) ReadPacket(context.Context) (Packet, error) {
	f.reads++
	if f.onRead != nil {
		f.onRead(f.reads)
	}
	return f.packet, nil
}
func (f *fakeClipboard) WritePacket(_ context.Context, p Packet) error {
	f.packet = desktopProjection(p)
	f.writes++
	return nil
}
func textPacket(s string) Packet {
	return desktopProjection(Packet{MIME: "text/plain", Data: []byte(s)})
}

func TestSyncEchoAndNetworkWindow(t *testing.T) {
	_, c := fixture(t)
	ctx := context.Background()
	cb := &fakeClipboard{packet: textPacket("initial")}
	s := &Syncer{client: c, clipboard: cb, path: filepath.Join(t.TempDir(), "sync.json")}
	if err := s.step(ctx); err != nil {
		t.Fatal(err)
	}
	req := writeFor(c, t, "from server")
	if _, err := c.put(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := s.step(ctx); err != nil {
		t.Fatal(err)
	}
	if cb.writes != 1 || string(cb.packet.Data) != "from server" {
		t.Fatal("no desktop paste")
	}
	if err := s.step(ctx); err != nil {
		t.Fatal(err)
	}
	clips, _ := c.history(ctx)
	if len(clips) != 1 {
		t.Fatal("echo loop")
	}
	req = writeFor(c, t, "remote race")
	if _, err := c.put(ctx, req); err != nil {
		t.Fatal(err)
	}
	cb.onRead = func(n int) {
		if n == cb.reads && n == 6 {
			cb.packet = textPacket("new local")
		}
	}
	// Reads: init=1, first pull=2/3, no-op=4, race pull=5/6.
	if err := s.step(ctx); err != nil {
		t.Fatal(err)
	}
	if string(cb.packet.Data) != "new local" || cb.writes != 1 {
		t.Fatal("overwrote local change during GET")
	}
	err := s.step(ctx)
	var conflict *Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("wanted conflict: %v", err)
	}
	var saved SyncState
	if err := loadJSON(s.path, &saved); err != nil || saved.Pending == nil || string(saved.Pending.Data) != "new local" {
		t.Fatal("local conflict not persisted", err)
	}
}

func TestOfflinePendingSurvivesRestart(t *testing.T) {
	_, c := fixture(t)
	ctx := context.Background()
	cb := &fakeClipboard{packet: textPacket("initial")}
	s := &Syncer{client: c, clipboard: cb, path: filepath.Join(t.TempDir(), "sync.json")}
	if err := s.step(ctx); err != nil {
		t.Fatal(err)
	}
	cb.packet = textPacket("offline screenshot metadata")
	original := c.config.URL
	c.config.URL = "http://127.0.0.1:1"
	if err := s.step(ctx); err == nil {
		t.Fatal("offline accepted")
	}
	c.config.URL = original
	restored := &Syncer{client: c, clipboard: cb, path: s.path}
	if err := loadJSON(s.path, &restored.state); err != nil {
		t.Fatal(err)
	}
	if restored.state.Pending == nil {
		t.Fatal("outbox lost")
	}
	id := restored.state.Pending.ID
	if err := restored.step(ctx); err != nil {
		t.Fatal(err)
	}
	cur, _ := c.get(ctx, 0)
	if cur.ID != id || string(cur.Data) != "offline screenshot metadata" {
		t.Fatal("did not replay pending")
	}
}

func TestSyncAdoptsCLIFileCopyWithoutEchoOrConflict(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	_, c := fixture(t)
	ctx := context.Background()
	cb := &fakeClipboard{packet: textPacket("initial")}
	s := &Syncer{client: c, clipboard: cb, path: filepath.Join(t.TempDir(), "sync.json")}
	if err := s.step(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := packFiles(ctx, makeTestFiles(t))
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := c.copyPacket(ctx, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	cb.packet = p
	if err := s.step(ctx); err != nil {
		t.Fatal("CLI local copy caused false conflict", err)
	}
	if s.state.Pending != nil || s.state.Revision != accepted.Revision {
		t.Fatal("did not adopt matching head")
	}
	if err := s.step(ctx); err != nil {
		t.Fatal(err)
	}
	history, err := c.history(ctx)
	if err != nil || len(history) != 1 {
		t.Fatal("echoed CLI file copy", err)
	}
}
