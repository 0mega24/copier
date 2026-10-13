package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func publishOffer(t *testing.T, c *Client, p Packet) Offer {
	t.Helper()
	o, err := packetOffer(p)
	if err != nil {
		t.Fatal(err)
	}
	current, err := c.get(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.put(context.Background(), Write{Epoch: current.Epoch, ID: newID(), Base: current.Revision, Origin: "test", MIME: p.MIME, Data: p.Data}); err != nil {
		t.Fatal(err)
	}
	return o
}
func chunkCall(t *testing.T, c *Client, method string, o Offer, index int, chunk int64, device string, data []byte) *http.Response {
	t.Helper()
	response, err := c.binaryRequest(context.Background(), method, chunkURL(o.ID, index, chunk, device), data)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
func statusClose(t *testing.T, r *http.Response, want int) {
	t.Helper()
	defer r.Body.Close()
	if r.StatusCode != want {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("status %d want %d: %s", r.StatusCode, want, b)
	}
}

func TestThreeGiBOfferDoesNotReadOrUploadContents(t *testing.T) {
	r, c := fixture(t)
	ctx := context.Background()
	name := filepath.Join(t.TempDir(), "large.iso")
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(3 << 30); err != nil {
		t.Fatal(err)
	}
	f.Close()
	start := time.Now()
	packet, err := announceFiles(ctx, []string{name})
	if err != nil {
		t.Fatal(err)
	}
	o := publishOffer(t, c, packet)
	if o.Entries[0].Size != 3<<30 || len(packet.Data) > 4096 {
		t.Fatal("large file was not a small metadata offer")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("large copy announcement unexpectedly slow")
	}
	if r.chunkBytes != 0 {
		t.Fatal("uploaded data on copy")
	}
	paths, err := c.lazyOfferPaths(ctx, o, "")
	if err != nil || len(paths) != 1 || paths[0] != name {
		t.Fatal("local paste did not bypass relay", paths, err)
	}
	if _, err = os.Stat(r.chunkRoot()); !os.IsNotExist(err) {
		t.Fatal("local paste created transfer state")
	}
	statusClose(t, chunkCall(t, c, "GET", o, 0, 0, o.Device, nil), 400)
	if r.chunkBytes != 0 {
		t.Fatal("same-device chunk request uploaded content")
	}
}

func TestRemoteDemandIdempotencySourceMutationAndQuota(t *testing.T) {
	r, c := fixture(t)
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "file.bin")
	content := bytes.Repeat([]byte{0x5a}, int(2*chunkSize))
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
	p, err := announceFiles(ctx, []string{file})
	if err != nil {
		t.Fatal(err)
	}
	o := publishOffer(t, c, p)
	receiver := newID()
	statusClose(t, chunkCall(t, c, "GET", o, 0, 0, receiver, nil), 202)
	if r.chunkBytes != 0 {
		t.Fatal("request itself moved source bytes")
	}
	if err := c.agentStep(ctx); err != nil {
		t.Fatal(err)
	}
	response := chunkCall(t, c, "GET", o, 0, 0, receiver, nil)
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || !bytes.Equal(data, content[:chunkSize]) {
		t.Fatal("chunk changed", err)
	}
	if response.Header.Get("X-Chunk-SHA256") != digest(data) || r.chunkBytes != chunkSize {
		t.Fatal("invalid receipt/accounting")
	}
	statusClose(t, chunkCall(t, c, "PUT", o, 0, 0, o.Device, data), 200)
	if r.chunkBytes != chunkSize {
		t.Fatal("duplicate upload charged twice")
	}
	corrupt := append([]byte(nil), data...)
	corrupt[0] ^= 1
	statusClose(t, chunkCall(t, c, "PUT", o, 0, 0, o.Device, corrupt), 422)
	statusClose(t, chunkCall(t, c, "PUT", o, 0, 1, receiver, data), 403)
	// A new chunk must fail after a file changes, rather than mixing versions.
	if err = os.WriteFile(file, bytes.Repeat([]byte{1}, len(content)), 0600); err != nil {
		t.Fatal(err)
	}
	statusClose(t, chunkCall(t, c, "GET", o, 0, 1, receiver, nil), 202)
	if err = c.agentStep(ctx); err != nil {
		t.Fatal(err)
	}
	statusClose(t, chunkCall(t, c, "GET", o, 0, 1, receiver, nil), 410)
	if r.chunkBytes != chunkSize {
		t.Fatal("uploaded changed source")
	}
	r.chunkQuota = chunkSize
	other, err := announceFiles(ctx, makeTestFiles(t))
	if err != nil {
		t.Fatal(err)
	}
	otherOffer := publishOffer(t, c, other)
	statusClose(t, chunkCall(t, c, "GET", otherOffer, 0, 0, receiver, nil), 507)
}

func TestChunkCacheSurvivesRestartAndHistoryChange(t *testing.T) {
	r, c := fixture(t)
	ctx := context.Background()
	packet, err := announceFiles(ctx, makeTestFiles(t))
	if err != nil {
		t.Fatal(err)
	}
	o := publishOffer(t, c, packet)
	device := newID()
	statusClose(t, chunkCall(t, c, "GET", o, 0, 0, device, nil), 202)
	// Metadata changes while this transfer is pending do not retarget its source.
	current, _ := c.get(ctx, 0)
	_, err = c.put(ctx, Write{Epoch: current.Epoch, ID: newID(), Base: current.Revision, MIME: "text/plain", Data: []byte("new clipboard")})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.agentStep(ctx); err != nil {
		t.Fatal(err)
	}
	reloaded, err := newRelay(r.path, r.token)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(reloaded)
	defer server.Close()
	fresh, err := newClient(Config{server.URL, r.token, "default"}, false)
	if err != nil {
		t.Fatal(err)
	}
	response := chunkCall(t, fresh, "GET", o, 0, 0, device, nil)
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(data) != "file contents\x00binary" {
		t.Fatal("chunk lost on relay restart", err)
	}
	if reloaded.chunkBytes != int64(len(data)) {
		t.Fatal("restart accounting")
	}
}

func TestMetadataValidationAndLocalClipboardBypass(t *testing.T) {
	r, c := fixture(t)
	ctx := context.Background()
	original := Packet{MIME: "text/plain", Data: []byte("private local text")}
	p, err := announcePacket(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	o := publishOffer(t, c, p)
	if bytes.Contains(p.Data, original.Data) {
		t.Fatal("clipboard bytes were announced")
	}
	var out bytes.Buffer
	if err = c.streamOfferFormat(ctx, o, "", &out); err != nil || !bytes.Equal(out.Bytes(), original.Data) {
		t.Fatal("local clipboard bypass", err)
	}
	if r.chunkBytes != 0 {
		t.Fatal("local text was uploaded")
	}
	o.Entries[0].Path = "../escape"
	if offerPacket(o).valid() {
		t.Fatal("unsafe offer path valid")
	}
	o, _ = packetOffer(p)
	o.Entries[0].Bits = 7
	if offerPacket(o).valid() {
		t.Fatal("invalid representation units valid")
	}
	id, err := deviceID()
	if err != nil {
		t.Fatal(err)
	}
	again, err := deviceID()
	if err != nil || id != again {
		t.Fatal("device identity unstable")
	}
}

func TestTransferExpiryAndCachedIntegrity(t *testing.T) {
	r, c := fixture(t)
	ctx := context.Background()
	packet, err := announceFiles(ctx, makeTestFiles(t))
	if err != nil {
		t.Fatal(err)
	}
	o := publishOffer(t, c, packet)
	receiver := newID()
	statusClose(t, chunkCall(t, c, "GET", o, 0, 0, receiver, nil), 202)
	if err = c.agentStep(ctx); err != nil {
		t.Fatal(err)
	}
	base := chunkBase(r.transferDir("default", o.ID), 0, 0)
	if err = os.WriteFile(base+".bin", []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	statusClose(t, chunkCall(t, c, "GET", o, 0, 0, receiver, nil), 500)
	o.Expires = time.Now().Add(-time.Minute).Unix()
	if err = saveJSON(filepath.Join(r.transferDir("default", o.ID), "offer.json"), o); err != nil {
		t.Fatal(err)
	}
	statusClose(t, chunkCall(t, c, "GET", o, 0, 0, receiver, nil), 410)
	r.mu.Lock()
	err = r.cleanChunksLocked()
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if r.chunkBytes != 0 {
		t.Fatal("expired cache was retained")
	}
}

func TestOfferMetadataNeverIncludesAbsoluteSourcePaths(t *testing.T) {
	fixture(t)
	paths := makeTestFiles(t)
	p, err := announceFiles(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(p.Data, []byte(filepath.Dir(paths[0]))) {
		t.Fatal("source path exposed")
	}
	var value map[string]any
	if err = json.Unmarshal(p.Data, &value); err != nil {
		t.Fatal(err)
	}
}
