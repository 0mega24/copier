package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func makeTestFiles(t *testing.T) []string {
	t.Helper()
	root := t.TempDir()
	file := filepath.Join(root, "hello 世界 #.txt")
	if err := os.WriteFile(file, []byte("file contents\x00binary"), 0600); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(root, "folder")
	if err := os.MkdirAll(filepath.Join(folder, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "nested.bin"), []byte{0, 1, 255}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return []string{file, folder}
}

func TestFileManagerBundleRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := context.Background()
	paths := makeTestFiles(t)
	native := nativeFilePacket(paths)
	decoded, err := clipboardFilePaths(native)
	if err != nil || fmt.Sprint(decoded) != fmt.Sprint(paths) {
		t.Fatalf("URI parsing %v %v", decoded, err)
	}
	packed, err := transportClipboardFiles(ctx, native)
	if err != nil {
		t.Fatal(err)
	}
	if !packed.valid() || strings.Contains(string(packed.Data), filepath.Dir(paths[0])) {
		t.Fatal("source path leaked into archive")
	}
	restored, err := prepareDesktopFiles(ctx, packed)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := clipboardFilePaths(restored)
	if err != nil {
		t.Fatal(err)
	}
	if receiver[0] == paths[0] || !strings.HasPrefix(receiver[0], receivedDir()) {
		t.Fatal("did not materialize files locally")
	}
	data, err := os.ReadFile(receiver[0])
	if err != nil || string(data) != "file contents\x00binary" {
		t.Fatal("file bytes changed", err)
	}
	info, err := os.Stat(filepath.Join(receiver[1], "run.sh"))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("executable permission", err)
	}
	if info, err := os.Stat(filepath.Join(receiver[1], "empty")); err != nil || !info.IsDir() {
		t.Fatal("empty directory lost", err)
	}
	recaptured, err := transportClipboardFiles(ctx, restored)
	if err != nil || !equalPackets(packed, recaptured) {
		t.Fatal("file copy echoes or changed", err)
	}
	repeated, err := prepareDesktopFiles(ctx, packed)
	if err != nil || !equalPackets(restored, repeated) {
		t.Fatal("cache reuse", err)
	}
	destination := filepath.Join(t.TempDir(), "received")
	extracted, err := extractFiles(ctx, packed, destination)
	if err != nil || len(extracted) != 2 {
		t.Fatal("explicit extract", err)
	}
	if _, err := extractFiles(ctx, packed, destination); err == nil {
		t.Fatal("overwrote existing destination")
	}
}

func TestClipboardURIValidationAndCut(t *testing.T) {
	paths := makeTestFiles(t)
	p := nativeFilePacket(paths)
	f := p.Formats["x-special/gnome-copied-files"]
	f.Data = bytes.Replace(f.Data, []byte("copy\n"), []byte("cut\n"), 1)
	p.Formats["x-special/gnome-copied-files"] = f
	packed, err := transportClipboardFiles(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	local, err := prepareDesktopFiles(context.Background(), packed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(local.Formats["x-special/gnome-copied-files"].Data, []byte("copy\n")) || string(local.Formats["application/x-kde-cutselection"].Data) != "0" {
		t.Fatal("cut propagated")
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Fatal("source removed")
	}
	for _, uri := range []string{"file://other-host/etc/passwd", "file:///tmp/x?query=1", "file:///tmp/x#fragment", "file:///tmp/%00", "file:relative"} {
		packet := Packet{MIME: "text/uri-list", Data: []byte(uri)}
		if _, err := clipboardFilePaths(packet); err == nil {
			t.Errorf("accepted %s", uri)
		}
	}
	text := Packet{MIME: "text/plain", Data: []byte((&url.URL{Scheme: "file", Path: paths[0]}).String())}
	unchanged, err := transportClipboardFiles(context.Background(), text)
	if err != nil || !equalPackets(text, unchanged) {
		t.Fatal("plain text read filesystem")
	}
	link := Packet{MIME: "text/uri-list", Data: []byte("https://example.com/link\r\n")}
	unchanged, err = transportClipboardFiles(context.Background(), link)
	if err != nil || !equalPackets(link, unchanged) {
		t.Fatal("copied hyperlink misidentified as files", err)
	}
}

func TestFileLimitsAndCustomReceivingDirectory(t *testing.T) {
	ctx := context.Background()
	source := filepath.Join(t.TempDir(), "oversized")
	f, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(maxClip + 1)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = packFiles(ctx, []string{source}); err == nil {
		t.Fatal("accepted oversized source")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = packFiles(cancelled, makeTestFiles(t)); !errors.Is(err, context.Canceled) {
		t.Fatal("capture ignored cancellation", err)
	}
	p, err := packFiles(ctx, makeTestFiles(t))
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(t.TempDir(), "Downloads", "Copier")
	native, err := prepareDesktopFilesIn(ctx, p, base)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := clipboardFilePaths(native)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(paths[0], base+string(filepath.Separator)) {
		t.Fatal("ignored custom receive directory", paths)
	}
}

func TestDuplicateNamesAndSourceSymlinks(t *testing.T) {
	ctx := context.Background()
	var names []string
	for i := 0; i < 2; i++ {
		name := filepath.Join(t.TempDir(), "same.txt")
		if err := os.WriteFile(name, []byte(fmt.Sprintf("%d", i)), 0600); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	p, err := packFiles(ctx, names)
	if err != nil {
		t.Fatal(err)
	}
	output, err := extractFiles(ctx, p, filepath.Join(t.TempDir(), "received"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(output[0]) != "same.txt" || filepath.Base(output[1]) != "same (2).txt" {
		t.Fatal(output)
	}
	recaptured, err := packFiles(ctx, output)
	if err != nil || !equalPackets(p, recaptured) {
		t.Fatal("collision naming unstable", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(names[0], link); err != nil {
		t.Fatal(err)
	}
	if _, err := packFiles(ctx, []string{link}); err == nil {
		t.Fatal("followed source symlink")
	}
	dir := t.TempDir()
	if err := os.Symlink(names[0], filepath.Join(dir, "nested-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := packFiles(ctx, []string{dir}); err == nil {
		t.Fatal("followed nested symlink")
	}
}

func badArchive(t *testing.T, name string, mode os.FileMode, method uint16, data []byte) Packet {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	_ = w.SetComment(fileBundleComment)
	h := &zip.FileHeader{Name: name, Method: method}
	h.SetMode(mode)
	f, err := w.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	return Packet{MIME: fileBundleMIME, Data: b.Bytes()}
}
func TestUnsafeArchiveAndCorruption(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		mode   os.FileMode
		method uint16
	}{{"../escape", 0600, zip.Store}, {"/absolute", 0600, zip.Store}, {"a\\b", 0600, zip.Store}, {"a/../b", 0600, zip.Store}, {"a/b", 0600, zip.Store}, {"link", os.ModeSymlink | 0777, zip.Store}, {"compressed", 0600, zip.Deflate}} {
		p := badArchive(t, test.name, test.mode, test.method, []byte("bad"))
		destination := filepath.Join(t.TempDir(), "received")
		if p.valid() {
			t.Errorf("valid unsafe archive %q", test.name)
		}
		if _, err := extractFiles(ctx, p, destination); err == nil {
			t.Errorf("extracted %q", test.name)
		}
		if _, err := os.Stat(destination); !os.IsNotExist(err) {
			t.Fatal("published failed extract")
		}
	}
	p := badArchive(t, "file", 0600, zip.Store, []byte("some payload"))
	at := bytes.Index(p.Data, []byte("some payload"))
	if at < 0 {
		t.Fatal("payload missing")
	}
	p.Data[at] ^= 255
	parent := t.TempDir()
	destination := filepath.Join(parent, "received")
	if _, err := extractFiles(ctx, p, destination); err == nil {
		t.Fatal("ignored CRC mismatch")
	}
	items, _ := os.ReadDir(parent)
	if len(items) != 0 {
		t.Fatal("partial files leaked")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	good, err := packFiles(ctx, makeTestFiles(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extractFiles(cancelled, good, destination); !errors.Is(err, context.Canceled) {
		t.Fatal("ignored cancellation", err)
	}
}

func TestMaterializedEditsAndConcurrentDestination(t *testing.T) {
	ctx := context.Background()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p, err := packFiles(ctx, makeTestFiles(t))
	if err != nil {
		t.Fatal(err)
	}
	first, err := prepareDesktopFiles(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	paths, _ := clipboardFilePaths(first)
	if err = os.WriteFile(paths[0], []byte("local edit"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := prepareDesktopFiles(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := clipboardFilePaths(second)
	if other[0] == paths[0] {
		t.Fatal("reused changed files")
	}
	edited, _ := os.ReadFile(paths[0])
	if string(edited) != "local edit" {
		t.Fatal("overwrote local edit")
	}
	destination := filepath.Join(t.TempDir(), "received")
	var wg sync.WaitGroup
	out := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := extractFiles(ctx, p, destination); out <- err }()
	}
	wg.Wait()
	close(out)
	count := 0
	for err := range out {
		if err == nil {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("concurrent destination succeeded %d times", count)
	}
}

func TestFileBundleRelayConflictAndRetry(t *testing.T) {
	_, client := fixture(t)
	ctx := context.Background()
	p, err := packFiles(ctx, makeTestFiles(t))
	if err != nil {
		t.Fatal(err)
	}
	first := writeFor(client, t, "")
	first.MIME = p.MIME
	first.Data = p.Data
	stale := first
	stale.ID = newID()
	accepted, err := client.put(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.put(ctx, stale)
	var conflict *Conflict
	if !errors.As(err, &conflict) {
		t.Fatal("stale bundle accepted", err)
	}
	repeated, err := client.put(ctx, first)
	if err != nil || repeated.Revision != accepted.Revision {
		t.Fatal("bundle retry duplicated", err)
	}
	fetched, err := client.get(ctx, accepted.Revision)
	if err != nil || !equalPackets(p, fetched.packet()) {
		t.Fatal("bundle changed over relay", err)
	}
}
