package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func fixture(t *testing.T) (*Relay, *Client) {
	t.Helper()
	t.Setenv("COPIER_NO_AUTOSTART", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	r, err := newRelay(filepath.Join(t.TempDir(), "db.json"), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	c, err := newClient(Config{server.URL, r.token, "default"}, false)
	if err != nil {
		t.Fatal(err)
	}
	return r, c
}
func writeFor(c *Client, t *testing.T, data string) Write {
	t.Helper()
	cur, err := c.get(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return Write{Epoch: cur.Epoch, ID: newID(), Base: cur.Revision, Origin: "test", MIME: "text/plain", Data: []byte(data)}
}
func TestConcurrentCAS(t *testing.T) {
	_, c := fixture(t)
	const writers = 24
	requests := make([]Write, writers)
	for i := range requests {
		requests[i] = writeFor(c, t, "concurrent")
	}
	var accepted, conflicts atomic.Int32
	var wg sync.WaitGroup
	for _, req := range requests {
		wg.Add(1)
		go func(w Write) {
			defer wg.Done()
			_, err := c.put(context.Background(), w)
			if err == nil {
				accepted.Add(1)
			} else {
				var conflict *Conflict
				if errors.As(err, &conflict) {
					conflicts.Add(1)
				} else {
					t.Error(err)
				}
			}
		}(req)
	}
	wg.Wait()
	if accepted.Load() != 1 || conflicts.Load() != writers-1 {
		t.Fatalf("accepted %d conflicts %d", accepted.Load(), conflicts.Load())
	}
	history, err := c.history(context.Background())
	if err != nil || len(history) != 1 {
		t.Fatalf("history %d %v", len(history), err)
	}
}
func TestIdempotencyPersistenceAndBinaryBundle(t *testing.T) {
	r, c := fixture(t)
	req := writeFor(c, t, "plain\x00text")
	req.Formats = map[string]Format{"text/plain": {Data: req.Data, Type: "text/plain", Bits: 8}, "text/html": {Data: []byte("<b>plain</b>"), Type: "text/html", Bits: 8}, "image/png": {Data: []byte{0x89, 'P', 'N', 'G', 0, 255}, Type: "image/png", Bits: 8}, "application/x-private": {Data: []byte{1, 0, 2, 0}, Type: "application/x-private", Bits: 16}}
	first, err := c.put(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	later := writeFor(c, t, "later")
	if _, err = c.put(context.Background(), later); err != nil {
		t.Fatal(err)
	}
	again, err := c.put(context.Background(), req)
	if err != nil || again.Revision != first.Revision || !equalPackets(again.packet(), req.packet()) {
		t.Fatalf("retry: %#v %v", again, err)
	}
	reloaded, err := newRelay(r.path, r.token)
	if err != nil {
		t.Fatal(err)
	}
	if latest(reloaded.db.Boards["default"]).Revision != 2 || reloaded.db.Epoch != r.db.Epoch {
		t.Fatal("state not preserved")
	}
	req.Data = []byte("changed")
	req.Formats["text/plain"] = Format{Data: req.Data, Type: "text/plain", Bits: 8}
	_, err = c.put(context.Background(), req)
	var api *APIError
	if !errors.As(err, &api) || api.Status != 422 {
		t.Fatalf("ID reuse: %v", err)
	}
}
func TestLostResponseRetriesSameOperation(t *testing.T) {
	r, _ := fixture(t)
	var dropped atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "PUT" && dropped.CompareAndSwap(false, true) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		r.ServeHTTP(w, req)
	}))
	defer server.Close()
	c, err := newClient(Config{server.URL, r.token, "default"}, false)
	if err != nil {
		t.Fatal(err)
	}
	req := writeFor(c, t, "once")
	clip, err := c.put(context.Background(), req)
	if err != nil || clip.Revision != 1 {
		t.Fatalf("lost reply: %v %#v", err, clip)
	}
	if len(r.db.Boards["default"].Clips) != 1 {
		t.Fatal("duplicate write")
	}
}
func TestAuthValidationAndEpoch(t *testing.T) {
	r, c := fixture(t)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/boards/default", nil))
	if rr.Code != 401 {
		t.Fatal(rr.Code)
	}
	req := writeFor(c, t, "stale")
	req.Epoch = newID()
	_, err := c.put(context.Background(), req)
	var conflict *Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("stale epoch: %v", err)
	}
	req.Epoch = r.db.Epoch
	req.Formats = map[string]Format{"text/plain": {Data: []byte("different"), Type: "text/plain", Bits: 8}}
	_, err = c.put(context.Background(), req)
	var api *APIError
	if !errors.As(err, &api) || api.Status != 400 {
		t.Fatalf("bundle validation: %v", err)
	}
	if _, err = newClient(Config{"http://192.0.2.1", r.token, "default"}, false); err == nil {
		t.Fatal("remote HTTP allowed")
	}
}
func TestPersistenceFailureDoesNotPublish(t *testing.T) {
	r, c := fixture(t)
	req := writeFor(c, t, "uncommitted")
	r.path = t.TempDir() // rename onto a directory fails
	b, _ := json.Marshal(req)
	rr := httptest.NewRecorder()
	request := httptest.NewRequest("PUT", "/v1/boards/default", bytes.NewReader(b))
	request.Header.Set("Authorization", "Bearer "+r.token)
	r.ServeHTTP(rr, request)
	if rr.Code != 500 || latest(r.db.Boards["default"]).Revision != 0 {
		t.Fatalf("published failed write: %d", rr.Code)
	}
}
func TestHistoryRetentionAndExpiredRetry(t *testing.T) {
	_, c := fixture(t)
	first := writeFor(c, t, "0")
	if _, err := c.put(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < historyLimit; i++ {
		req := writeFor(c, t, "new")
		if _, err := c.put(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	clips, err := c.history(context.Background())
	if err != nil || len(clips) != historyLimit {
		t.Fatalf("history: %d %v", len(clips), err)
	}
	_, err = c.put(context.Background(), first)
	var conflict *Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("expired retry re-applied: %v", err)
	}
}
func TestFileLockAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "db.json")
	lock, err := lockFile(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if second, err := lockFile(path + ".lock"); err == nil {
		second.Close()
		t.Fatal("second owner accepted")
	}
	if err := saveJSON(path, map[string]string{"secret": "data"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("permissions", err)
	}
}
