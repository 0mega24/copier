package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

func writeJSON(w io.Writer, v any) error { return json.NewEncoder(w).Encode(v) }

func interactive(ctx context.Context, c *Client, backend, selection, fileDir string, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), maxClip)
	fmt.Fprintf(out, "Copier • %s • board %s\n", c.config.URL, c.config.Board)
	fmt.Fprintln(out, "[c] copy text  [f] copy file bytes  [a] copy files/folders  [e] extract files")
	fmt.Fprintln(out, "[p] preview latest  [h] history")
	fmt.Fprintln(out, "[d] copy desktop  [l] paste to desktop  [r] restore revision  [q] quit")
	for {
		fmt.Fprint(out, "copier> ")
		if !scanner.Scan() {
			return scanner.Err()
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "q" || line == "quit" {
			return nil
		}
		if line == "" {
			continue
		}
		var err error
		switch line {
		case "c":
			fmt.Fprintln(out, "Enter text; a line containing only . finishes (newline preserved):")
			var b strings.Builder
			for scanner.Scan() {
				if scanner.Text() == "." {
					break
				}
				if b.Len()+len(scanner.Text())+1 > maxClip {
					err = fmt.Errorf("content exceeds 32 MiB")
					break
				}
				b.WriteString(scanner.Text())
				b.WriteByte('\n')
			}
			if err == nil {
				_, err = c.copy(ctx, []byte(b.String()), "text/plain;charset=utf-8", nil)
			}
		case "f":
			fmt.Fprint(out, "Path: ")
			if !scanner.Scan() {
				return scanner.Err()
			}
			err = copyFile(ctx, c, scanner.Text())
		case "a":
			fmt.Fprintln(out, "Enter one file/folder path per line; . finishes:")
			var paths []string
			for scanner.Scan() {
				if scanner.Text() == "." {
					break
				}
				paths = append(paths, scanner.Text())
				if len(paths) > maxFileEntries {
					err = fmt.Errorf("too many copied paths")
					break
				}
			}
			if err == nil {
				var packet Packet
				packet, err = announceFiles(ctx, paths)
				if err == nil {
					_, err = c.copyPacket(ctx, packet, nil)
				}
			}
		case "e":
			fmt.Fprint(out, "New destination directory: ")
			if !scanner.Scan() {
				return scanner.Err()
			}
			var clip Clip
			clip, err = c.get(ctx, 0)
			if err == nil {
				var paths []string
				if clip.MIME == offerMIME {
					var offer Offer
					offer, err = packetOffer(clip.packet())
					if err == nil {
						paths, err = c.extractOffer(ctx, offer, scanner.Text())
					}
				} else {
					paths, err = extractFiles(ctx, clip.packet(), scanner.Text())
				}
				if err == nil {
					for _, name := range paths {
						fmt.Fprintln(out, name)
					}
				}
			}
		case "p":
			var clip Clip
			clip, err = c.get(ctx, 0)
			if err == nil {
				if clip.Revision == 0 {
					fmt.Fprintln(out, "Empty clipboard")
				} else {
					p := clip.packet()
					display := previewPacket(p)
					if p.MIME == offerMIME {
						offer, e := packetOffer(p)
						if e != nil {
							err = e
						} else if offer.Kind == "clipboard" {
							for i, entry := range offer.Entries {
								if entry.Name == offer.MIME {
									var data []byte
									data, err = c.readChunk(ctx, offer, i, 0)
									if err == nil {
										display = preview(data)
									}
									break
								}
							}
						}
					}
					if err == nil {
						fmt.Fprintf(out, "Revision %d • %s\n%s\n", clip.Revision, clip.MIME, display)
					}
				}
			}
		case "h":
			var clips []Clip
			clips, err = c.history(ctx)
			if err == nil {
				for i := len(clips) - 1; i >= 0; i-- {
					clip := clips[i]
					fmt.Fprintf(out, "%d • %q • %d bytes • %s\n", clip.Revision, clip.Origin, len(clip.Data), previewPacket(clip.packet()))
				}
			}
		case "d", "l":
			cb, e := nativeClipboard(backend, selection)
			if e == nil {
				cb.FileDir = fileDir
				cb.Client = c
			}
			err = e
			if err == nil {
				if line == "d" {
					var packet Packet
					packet, err = cb.ReadPacket(ctx)
					if err == nil {
						_, err = c.copyPacket(ctx, packet, nil)
					}
				} else {
					var clip Clip
					clip, err = c.get(ctx, 0)
					if err == nil {
						if clip.Revision == 0 {
							err = fmt.Errorf("clipboard is empty")
						} else {
							err = cb.WritePacket(ctx, clip.packet())
						}
					}
				}
			}
		case "r":
			fmt.Fprint(out, "Revision to restore as a new copy: ")
			if !scanner.Scan() {
				return scanner.Err()
			}
			var rev uint64
			rev, err = strconv.ParseUint(scanner.Text(), 10, 64)
			if err == nil && rev == 0 {
				err = fmt.Errorf("revision must be positive")
			}
			if err == nil {
				var clip Clip
				clip, err = c.get(ctx, rev)
				if err == nil {
					_, err = c.copyPacket(ctx, clip.packet(), nil)
				}
			}
		default:
			fmt.Fprintln(out, "Choose c, f, a, e, p, h, d, l, r or q")
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			fmt.Fprintln(out, "Error:", err)
		} else if strings.Contains("cfaedlr", line) {
			fmt.Fprintln(out, "Done")
		}
	}
}
func preview(b []byte) string {
	if len(b) > 160 {
		return strconv.Quote(string(b[:160])) + "…"
	}
	return strconv.Quote(string(b))
}

func previewPacket(p Packet) string {
	if p.MIME == offerMIME {
		offer, err := packetOffer(p)
		if err != nil {
			return "Invalid offer"
		}
		if offer.Kind == "files" {
			var names []string
			for i, name := range offer.Roots {
				if i == 10 {
					names = append(names, "…")
					break
				}
				names = append(names, strconv.Quote(name))
			}
			return "Files: " + strings.Join(names, ", ")
		}
		return "Clipboard offer: " + offer.MIME
	}
	if isFileBundle(p) {
		a, err := inspectFileArchive(p.Data)
		if err != nil {
			return "Invalid file bundle"
		}
		var names []string
		for i, name := range a.roots {
			if i == 10 {
				names = append(names, "…")
				break
			}
			names = append(names, strconv.Quote(name))
		}
		return "Files: " + strings.Join(names, ", ")
	}
	return preview(p.Data)
}
