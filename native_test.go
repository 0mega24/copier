package main

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

// Uses a private X11 selection, never the user's CLIPBOARD or PRIMARY.
func TestNativeX11Bundle(t *testing.T) {
	if os.Getenv("COPIER_NATIVE_TEST") != "1" {
		t.Skip("set COPIER_NATIVE_TEST=1 with an X11 display to test a private selection")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	selection := "copier_test_" + newID()
	cb := &NativeClipboard{Backend: "x11", Selection: selection}
	defer func() {
		script := `import ctypes as c,sys
x=c.CDLL("libX11.so.6")
x.XOpenDisplay.restype=c.c_void_p
x.XInternAtom.argtypes=[c.c_void_p,c.c_char_p,c.c_int]
x.XInternAtom.restype=c.c_ulong
x.XSetSelectionOwner.argtypes=[c.c_void_p,c.c_ulong,c.c_ulong,c.c_ulong]
x.XSync.argtypes=[c.c_void_p,c.c_int]
x.XCloseDisplay.argtypes=[c.c_void_p]
d=x.XOpenDisplay(None)
if d:
 a=x.XInternAtom(d,sys.argv[1].upper().encode(),0)
 x.XSetSelectionOwner(d,a,0,0)
 x.XSync(d,0)
 x.XCloseDisplay(d)
`
		if out, err := exec.Command("python3", "-c", script, selection).CombinedOutput(); err != nil {
			t.Logf("cleanup: %s %v", out, err)
		}
	}()
	packet := Packet{MIME: "text/plain;charset=utf-8", Data: []byte("rich clipboard"), Formats: map[string]Format{
		"text/plain;charset=utf-8": {Data: []byte("rich clipboard"), Type: "text/plain;charset=utf-8", Bits: 8},
		"text/html":                {Data: []byte("<b>rich clipboard</b>"), Type: "text/html", Bits: 8},
		"image/png":                {Data: []byte{137, 80, 78, 71, 0, 255, 0}, Type: "image/png", Bits: 8},
		"application/x-test":       {Data: []byte{0, 1, 2, 255}, Type: "application/x-test", Bits: 8},
		"application/x-test16":     {Data: []byte{1, 0, 2, 0}, Type: "application/x-test16", Bits: 16},
		"application/x-test32":     {Data: []byte{1, 0, 0, 0, 2, 0, 0, 0}, Type: "application/x-test32", Bits: 32},
	}}
	if err := cb.WritePacket(context.Background(), packet); err != nil {
		t.Fatal(err)
	}
	actual, err := cb.ReadRawPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !equalPackets(packet, actual) {
		t.Fatalf("bundle changed: expected %#v got %#v", packet, actual)
	}
	// Exercise text alias projection separately to ensure sync suppresses echoes.
	text := Packet{MIME: "text/plain", Data: []byte("hello")}
	if err := cb.WritePacket(context.Background(), text); err != nil {
		t.Fatal(err)
	}
	actual, err = cb.ReadRawPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !equalPackets(desktopProjection(text), actual) {
		t.Fatalf("projection differs: %#v", actual)
	}
	// A file manager publishes URIs; the sender must transport file bytes. The
	// receiver then publishes different, valid local URIs for the same snapshot.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	paths := makeTestFiles(t)
	if err := cb.WritePacket(context.Background(), nativeFilePacket(paths)); err != nil {
		t.Fatal(err)
	}
	files, err := cb.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	offer, err := packetOffer(files)
	if err != nil || offer.Kind != "files" {
		t.Fatal("file-manager copy did not create a file offer", err)
	}
	if err := cb.WritePacket(context.Background(), files); err != nil {
		t.Fatal(err)
	}
	roundTrip, err := cb.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !equalPackets(files, roundTrip) {
		t.Fatal("native file bundle changed or echoes")
	}
}
