package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type ChunkJob struct {
	Board string `json:"board"`
	Offer Offer  `json:"offer"`
	Index int    `json:"index"`
	Chunk int64  `json:"chunk"`
	Error string `json:"error,omitempty"`
}
type ChunkReceipt struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

func (s *Relay) chunkRoot() string                   { return s.path + ".chunks" }
func (s *Relay) transferDir(board, id string) string { return filepath.Join(s.chunkRoot(), board, id) }
func chunkBase(dir string, index int, chunk int64) string {
	return filepath.Join(dir, fmt.Sprintf("%d-%d", index, chunk))
}

func chunkLength(o Offer, index int, chunk int64) (int64, error) {
	if index < 0 || index >= len(o.Entries) || o.Entries[index].Dir || chunk < 0 {
		return 0, fmt.Errorf("invalid chunk")
	}
	size := o.Entries[index].Size
	if size == 0 && chunk == 0 {
		return 0, nil
	}
	if chunk >= ((size + chunkSize - 1) / chunkSize) {
		return 0, fmt.Errorf("chunk outside file")
	}
	remain := size - chunk*chunkSize
	if remain > chunkSize {
		remain = chunkSize
	}
	return remain, nil
}
func (s *Relay) offerForTransfer(board, id string) (Offer, error) {
	var o Offer
	lease := filepath.Join(s.transferDir(board, id), "offer.json")
	if err := loadJSON(lease, &o); err == nil {
		return o, nil
	} else if !isNotExist(err) {
		return o, err
	}
	for _, clip := range s.db.Boards[board].Clips {
		if clip.MIME == offerMIME {
			candidate, err := packetOffer(clip.packet())
			if err == nil && candidate.ID == id {
				if err = saveJSON(lease, candidate); err != nil {
					return o, err
				}
				return candidate, nil
			}
		}
	}
	return o, fmt.Errorf("offer is no longer retained")
}

// Called after bearer authentication. Data only enters this cache in response
// to a remote device's read request, never in response to clipboard metadata.
func (s *Relay) handleChunks(w http.ResponseWriter, r *http.Request, parts []string) bool {
	if len(parts) < 4 || parts[0] != "v1" || parts[1] != "boards" || !namePattern.MatchString(parts[2]) {
		return false
	}
	if parts[3] == "jobs" && len(parts) == 4 {
		device := r.URL.Query().Get("device")
		if r.Method != "GET" || !idPattern.MatchString(device) {
			respond(w, 400, map[string]string{"error": "invalid job poll"})
			return true
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if time.Since(s.lastChunkCleanup) > time.Minute {
			if err := s.cleanChunksLocked(); err != nil {
				respond(w, 500, map[string]string{"error": "chunk cleanup failed"})
				return true
			}
		}
		loadJobs := func() []ChunkJob {
			var jobs []ChunkJob
			dir := filepath.Join(s.chunkRoot(), parts[2])
			_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(path, ".request.json") {
					return nil
				}
				if len(jobs) >= 128 {
					return nil
				}
				var job ChunkJob
				if loadJSON(path, &job) != nil || job.Offer.Device != device || job.Error != "" || job.Offer.Expires < time.Now().Unix() {
					return nil
				}
				base := strings.TrimSuffix(path, ".request.json")
				var receipt ChunkReceipt
				if loadJSON(base+".receipt.json", &receipt) == nil {
					return nil
				}
				jobs = append(jobs, job)
				return nil
			})
			return jobs
		}
		jobs := loadJobs()
		if len(jobs) == 0 && r.URL.Query().Get("wait") == "1" {
			signal := s.jobsChanged
			s.mu.Unlock()
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			case <-signal:
			}
			s.mu.Lock()
			jobs = loadJobs()
		}
		respond(w, 200, jobs)
		return true
	}
	if parts[3] != "chunks" {
		return false
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(2 * time.Minute))
	_ = controller.SetWriteDeadline(time.Now().Add(2 * time.Minute))
	if len(parts) != 7 || !idPattern.MatchString(parts[4]) {
		respond(w, 400, map[string]string{"error": "invalid chunk address"})
		return true
	}
	board, id := parts[2], parts[4]
	index, e1 := strconv.Atoi(parts[5])
	chunk, e2 := strconv.ParseInt(parts[6], 10, 64)
	if e1 != nil || e2 != nil {
		respond(w, 400, map[string]string{"error": "invalid chunk index"})
		return true
	}
	device := r.URL.Query().Get("device")
	if !idPattern.MatchString(device) {
		respond(w, 400, map[string]string{"error": "invalid device ID"})
		return true
	}
	var upload []byte
	var err error
	if r.Method == "PUT" {
		upload, err = io.ReadAll(http.MaxBytesReader(w, r.Body, chunkSize+1))
		if err != nil || int64(len(upload)) > chunkSize {
			respond(w, 400, map[string]string{"error": "chunk exceeds 1 MiB"})
			return true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.offerForTransfer(board, id)
	if err != nil {
		respond(w, 404, map[string]string{"error": err.Error()})
		return true
	}
	if o.Expires < time.Now().Unix() {
		respond(w, 410, map[string]string{"error": "offer expired; copy it again"})
		return true
	}
	length, err := chunkLength(o, index, chunk)
	if err != nil {
		respond(w, 400, map[string]string{"error": err.Error()})
		return true
	}
	base := chunkBase(s.transferDir(board, id), index, chunk)
	var job ChunkJob
	jobErr := loadJSON(base+".request.json", &job)
	switch r.Method {
	case "GET", "HEAD":
		if device == o.Device {
			respond(w, 400, map[string]string{"error": "local reads must use original local files"})
			return true
		}
		if jobErr != nil && !isNotExist(jobErr) {
			respond(w, 500, map[string]string{"error": "unreadable transfer job"})
			return true
		}
		if job.Error != "" {
			respond(w, 410, map[string]string{"error": job.Error})
			return true
		}
		var receipt ChunkReceipt
		if loadJSON(base+".receipt.json", &receipt) == nil {
			data, err := os.ReadFile(base + ".bin")
			if err != nil || int64(len(data)) != length || receipt.Size != length || digest(data) != receipt.Hash {
				respond(w, 500, map[string]string{"error": "cached chunk integrity failure"})
				return true
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-Chunk-SHA256", receipt.Hash)
			w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
			s.mu.Unlock()
			w.WriteHeader(200)
			_, _ = w.Write(data)
			s.mu.Lock()
			return true
		}
		if isNotExist(jobErr) {
			if s.chunkBytes+length > s.chunkQuota {
				respond(w, 507, map[string]string{"error": "relay chunk cache quota exceeded"})
				return true
			}
			job = ChunkJob{Board: board, Offer: Offer{ID: o.ID, Device: o.Device, Expires: o.Expires}, Index: index, Chunk: chunk}
			if err = saveJSON(base+".request.json", job); err != nil {
				respond(w, 500, map[string]string{"error": "could not persist chunk request"})
				return true
			}
			close(s.jobsChanged)
			s.jobsChanged = make(chan struct{})
		}
		respond(w, 202, map[string]string{"status": "waiting for source device"})
		return true
	case "PUT", "POST":
		if device != o.Device {
			respond(w, 403, map[string]string{"error": "no matching source request"})
			return true
		}
		if r.Method == "POST" {
			var receipt ChunkReceipt
			if loadJSON(base+".receipt.json", &receipt) == nil {
				respond(w, 200, map[string]string{"status": "already ready"})
				return true
			}
			if jobErr != nil {
				respond(w, 403, map[string]string{"error": "no matching source request"})
				return true
			}
			job.Error = "source file changed or became unavailable; copy it again"
			if err = saveJSON(base+".request.json", job); err != nil {
				respond(w, 500, map[string]string{"error": "could not persist source error"})
				return true
			}
			respond(w, 200, map[string]string{"status": "failed"})
			return true
		}
		if job.Error != "" {
			respond(w, 410, map[string]string{"error": job.Error})
			return true
		}
		hash := digest(upload)
		if int64(len(upload)) != length || r.Header.Get("X-Chunk-SHA256") != hash {
			respond(w, 400, map[string]string{"error": "chunk length/checksum mismatch"})
			return true
		}
		var receipt ChunkReceipt
		if loadJSON(base+".receipt.json", &receipt) == nil {
			if receipt.Hash != hash || receipt.Size != length {
				respond(w, 422, map[string]string{"error": "chunk was already committed with other data"})
				return true
			}
			respond(w, 200, receipt)
			return true
		}
		var existing int64
		if info, err := os.Stat(base + ".bin"); err == nil {
			existing = info.Size()
		}
		if jobErr != nil {
			respond(w, 403, map[string]string{"error": "no matching source request"})
			return true
		}
		if s.chunkBytes+length-existing > s.chunkQuota {
			respond(w, 507, map[string]string{"error": "relay chunk cache quota exceeded"})
			return true
		}
		if err = saveBytes(base+".bin", upload); err != nil {
			respond(w, 500, map[string]string{"error": "could not persist chunk"})
			return true
		}
		s.chunkBytes += length - existing
		receipt = ChunkReceipt{Hash: hash, Size: length}
		if err = saveJSON(base+".receipt.json", receipt); err != nil {
			respond(w, 500, map[string]string{"error": "could not persist checksum receipt"})
			return true
		}
		_ = os.Remove(base + ".request.json")
		respond(w, 201, receipt)
		return true
	default:
		respond(w, 405, map[string]string{"error": "method not allowed"})
		return true
	}
}

func saveBytes(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".chunk-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Relay) cleanChunksLocked() error {
	s.lastChunkCleanup = time.Now()
	s.chunkBytes = 0
	boards, err := os.ReadDir(s.chunkRoot())
	if isNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, board := range boards {
		if !board.IsDir() {
			continue
		}
		offers, err := os.ReadDir(filepath.Join(s.chunkRoot(), board.Name()))
		if err != nil {
			return err
		}
		for _, entry := range offers {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(s.chunkRoot(), board.Name(), entry.Name())
			var offer Offer
			if loadJSON(filepath.Join(dir, "offer.json"), &offer) == nil && offer.Expires < time.Now().Unix() {
				if err = os.RemoveAll(dir); err != nil {
					return err
				}
				continue
			}
			if err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() && strings.HasSuffix(path, ".bin") {
					info, err := d.Info()
					if err != nil {
						return err
					}
					s.chunkBytes += info.Size()
				}
				return nil
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func chunkURL(id string, index int, chunk int64, device string) string {
	return fmt.Sprintf("/chunks/%s/%d/%d?device=%s", id, index, chunk, device)
}
