package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxClip = 32 << 20
const historyLimit = 100
const historyBytes = 128 << 20

type Format struct {
	Data []byte `json:"data"`
	Type string `json:"type"`
	Bits int    `json:"bits"`
}
type Packet struct {
	MIME    string            `json:"mime"`
	Data    []byte            `json:"data"`
	Formats map[string]Format `json:"formats,omitempty"`
}

func (p Packet) size() int {
	n := len(p.Data)
	for _, f := range p.Formats {
		n += len(f.Data)
	}
	return n
}
func (p Packet) valid() bool {
	if p.MIME == "" || len(p.MIME) > 256 || strings.ContainsAny(p.MIME, "\x00\r\n") || p.size() > maxClip || len(p.Formats) > 128 {
		return false
	}
	for name, f := range p.Formats {
		if name == "" || len(name) > 256 || strings.ContainsAny(name+f.Type, "\x00\r\n") || len(f.Type) > 256 || f.Type == "" || (f.Bits != 8 && f.Bits != 16 && f.Bits != 32) || len(f.Data)%(f.Bits/8) != 0 {
			return false
		}
	}
	if len(p.Formats) > 0 {
		f, ok := p.Formats[p.MIME]
		if !ok || !equalBytes(f.Data, p.Data) {
			return false
		}
	}
	if isFileBundle(p) {
		if len(p.Formats) != 0 {
			return false
		}
		if _, err := inspectFileArchive(p.Data); err != nil {
			return false
		}
	}
	if p.MIME == offerMIME {
		if len(p.Formats) != 0 {
			return false
		}
		if _, err := packetOffer(p); err != nil {
			return false
		}
	}
	return true
}
func (c Clip) packet() Packet  { return Packet{c.MIME, c.Data, c.Formats} }
func (w Write) packet() Packet { return Packet{w.MIME, w.Data, w.Formats} }
func equalPackets(a, b Packet) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return equalBytes(x, y)
}

type Clip struct {
	Epoch    string            `json:"epoch"`
	Revision uint64            `json:"revision"`
	ID       string            `json:"id"`
	Base     uint64            `json:"base"`
	Origin   string            `json:"origin"`
	MIME     string            `json:"mime"`
	Data     []byte            `json:"data"`
	Formats  map[string]Format `json:"formats,omitempty"`
	Created  time.Time         `json:"created"`
}
type Write struct {
	Epoch   string            `json:"epoch"`
	ID      string            `json:"id"`
	Base    uint64            `json:"base"`
	Origin  string            `json:"origin"`
	MIME    string            `json:"mime"`
	Data    []byte            `json:"data"`
	Formats map[string]Format `json:"formats,omitempty"`
}
type Board struct {
	Clips []Clip `json:"clips"`
}
type Database struct {
	Epoch  string           `json:"epoch"`
	Boards map[string]Board `json:"boards"`
}
type Relay struct {
	mu                     sync.Mutex
	db                     Database
	path, token            string
	chunkBytes, chunkQuota int64
	lastChunkCleanup       time.Time
	jobsChanged            chan struct{}
}

func newRelay(path, token string) (*Relay, error) {
	r := &Relay{path: path, token: token, db: Database{Boards: map[string]Board{}}, chunkQuota: 20 << 30, jobsChanged: make(chan struct{})}
	if err := loadJSON(path, &r.db); err != nil && !errors.Is(err, io.EOF) && !isNotExist(err) {
		return nil, err
	}
	if r.db.Boards == nil {
		r.db.Boards = map[string]Board{}
	}
	if r.db.Epoch == "" {
		r.db.Epoch = newID()
		if err := saveJSON(path, r.db); err != nil {
			return nil, err
		}
	}
	if err := r.cleanChunksLocked(); err != nil {
		return nil, err
	}
	return r, nil
}

func latest(b Board) Clip {
	if len(b.Clips) == 0 {
		return Clip{}
	}
	return b.Clips[len(b.Clips)-1]
}

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func respond(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Relay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	given := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	expected := sha256.Sum256([]byte(s.token))
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare(given[:], expected[:]) != 1 {
		respond(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if s.handleChunks(w, r, parts) {
		return
	}
	if len(parts) < 3 || len(parts) > 4 || parts[0] != "v1" || parts[1] != "boards" || !namePattern.MatchString(parts[2]) {
		respond(w, 404, map[string]string{"error": "unknown endpoint"})
		return
	}
	name := parts[2]
	if r.Method == "GET" {
		s.mu.Lock()
		board := s.db.Boards[name]
		epoch := s.db.Epoch
		s.mu.Unlock()
		if len(parts) == 3 {
			current := latest(board)
			current.Epoch = epoch
			respond(w, 200, current)
			return
		}
		if parts[3] == "history" {
			respond(w, 200, board.Clips)
			return
		}
		rev, err := strconv.ParseUint(parts[3], 10, 64)
		if err == nil {
			for _, c := range board.Clips {
				if c.Revision == rev {
					respond(w, 200, c)
					return
				}
			}
		}
		respond(w, 404, map[string]string{"error": "revision not retained"})
		return
	}
	if r.Method != "PUT" || len(parts) != 3 {
		respond(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	var req Write
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 46<<20))
	dec.DisallowUnknownFields()
	err := dec.Decode(&req)
	if err == nil {
		var extra any
		if dec.Decode(&extra) != io.EOF {
			err = fmt.Errorf("trailing JSON")
		}
	}
	if err != nil || req.MIME == localOnlyMIME || !idPattern.MatchString(req.ID) || !req.packet().valid() || len(req.Origin) > 128 {
		respond(w, 400, map[string]string{"error": "invalid write (maximum bundle 32 MiB, 128 formats)"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	board := s.db.Boards[name]
	if req.Epoch != s.db.Epoch {
		current := latest(board)
		current.Epoch = s.db.Epoch
		respond(w, 409, current)
		return
	}
	// Check idempotency before the base revision: a committed retry has an old base.
	for _, c := range board.Clips {
		if c.ID == req.ID {
			if c.Base != req.Base || c.Origin != req.Origin || !equalPackets(c.packet(), req.packet()) {
				respond(w, 422, map[string]string{"error": "request ID reused with different content"})
				return
			}
			respond(w, 200, c)
			return
		}
	}
	current := latest(board)
	if current.Revision != req.Base {
		respond(w, 409, current)
		return
	}
	if current.Revision == ^uint64(0) {
		respond(w, 507, map[string]string{"error": "revision exhausted"})
		return
	}
	if _, ok := s.db.Boards[name]; !ok && len(s.db.Boards) >= 32 {
		respond(w, 507, map[string]string{"error": "board limit reached"})
		return
	}
	c := Clip{Epoch: s.db.Epoch, Revision: current.Revision + 1, ID: req.ID, Base: req.Base, Origin: req.Origin, MIME: req.MIME, Data: req.Data, Formats: req.Formats, Created: time.Now().UTC()}
	clips := append(append([]Clip(nil), board.Clips...), c)
	size := 0
	start := len(clips)
	for start > 0 && len(clips)-start < historyLimit {
		n := clips[start-1].packet().size()
		if size+n > historyBytes {
			break
		}
		size += n
		start--
	}
	next := Database{Epoch: s.db.Epoch, Boards: make(map[string]Board, len(s.db.Boards)+1)}
	for k, v := range s.db.Boards {
		next.Boards[k] = v
	}
	next.Boards[name] = Board{Clips: clips[start:]}
	if err := saveJSON(s.path, next); err != nil {
		respond(w, 500, map[string]string{"error": "could not persist clipboard"})
		return
	}
	s.db = next
	respond(w, 201, c)
}
