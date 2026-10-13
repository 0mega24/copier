package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"
)

const fileBundleMIME = "application/vnd.copier.files+zip"
const fileBundleComment = "copier-files-v1"
const maxFileEntries = 4096
const maxFileDepth = 64

func isFileBundle(p Packet) bool { return p.MIME == fileBundleMIME }

// Only native file targets trigger filesystem reads. Plain text that happens
// to contain a filename or URL never grants permission to read that file.
func clipboardFilePaths(p Packet) ([]string, error) {
	var raw []byte
	found := false
	gnome := false
	explicitFiles := false
	for _, name := range []string{"x-special/gnome-copied-files", "x-special/nautilus-clipboard", "text/uri-list", "application/x-kde4-urilist"} {
		if f, ok := p.Formats[name]; ok {
			raw = f.Data
			found = true
			gnome = strings.HasPrefix(name, "x-special/")
			explicitFiles = name != "text/uri-list"
			break
		}
		if p.MIME == name {
			raw = p.Data
			found = true
			gnome = strings.HasPrefix(name, "x-special/")
			explicitFiles = name != "text/uri-list"
			break
		}
	}
	if !found {
		return nil, nil
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("file clipboard is not UTF-8")
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	if gnome {
		if len(lines) > 0 && lines[0] == "x-special/nautilus-clipboard" {
			lines = lines[1:]
		}
		if len(lines) == 0 || (lines[0] != "copy" && lines[0] != "cut") {
			return nil, fmt.Errorf("invalid file-manager copy/cut header")
		}
		lines = lines[1:]
	}
	if !explicitFiles {
		// Browsers can advertise URI lists for copied hyperlinks, too. Preserve
		// such clipboards normally; only lists containing file: URLs are files.
		hasFile := false
		for _, line := range lines {
			if u, err := url.Parse(line); err == nil && u.Scheme == "file" {
				hasFile = true
				break
			}
		}
		if !hasFile {
			return nil, nil
		}
	}
	var paths []string
	seen := map[string]bool{}
	for _, line := range lines {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || u == nil || u.Scheme != "file" || (u.Host != "" && !strings.EqualFold(u.Host, "localhost")) || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || !filepath.IsAbs(u.Path) || strings.ContainsRune(u.Path, 0) {
			return nil, fmt.Errorf("file clipboard contains an unsupported non-local URI")
		}
		value := filepath.Clean(u.Path)
		if !seen[value] {
			paths = append(paths, value)
			seen[value] = true
		}
		if len(paths) > maxFileEntries {
			return nil, fmt.Errorf("too many copied files")
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("file clipboard has no local paths")
	}
	return paths, nil
}

func transportClipboardFiles(ctx context.Context, p Packet) (Packet, error) {
	if p.MIME == offerMIME {
		_, err := packetOffer(p)
		return p, err
	}
	if isFileBundle(p) {
		return p, nil
	}
	paths, err := clipboardFilePaths(p)
	if err != nil {
		return Packet{}, err
	}
	if len(paths) == 0 {
		return p, nil
	}
	return packFiles(ctx, paths) // Legacy ZIP conversion; new sharing uses announcePacket.
}

func validFileName(name string) bool {
	if name == "" || !utf8.ValidString(name) || strings.ContainsAny(name, "\\\x00\r\n") || strings.HasPrefix(name, "/") {
		return false
	}
	clean := strings.TrimSuffix(name, "/")
	if clean == "" || path.Clean(clean) != clean || strings.Count(clean, "/") >= maxFileDepth {
		return false
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "." || part == ".." || part == "" || len(part) > 255 {
			return false
		}
	}
	return true
}

// Store-only ZIP with fixed metadata makes file snapshots deterministic. It is
// also bounded without relying on attacker-supplied compression ratios.
func packFiles(ctx context.Context, paths []string) (Packet, error) {
	if len(paths) == 0 {
		return Packet{}, fmt.Errorf("copy --files requires at least one path")
	}
	out := &boundedBuffer{max: maxClip}
	writer := zip.NewWriter(out)
	if err := writer.SetComment(fileBundleComment); err != nil {
		return Packet{}, err
	}
	entries, total := 0, int64(0)
	used := map[string]bool{}
	seen := map[string]bool{}
	var walk func(*os.Root, string, string, int) error
	walk = func(root *os.Root, rel, name string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth >= maxFileDepth || !validFileName(name) {
			return fmt.Errorf("unsupported filename or directory depth: %q", name)
		}
		before, err := root.Lstat(rel)
		if err != nil {
			return err
		}
		if !before.IsDir() && !before.Mode().IsRegular() {
			return fmt.Errorf("symlinks and special files cannot be copied: %q", name)
		}
		f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		opened, err := f.Stat()
		if err != nil {
			return err
		}
		if !os.SameFile(before, opened) {
			return fmt.Errorf("source changed while opening: %q", name)
		}
		entries++
		if entries > maxFileEntries {
			return fmt.Errorf("file bundle exceeds %d entries", maxFileEntries)
		}
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		if before.IsDir() {
			header.Name += "/"
			header.SetMode(os.ModeDir | 0700)
		} else {
			header.SetMode(0600 | before.Mode().Perm()&0100)
		}
		dst, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		if before.IsDir() {
			child, err := root.OpenRoot(rel)
			if err != nil {
				return err
			}
			defer child.Close()
			check, err := child.Stat(".")
			if err != nil {
				return err
			}
			if !os.SameFile(opened, check) {
				return fmt.Errorf("source directory changed: %q", name)
			}
			children, err := f.ReadDir(-1)
			if err != nil {
				return err
			}
			sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
			for _, entry := range children {
				if err := walk(child, entry.Name(), name+"/"+entry.Name(), depth+1); err != nil {
					return err
				}
			}
		} else {
			if opened.Size() > int64(maxClip)-total {
				return fmt.Errorf("file bundle exceeds 32 MiB")
			}
			n, err := io.Copy(dst, io.LimitReader(f, int64(maxClip)-total+1))
			if err != nil {
				return err
			}
			total += n
			if total > maxClip || n != opened.Size() {
				return fmt.Errorf("source changed or file bundle exceeds 32 MiB: %q", name)
			}
		}
		after, err := f.Stat()
		if err != nil {
			return err
		}
		final, err := root.Lstat(rel)
		if err != nil {
			return err
		}
		if !os.SameFile(opened, final) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) || opened.Mode() != after.Mode() {
			return fmt.Errorf("source changed during capture: %q", name)
		}
		return nil
	}
	for _, source := range paths {
		full, err := filepath.Abs(source)
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
			return Packet{}, fmt.Errorf("unsupported source root: %q", source)
		}
		original := name
		for n := 2; used[name]; n++ {
			ext := filepath.Ext(original)
			name = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(original, ext), n, ext)
		}
		used[name] = true
		root, err := os.OpenRoot(filepath.Dir(full))
		if err != nil {
			return Packet{}, err
		}
		err = walk(root, filepath.Base(full), name, 0)
		root.Close()
		if err != nil {
			return Packet{}, err
		}
	}
	if err := writer.Close(); err != nil {
		return Packet{}, err
	}
	return Packet{MIME: fileBundleMIME, Data: append([]byte(nil), out.Bytes()...)}, nil
}

type fileArchive struct {
	zip   *zip.Reader
	roots []string
}

func inspectFileArchive(data []byte) (fileArchive, error) {
	if len(data) > maxClip {
		return fileArchive{}, fmt.Errorf("file bundle exceeds 32 MiB")
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fileArchive{}, err
	}
	if zr.Comment != fileBundleComment || len(zr.File) == 0 || len(zr.File) > maxFileEntries {
		return fileArchive{}, fmt.Errorf("invalid Copier file bundle")
	}
	a := fileArchive{zip: zr}
	known := map[string]bool{}
	dirs := map[string]bool{}
	var total uint64
	for _, f := range zr.File {
		name := strings.TrimSuffix(f.Name, "/")
		directory := strings.HasSuffix(f.Name, "/")
		if !validFileName(f.Name) || known[name] || f.Method != zip.Store || f.Flags&1 != 0 || (!f.Mode().IsRegular() && !f.Mode().IsDir()) || f.Mode().IsDir() != directory {
			return fileArchive{}, fmt.Errorf("unsafe or duplicate archive entry: %q", f.Name)
		}
		if f.UncompressedSize64 != f.CompressedSize64 || f.UncompressedSize64 > uint64(maxClip)-total || (directory && f.UncompressedSize64 != 0) {
			return fileArchive{}, fmt.Errorf("invalid or oversized archive entry")
		}
		total += f.UncompressedSize64
		parent := path.Dir(name)
		if parent != "." && !dirs[parent] {
			return fileArchive{}, fmt.Errorf("archive parent missing or not a directory: %q", f.Name)
		}
		if parent == "." {
			a.roots = append(a.roots, name)
		}
		known[name] = true
		if directory {
			dirs[name] = true
		}
	}
	return a, nil
}

// Extract into a new private sibling and publish the complete tree with one
// no-replace rename. Failed/partial transfers never become a clipboard path.
func extractFiles(ctx context.Context, p Packet, destination string) ([]string, error) {
	if !isFileBundle(p) {
		return nil, fmt.Errorf("clipboard does not contain copied files; use --file to copy raw file bytes or --files to copy file objects")
	}
	archive, err := inspectFileArchive(p.Data)
	if err != nil {
		return nil, err
	}
	full, err := filepath.Abs(destination)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(full); !os.IsNotExist(err) {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("destination already exists: %s", full)
	}
	if err = os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		return nil, err
	}
	temp, err := os.MkdirTemp(filepath.Dir(full), ".copier-files-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temp)
	root, err := os.OpenRoot(temp)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	for _, entry := range archive.zip.File {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(entry.Name, "/")
		if entry.FileInfo().IsDir() {
			if err = root.Mkdir(name, 0700); err != nil {
				return nil, err
			}
			continue
		}
		src, err := entry.Open()
		if err != nil {
			return nil, err
		}
		target, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600|entry.Mode().Perm()&0100)
		if err != nil {
			src.Close()
			return nil, err
		}
		n, copyErr := io.Copy(target, io.LimitReader(src, int64(entry.UncompressedSize64)+1))
		src.Close()
		if copyErr == nil && n != int64(entry.UncompressedSize64) {
			copyErr = fmt.Errorf("archive size mismatch: %q", name)
		}
		if copyErr == nil {
			copyErr = target.Sync()
		}
		closeErr := target.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	// Flush nested directories before publishing, then the parent after rename.
	for i := len(archive.zip.File) - 1; i >= 0; i-- {
		entry := archive.zip.File[i]
		if entry.FileInfo().IsDir() {
			d, err := root.Open(strings.TrimSuffix(entry.Name, "/"))
			if err != nil {
				return nil, err
			}
			err = d.Sync()
			d.Close()
			if err != nil {
				return nil, err
			}
		}
	}
	d, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	err = d.Sync()
	d.Close()
	if err != nil {
		return nil, err
	}
	if err = renameNoReplace(temp, full); err != nil {
		return nil, err
	}
	parent, err := os.Open(filepath.Dir(full))
	if err != nil {
		return nil, err
	}
	err = parent.Sync()
	parent.Close()
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(archive.roots))
	for i, name := range archive.roots {
		paths[i] = filepath.Join(full, name)
	}
	return paths, nil
}

func nativeFilePacket(paths []string) Packet {
	var uris []string
	for _, name := range paths {
		uris = append(uris, (&url.URL{Scheme: "file", Path: name}).String())
	}
	list := []byte(strings.Join(uris, "\r\n") + "\r\n")
	gnome := []byte("copy\n" + strings.Join(uris, "\n"))
	formats := map[string]Format{
		"text/uri-list":                  {Data: list, Type: "text/uri-list", Bits: 8},
		"x-special/gnome-copied-files":   {Data: gnome, Type: "x-special/gnome-copied-files", Bits: 8},
		"application/x-kde4-urilist":     {Data: list, Type: "application/x-kde4-urilist", Bits: 8},
		"application/x-kde-cutselection": {Data: []byte("0"), Type: "application/x-kde-cutselection", Bits: 8},
	}
	return Packet{MIME: "text/uri-list", Data: list, Formats: formats}
}

func receivedDir() string { return filepath.Join(stateDir(), "received") }

func prepareDesktopFiles(ctx context.Context, p Packet) (Packet, error) {
	return prepareDesktopFilesIn(ctx, p, "")
}

func prepareDesktopFilesIn(ctx context.Context, p Packet, base string) (Packet, error) {
	if !isFileBundle(p) {
		return p, nil
	}
	archive, err := inspectFileArchive(p.Data)
	if err != nil {
		return Packet{}, err
	}
	if base == "" {
		base = receivedDir()
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return Packet{}, err
	}
	destination := filepath.Join(base, digest(p.Data))
	if err = os.MkdirAll(base, 0700); err != nil {
		return Packet{}, err
	}
	lock, err := lockFile(destination + ".lock")
	if err != nil {
		return Packet{}, err
	}
	defer lock.Close()
	paths := make([]string, len(archive.roots))
	for i, name := range archive.roots {
		paths[i] = filepath.Join(destination, name)
	}
	if _, err = os.Lstat(destination); err == nil {
		snapshot, checkErr := packFiles(ctx, paths)
		if checkErr == nil && equalBytes(snapshot.Data, p.Data) {
			return nativeFilePacket(paths), nil
		}
		// A receiver may have edited these files. Preserve the edits, and publish
		// this immutable transfer in a fresh directory instead of overwriting them.
		destination += "-" + newID()
	} else if !os.IsNotExist(err) {
		return Packet{}, err
	}
	paths, err = extractFiles(ctx, p, destination)
	if err != nil {
		return Packet{}, err
	}
	return nativeFilePacket(paths), nil
}
