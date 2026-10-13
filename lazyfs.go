package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type OfferNode struct {
	fs.Inode
	client *Client
	offer  Offer
	index  int
}

func (n *OfferNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Uid = uint32(os.Getuid())
	out.Gid = uint32(os.Getgid())
	if n.index < 0 {
		out.Mode = syscall.S_IFDIR | 0700
		return 0
	}
	e := n.offer.Entries[n.index]
	out.Mode = e.Mode
	if e.Dir {
		out.Mode |= syscall.S_IFDIR
	} else {
		out.Mode |= syscall.S_IFREG
		out.Size = uint64(e.Size)
		out.Blocks = (uint64(e.Size) + 511) / 512
	}
	return 0
}
func (n *OfferNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY {
		return nil, 0, syscall.EROFS
	}
	return n, fuse.FOPEN_KEEP_CACHE, 0
}
func (n *OfferNode) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if off < 0 || n.index < 0 {
		return nil, syscall.EINVAL
	}
	e := n.offer.Entries[n.index]
	if off >= e.Size {
		return fuse.ReadResultData(nil), 0
	}
	need := int64(len(dest))
	if need > e.Size-off {
		need = e.Size - off
	}
	nread := int64(0)
	for nread < need {
		chunk := (off + nread) / chunkSize
		data, err := n.client.readChunk(ctx, n.offer, n.index, chunk)
		if err != nil {
			log.Printf("remote clipboard read failed: %v", err)
			return nil, syscall.EIO
		}
		offset := (off + nread) % chunkSize
		take := int64(len(data)) - offset
		if take > need-nread {
			take = need - nread
		}
		if take <= 0 {
			return nil, syscall.EIO
		}
		copy(dest[nread:nread+take], data[offset:offset+take])
		nread += take
	}
	return fuse.ReadResultData(dest[:nread]), 0
}
func (n *OfferNode) OnAdd(ctx context.Context) {
	if n.index != -1 {
		return
	}
	nodes := map[string]*fs.Inode{".": &n.Inode}
	for i, e := range n.offer.Entries {
		mode := uint32(syscall.S_IFREG)
		if e.Dir {
			mode = syscall.S_IFDIR
		}
		child := n.NewPersistentInode(ctx, &OfferNode{client: n.client, offer: n.offer, index: i}, fs.StableAttr{Mode: mode, Ino: uint64(i + 2)})
		nodes[path.Dir(e.Path)].AddChild(path.Base(e.Path), child, false)
		nodes[e.Path] = child
	}
}

type MountSpec struct {
	Offer     Offer  `json:"offer"`
	Path      string `json:"path"`
	Config    Config `json:"config"`
	AllowHTTP bool   `json:"allow_http"`
}

func mountOffer(client *Client, offer Offer, mountpoint string) (*fuse.Server, error) {
	if _, err := packetOffer(offerPacket(offer)); err != nil {
		return nil, err
	}
	return fs.Mount(mountpoint, &OfferNode{client: client, offer: offer, index: -1}, &fs.Options{MountOptions: fuse.MountOptions{FsName: "copier", Name: "copier", Options: []string{"ro"}, DisableXAttrs: true}, UID: uint32(os.Getuid()), GID: uint32(os.Getgid())})
}
func runMount(ctx context.Context, specPath string) error {
	lock, err := lockFile(specPath + ".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	pidPath := specPath + ".pid.json"
	if err = saveJSON(pidPath, struct {
		PID int `json:"pid"`
	}{os.Getpid()}); err != nil {
		return err
	}
	defer os.Remove(pidPath)
	var spec MountSpec
	if err = loadJSON(specPath, &spec); err != nil {
		return err
	}
	client, err := newClient(spec.Config, spec.AllowHTTP)
	if err != nil {
		return err
	}
	server, err := mountOffer(client, spec.Offer, spec.Path)
	if err != nil {
		return err
	}
	done := make(chan struct{})
	go func() { server.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		return server.Unmount()
	case <-done:
		return nil
	}
}
func isFuseMount(path string) bool {
	var stat syscall.Statfs_t
	return syscall.Statfs(path, &stat) == nil && stat.Type == 0x65735546
}
func (c *Client) lazyOfferPaths(ctx context.Context, o Offer, base string) ([]string, error) {
	device, err := deviceID()
	if err != nil {
		return nil, err
	}
	if device == o.Device {
		return localOfferPaths(o)
	}
	if c.http == nil {
		return nil, fmt.Errorf("configure the relay endpoint for a remote file offer")
	}
	if base == "" {
		base = receivedDir()
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return nil, err
	}
	namespace := digest([]byte(c.config.URL + "\n" + c.config.Board))[:20]
	target := filepath.Join(base, "lazy-"+namespace+"-"+o.ID)
	specPath := filepath.Join(stateDir(), "mounts", namespace+"-"+o.ID+"-"+digest([]byte(target))[:12]+".json")
	if !isFuseMount(target) {
		if err = os.MkdirAll(target, 0700); err != nil {
			return nil, err
		}
		items, err := os.ReadDir(target)
		if err != nil {
			return nil, fmt.Errorf("stale mount at %s; unmount it before retrying: %w", target, err)
		}
		if len(items) > 0 {
			return nil, fmt.Errorf("mount directory is not empty")
		}
		if err = saveJSON(specPath, MountSpec{o, target, c.config, c.allowHTTP}); err != nil {
			return nil, err
		}
		executable, err := os.Executable()
		if err != nil {
			return nil, err
		}
		file, err := os.OpenFile(specPath+".log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		cmd := exec.Command(executable, "mount-offer", "--spec", specPath)
		cmd.Env = c.childEnv()
		cmd.Stdout = file
		cmd.Stderr = file
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err = cmd.Start(); err != nil {
			return nil, err
		}
		_ = cmd.Process.Release()
		for i := 0; i < 100; i++ {
			if isFuseMount(target) {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
		if !isFuseMount(target) {
			return nil, fmt.Errorf("lazy file mount failed (requires /dev/fuse and fusermount3); see %s.log; use paste --files NEW_DIRECTORY to explicitly download", specPath)
		}
	}
	paths := make([]string, len(o.Roots))
	for i, name := range o.Roots {
		paths[i] = filepath.Join(target, name)
	}
	return paths, nil
}
