package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const offerMIME = "application/vnd.copier.offer+json"
const localOnlyMIME = "application/vnd.copier.local-only"
const chunkSize = int64(1 << 20)
const maxOfferBytes = int64(16 << 30)
const offerLifetime = 24 * time.Hour

type OfferEntry struct {
	Path string `json:"path"`
	Dir  bool   `json:"dir,omitempty"`
	Size int64  `json:"size"`
	Mode uint32 `json:"mode"`
	Name string `json:"name,omitempty"`
	Type string `json:"type,omitempty"`
	Bits int    `json:"bits,omitempty"`
}
type Offer struct {
	ID      string       `json:"id"`
	Device  string       `json:"device"`
	Kind    string       `json:"kind"`
	MIME    string       `json:"mime"`
	Roots   []string     `json:"roots,omitempty"`
	Entries []OfferEntry `json:"entries"`
	Expires int64        `json:"expires"`
}
type Fingerprint struct {
	Device uint64 `json:"dev"`
	Inode  uint64 `json:"ino"`
	Size   int64  `json:"size"`
	Mtime  int64  `json:"mtime"`
	Ctime  int64  `json:"ctime"`
}
type Binding struct {
	Root     string      `json:"root"`
	Relative string      `json:"relative"`
	Stat     Fingerprint `json:"stat"`
}
type OwnedOffer struct {
	Offer    Offer     `json:"offer"`
	Bindings []Binding `json:"bindings"`
}

func deviceID() (string, error) {
	path := filepath.Join(stateDir(), "device.json")
	var value struct {
		ID string `json:"id"`
	}
	for i := 0; i < 20; i++ {
		err := loadJSON(path, &value)
		if err == nil {
			if !idPattern.MatchString(value.ID) {
				return "", fmt.Errorf("invalid device identity")
			}
			return value.ID, nil
		}
		if !isNotExist(err) {
			return "", err
		}
		lock, err := lockFile(path + ".lock")
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if err = loadJSON(path, &value); isNotExist(err) {
			value.ID = newID()
			err = saveJSON(path, value)
		}
		lock.Close()
		if err != nil {
			return "", err
		}
		return value.ID, nil
	}
	return "", fmt.Errorf("device identity is busy")
}
func fingerprint(info os.FileInfo) Fingerprint {
	stat := info.Sys().(*syscall.Stat_t)
	return Fingerprint{uint64(stat.Dev), stat.Ino, info.Size(), info.ModTime().UnixNano(), stat.Ctim.Sec*1e9 + stat.Ctim.Nsec}
}
func offerPacket(o Offer) Packet { b, _ := json.Marshal(o); return Packet{MIME: offerMIME, Data: b} }
func packetOffer(p Packet) (Offer, error) {
	var o Offer
	if p.MIME != offerMIME || len(p.Data) > 1<<20 {
		return o, fmt.Errorf("not a clipboard offer")
	}
	dec := json.NewDecoder(strings.NewReader(string(p.Data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return o, err
	}
	if dec.Decode(new(any)) != io.EOF {
		return o, fmt.Errorf("trailing offer JSON")
	}
	if !idPattern.MatchString(o.ID) || !idPattern.MatchString(o.Device) || len(o.Entries) == 0 || len(o.Entries) > maxFileEntries || (o.Kind != "files" && o.Kind != "clipboard") || o.Expires <= 0 {
		return o, fmt.Errorf("invalid offer")
	}
	var total int64
	names := map[string]bool{}
	dirs := map[string]bool{}
	targets := map[string]bool{}
	for _, e := range o.Entries {
		if !validFileName(e.Path) || strings.HasSuffix(e.Path, "/") || names[e.Path] || e.Size < 0 || e.Size > maxOfferBytes-total || (e.Dir && e.Size != 0) || e.Mode&^uint32(0700) != 0 {
			return o, fmt.Errorf("invalid offer entry")
		}
		total += e.Size
		names[e.Path] = true
		if o.Kind == "files" {
			parent := path.Dir(e.Path)
			if parent != "." && !dirs[parent] {
				return o, fmt.Errorf("missing offer directory")
			}
			if e.Dir {
				dirs[e.Path] = true
			}
		}
		if o.Kind == "clipboard" {
			if e.Dir || e.Name == "" || len(e.Name) > 256 || e.Type == "" || len(e.Type) > 256 || strings.ContainsAny(e.Name+e.Type, "\x00\r\n") || (e.Bits != 8 && e.Bits != 16 && e.Bits != 32) || e.Size%int64(e.Bits/8) != 0 || targets[e.Name] {
				return o, fmt.Errorf("invalid offer representation")
			}
			targets[e.Name] = true
		}
	}
	if o.Kind == "clipboard" {
		if len(o.Entries) > 128 || !targets[o.MIME] {
			return o, fmt.Errorf("invalid default representation")
		}
	}
	if o.Kind == "files" {
		rootSet := map[string]bool{}
		for _, r := range o.Roots {
			if strings.Contains(r, "/") || !names[r] || rootSet[r] {
				return o, fmt.Errorf("invalid offer root")
			}
			rootSet[r] = true
		}
		if len(rootSet) == 0 {
			return o, fmt.Errorf("missing offer roots")
		}
		for _, e := range o.Entries {
			if !rootSet[strings.Split(e.Path, "/")[0]] {
				return o, fmt.Errorf("undeclared offer root")
			}
		}
	}
	return o, nil
}
func offerDir(id string) string { return filepath.Join(stateDir(), "offers", id) }
func ownedOffer(id string) (OwnedOffer, error) {
	var record OwnedOffer
	if !idPattern.MatchString(id) {
		return record, fmt.Errorf("invalid offer ID")
	}
	err := loadJSON(filepath.Join(offerDir(id), "offer.json"), &record)
	return record, err
}
func commitOwnedOffer(o Offer, bindings []Binding, signature string) (Packet, error) {
	index := filepath.Join(stateDir(), "offers", "index", signature+".json")
	var prior struct {
		ID string `json:"id"`
	}
	if err := loadJSON(index, &prior); err == nil {
		if record, err := ownedOffer(prior.ID); err == nil && record.Offer.Expires > time.Now().Unix() {
			return offerPacket(record.Offer), nil
		}
	}
	if err := saveJSON(filepath.Join(offerDir(o.ID), "offer.json"), OwnedOffer{o, bindings}); err != nil {
		return Packet{}, err
	}
	if err := saveJSON(index, struct {
		ID string `json:"id"`
	}{o.ID}); err != nil {
		return Packet{}, err
	}
	return offerPacket(o), nil
}

// The file walk reads directory entries and stat information, never file bytes.
func announceFiles(ctx context.Context, paths []string) (Packet, error) {
	device, err := deviceID()
	if err != nil {
		return Packet{}, err
	}
	o := Offer{ID: newID(), Device: device, Kind: "files", MIME: "text/uri-list", Expires: time.Now().Add(offerLifetime).Unix()}
	var bindings []Binding
	var total int64
	used := map[string]bool{}
	seen := map[string]bool{}
	var walk func(*os.Root, string, string, string, int) error
	walk = func(root *os.Root, relative, base, name string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth >= maxFileDepth || !validFileName(name) {
			return fmt.Errorf("invalid file name or directory depth")
		}
		info, err := root.Lstat(relative)
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("symlinks/special files cannot be offered: %q", name)
		}
		if len(o.Entries) >= maxFileEntries || (!info.IsDir() && info.Size() > maxOfferBytes-total) {
			return fmt.Errorf("file offer exceeds 4096 entries or 16 GiB")
		}
		size := info.Size()
		if info.IsDir() {
			size = 0
		} else {
			total += size
		}
		mode := uint32(0600 | info.Mode().Perm()&0100)
		if info.IsDir() {
			mode = 0700
		}
		o.Entries = append(o.Entries, OfferEntry{Path: name, Dir: info.IsDir(), Size: size, Mode: mode})
		bindings = append(bindings, Binding{Root: base, Relative: relative, Stat: fingerprint(info)})
		if info.IsDir() {
			f, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			defer f.Close()
			check, err := f.Stat()
			if err != nil {
				return err
			}
			if !os.SameFile(info, check) {
				return fmt.Errorf("source directory changed")
			}
			children, err := f.ReadDir(-1)
			if err != nil {
				return err
			}
			sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
			for _, child := range children {
				if err := walk(root, filepath.Join(relative, child.Name()), base, name+"/"+child.Name(), depth+1); err != nil {
					return err
				}
			}
			after, err := f.Stat()
			if err != nil {
				return err
			}
			if fingerprint(after) != fingerprint(info) {
				return fmt.Errorf("source directory changed during announcement")
			}
		}
		return nil
	}
	if len(paths) == 0 {
		return Packet{}, fmt.Errorf("copy --files requires a path")
	}
	for _, p := range paths {
		full, err := filepath.Abs(p)
		if err != nil {
			return Packet{}, err
		}
		full = filepath.Clean(full)
		if seen[full] {
			continue
		}
		seen[full] = true
		name := filepath.Base(full)
		if full == string(filepath.Separator) || !validFileName(name) {
			return Packet{}, fmt.Errorf("invalid file root")
		}
		original := name
		for n := 2; used[name]; n++ {
			ext := filepath.Ext(original)
			name = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(original, ext), n, ext)
		}
		used[name] = true
		o.Roots = append(o.Roots, name)
		base := filepath.Dir(full)
		root, err := os.OpenRoot(base)
		if err != nil {
			return Packet{}, err
		}
		err = walk(root, filepath.Base(full), base, name, 0)
		root.Close()
		if err != nil {
			return Packet{}, err
		}
	}
	sig, _ := json.Marshal(struct {
		Device   string
		Entries  []OfferEntry
		Bindings []Binding
		Roots    []string
	}{device, o.Entries, bindings, o.Roots})
	return commitOwnedOffer(o, bindings, digest(sig))
}

func announcePacket(ctx context.Context, p Packet) (Packet, error) {
	if err := ctx.Err(); err != nil {
		return Packet{}, err
	}
	if p.MIME == localOnlyMIME {
		return p, nil
	}
	if isFileBundle(p) {
		return p, nil
	} // Preserve existing ZIP history.
	if p.MIME == offerMIME {
		_, err := packetOffer(p)
		return p, err
	}
	if paths, err := clipboardFilePaths(p); err != nil {
		return Packet{}, err
	} else if len(paths) > 0 {
		return announceFiles(ctx, paths)
	}
	if !p.valid() {
		return Packet{}, fmt.Errorf("invalid clipboard payload")
	}
	device, err := deviceID()
	if err != nil {
		return Packet{}, err
	}
	original, _ := json.Marshal(p)
	signature := digest(append([]byte(device), original...))
	var prior struct {
		ID string `json:"id"`
	}
	if loadJSON(filepath.Join(stateDir(), "offers", "index", signature+".json"), &prior) == nil {
		if record, err := ownedOffer(prior.ID); err == nil && record.Offer.Expires > time.Now().Unix() {
			return offerPacket(record.Offer), nil
		}
	}
	projected := desktopProjection(p)
	o := Offer{ID: newID(), Device: device, Kind: "clipboard", MIME: projected.MIME, Expires: time.Now().Add(offerLifetime).Unix()}
	var bindings []Binding
	for i, name := range packetTypes(projected) {
		f := projected.Formats[name]
		filename := fmt.Sprintf("format-%d", i)
		base := offerDir(o.ID)
		if err = os.MkdirAll(base, 0700); err != nil {
			return Packet{}, err
		}
		full := filepath.Join(base, filename)
		if err = os.WriteFile(full, f.Data, 0600); err != nil {
			return Packet{}, err
		}
		file, err := os.Open(full)
		if err != nil {
			return Packet{}, err
		}
		err = file.Sync()
		info, statErr := file.Stat()
		file.Close()
		if err != nil {
			return Packet{}, err
		}
		if statErr != nil {
			return Packet{}, statErr
		}
		o.Entries = append(o.Entries, OfferEntry{Path: filename, Size: int64(len(f.Data)), Mode: 0600, Name: name, Type: f.Type, Bits: f.Bits})
		bindings = append(bindings, Binding{Root: base, Relative: filename, Stat: fingerprint(info)})
	}
	return commitOwnedOffer(o, bindings, signature)
}

func openOfferedFile(o Offer, index int) (*os.File, error) {
	id, err := deviceID()
	if err != nil {
		return nil, err
	}
	if id != o.Device {
		return nil, fmt.Errorf("offer belongs to another device")
	}
	record, err := ownedOffer(o.ID)
	if err != nil {
		return nil, fmt.Errorf("source offer is unavailable: %w", err)
	}
	if !equalPackets(offerPacket(o), offerPacket(record.Offer)) || len(record.Bindings) != len(o.Entries) || index < 0 || index >= len(o.Entries) || o.Entries[index].Dir {
		return nil, fmt.Errorf("source offer binding mismatch")
	}
	binding := record.Bindings[index]
	root, err := os.OpenRoot(binding.Root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.OpenFile(binding.Relative, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || fingerprint(info) != binding.Stat {
		f.Close()
		return nil, fmt.Errorf("source file changed since copy; copy it again")
	}
	return f, nil
}
func localOfferPaths(o Offer) ([]string, error) {
	record, err := ownedOffer(o.ID)
	if err != nil {
		return nil, err
	}
	if !equalPackets(offerPacket(o), offerPacket(record.Offer)) {
		return nil, fmt.Errorf("offer binding mismatch")
	}
	var result []string
	for _, name := range o.Roots {
		for i, e := range o.Entries {
			if e.Path == name {
				b := record.Bindings[i]
				result = append(result, filepath.Join(b.Root, b.Relative))
				break
			}
		}
	}
	return result, nil
}
