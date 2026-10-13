package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
)

func copyFile(ctx context.Context, c *Client, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := readContent(f)
	if err != nil {
		return err
	}
	_, err = c.copy(ctx, b, http.DetectContentType(b), nil)
	return err
}

// These wrappers implement the usual piping forms, rejecting unsupported
// options instead of quietly changing semantics. Native backend calls never
// invoke these wrappers, so shell aliases do not recurse.
func compatibility(ctx context.Context, command string, args []string) error {
	paste := command == "wl-paste"
	selection := "clipboard"
	mime := ""
	list := false
	newline := false
	var words []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := func() (string, error) {
			i++
			if i >= len(args) {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			return args[i], nil
		}
		switch arg {
		case "--help", "-h":
			fmt.Println("Supported: xclip -i/-o -selection clipboard/primary -target TYPE; wl-copy --primary --type TYPE [TEXT]; wl-paste --primary --type TYPE --list-types --no-newline. Uses COPIER_* or join configuration.")
			return nil
		case "-o", "-out":
			if command != "xclip" {
				return fmt.Errorf("unsupported option %s", arg)
			}
			paste = true
		case "-i", "-in":
			if command != "xclip" {
				return fmt.Errorf("unsupported option %s", arg)
			}
			paste = false
		case "-selection":
			v, e := value()
			if e != nil {
				return e
			}
			selection = v
		case "--primary", "-p":
			selection = "primary"
		case "--type", "-t", "-target":
			v, e := value()
			if e != nil {
				return e
			}
			mime = v
		case "--list-types", "-l":
			if command != "wl-paste" {
				return fmt.Errorf("unsupported option %s", arg)
			}
			list = true
		case "--no-newline", "-n":
			if command != "wl-paste" {
				return fmt.Errorf("unsupported option %s", arg)
			}
		case "--":
			words = append(words, args[i+1:]...)
			i = len(args)
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unsupported compatibility option %q", arg)
			}
			words = append(words, arg)
		}
	}
	if selection != "clipboard" && selection != "primary" {
		return fmt.Errorf("selection must be clipboard or primary")
	}
	config, err := readConfig()
	if err != nil {
		return err
	}
	c, err := newClient(config, false)
	if err != nil {
		return err
	}
	if paste {
		if len(words) > 0 {
			return fmt.Errorf("paste takes no positional arguments")
		}
		clip, err := c.get(ctx, 0)
		if err != nil {
			return err
		}
		if clip.Revision == 0 {
			return fmt.Errorf("clipboard is empty")
		}
		if clip.MIME == offerMIME {
			o, err := packetOffer(clip.packet())
			if err != nil {
				return err
			}
			if list {
				if o.Kind == "files" {
					fmt.Println("text/uri-list")
				} else {
					for _, e := range o.Entries {
						fmt.Println(e.Name)
					}
				}
				return nil
			}
			return c.streamOfferFormat(ctx, o, mime, os.Stdout)
		}
		if list {
			for _, n := range packetTypes(clip.packet()) {
				fmt.Println(n)
			}
			return nil
		}
		data := clip.Data
		if mime != "" && mime != clip.MIME {
			f, ok := clip.Formats[mime]
			if !ok {
				return fmt.Errorf("representation %q not available", mime)
			}
			data = f.Data
		}
		newline = command == "wl-paste" && strings.HasPrefix(clip.MIME, "text/")
		for _, a := range args {
			if a == "--no-newline" || a == "-n" {
				newline = false
			}
		}
		if _, err = os.Stdout.Write(data); err != nil {
			return err
		}
		if newline && (len(data) == 0 || data[len(data)-1] != '\n') {
			_, err = os.Stdout.Write([]byte{'\n'})
		}
		return err
	}
	var data []byte
	if command == "wl-copy" && len(words) > 0 {
		data = []byte(strings.Join(words, " "))
	} else if len(words) == 1 && command == "xclip" {
		f, e := os.Open(words[0])
		if e != nil {
			return e
		}
		defer f.Close()
		data, err = readContent(f)
	} else if len(words) > 0 {
		return fmt.Errorf("unsupported positional arguments")
	} else {
		data, err = readContent(os.Stdin)
	}
	if err != nil {
		return err
	}
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	packet := Packet{MIME: mime, Data: data}
	clip, err := c.copyPacket(ctx, packet, nil)
	if err != nil {
		return err
	}
	// On headless servers the wrappers act as remote-only clipboard commands.
	if os.Getenv("WAYLAND_DISPLAY") != "" || os.Getenv("DISPLAY") != "" {
		cb, e := nativeClipboard("auto", selection)
		if e != nil {
			return e
		}
		cb.Client = c
		return cb.WritePacket(ctx, clip.packet())
	}
	return nil
}
