package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const help = `copier — a revisioned clipboard for desktops and SSH sessions

Commands:
  serve        Run the durable relay (default 127.0.0.1:2587)
  join URL     Save client configuration (--token-file PATH)
  token        Generate a random relay token
  copy         Copy stdin, --file PATH bytes, --files PATH... or desktop clipboard
  paste        Paste to stdout, --local desktop, or --files NEW_DIRECTORY
  history      Show retained revision metadata (--json for payloads)
  retry        Replay a retained copy (--file PATH; --force to promote)
  sync         Share clipboard offers; bytes transfer on remote demand
  agent        Serve this device's offers on remote demand
  ui           Interactive terminal menu
  xclip        Common xclip options backed by the relay
  wl-copy      Copy stdin/arguments to relay and local clipboard
  wl-paste     Paste relay to stdout

Run copier COMMAND --help for flags. Common client flags follow COMMAND:
  --url URL --board NAME --token-file PATH --allow-http
Environment: COPIER_URL, COPIER_TOKEN, COPIER_BOARD, COPIER_DEVICE.
With no command, a terminal opens ui; piped stdin runs copy.
`

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "copier:", err)
		var conflict *Conflict
		if errors.As(err, &conflict) {
			os.Exit(3)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		info, err := os.Stdin.Stat()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeCharDevice != 0 {
			args = []string{"ui"}
		} else {
			args = []string{"copy"}
		}
	}
	command := args[0]
	args = args[1:]
	if command == "--help" || command == "help" || command == "-h" {
		fmt.Print(help)
		return nil
	}
	if command == "token" {
		if len(args) > 0 {
			return fmt.Errorf("token takes no arguments")
		}
		fmt.Println(newID() + newID())
		return nil
	}
	if command == "serve" {
		return serve(ctx, args)
	}
	if command == "join" {
		return join(args)
	}
	if command == "mount-offer" {
		f := flag.NewFlagSet(command, flag.ContinueOnError)
		spec := f.String("spec", "", "private mount specification")
		if err := f.Parse(args); err != nil {
			return err
		}
		return runMount(ctx, *spec)
	}
	if command == "xclip" || command == "wl-copy" || command == "wl-paste" {
		return compatibility(ctx, command, args)
	}
	switch command {
	case "copy", "paste", "history", "retry", "sync", "ui", "agent", "fetch-format":
	default:
		return fmt.Errorf("unknown command %q; use copier --help", command)
	}
	config, err := readConfig()
	if err != nil {
		return err
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.StringVar(&config.URL, "url", config.URL, "relay URL")
	f.StringVar(&config.Board, "board", config.Board, "shared clipboard name")
	tokenFile := f.String("token-file", "", "read token from private file")
	allow := f.Bool("allow-http", false, "allow remote plain HTTP on a trusted VPN")
	backend := f.String("backend", "auto", "desktop backend: auto, wayland, x11")
	selection := f.String("selection", "clipboard", "desktop selection: clipboard or primary")
	fileDir := f.String("file-dir", "", "base directory for received desktop files (default: private state cache)")
	var file *string
	var local, from, force, jsonOut, types, files *bool
	var outputDir *string
	var revision *uint64
	var base *string
	var mime *string
	var interval *time.Duration
	var resolve *string
	var formatIndex *int
	var localOnly *bool
	switch command {
	case "copy":
		localOnly = f.Bool("local-only", false, "copy to the local desktop without contacting a relay")
		file = f.String("file", "", "read payload from file")
		files = f.Bool("files", false, "copy named files/folders as portable file objects (paths follow flags)")
		from = f.Bool("from-clipboard", false, "read all desktop clipboard formats")
		local = f.Bool("local", false, "also write successful copy to desktop")
		base = f.String("base", "", "expected revision (default: read current before write)")
		mime = f.String("type", "", "content MIME type (default: detect from bytes)")
	case "paste":
		local = f.Bool("local", false, "write all formats to desktop instead of stdout")
		outputDir = f.String("files", "", "extract copied files into a new directory (refuses existing destination)")
		revision = f.Uint64("revision", 0, "paste specific retained revision (0 means latest)")
		mime = f.String("type", "", "choose representation for stdout")
		types = f.Bool("list-types", false, "list available representations")
	case "history":
		jsonOut = f.Bool("json", false, "include payloads in JSON")
	case "retry":
		file = f.String("file", "", "retained operation JSON path")
		force = f.Bool("force", false, "explicitly promote retained copy against current revision")
	case "sync":
		interval = f.Duration("interval", 500*time.Millisecond, "poll interval (minimum 100ms)")
		resolve = f.String("resolve", "", "resolve pending conflict: local or remote")
	case "fetch-format":
		formatIndex = f.Int("index", -1, "offer representation index")
	}
	if err = f.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	if f.NArg() != 0 && (command != "copy" || !*files) {
		return fmt.Errorf("unexpected arguments: %v", f.Args())
	}
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		config.Token = strings.TrimSpace(string(b))
	}
	c, err := newClient(config, *allow)
	if err != nil && command == "copy" && *localOnly {
		c = &Client{}
		err = nil
	}
	if err != nil && command == "fetch-format" {
		c = &Client{}
		err = nil
	}
	if err != nil {
		return err
	}
	getCB := func() (*NativeClipboard, error) {
		cb, err := nativeClipboard(*backend, *selection)
		if err == nil {
			cb.FileDir = *fileDir
			cb.Client = c
		}
		return cb, err
	}
	switch command {
	case "copy":
		if *files && (*from || *file != "" || *mime != "") {
			return fmt.Errorf("--files cannot be combined with --file, --from-clipboard or --type")
		}
		if *from && *file != "" {
			return fmt.Errorf("choose --file or --from-clipboard")
		}
		var data []byte
		var cb *NativeClipboard
		var packet Packet
		if *from || *local || *localOnly {
			cb, err = getCB()
			if err != nil {
				return err
			}
		}
		if *files {
			if *localOnly {
				if len(f.Args()) == 0 {
					return fmt.Errorf("--files requires paths")
				}
				var paths []string
				for _, value := range f.Args() {
					full, e := filepath.Abs(value)
					if e != nil {
						return e
					}
					if _, e = os.Lstat(full); e != nil {
						return e
					}
					paths = append(paths, full)
				}
				packet = nativeFilePacket(paths)
			} else {
				packet, err = announceFiles(ctx, f.Args())
			}
		} else if *from {
			packet, err = cb.ReadForCopy(ctx)
		} else if *file != "" {
			var reader *os.File
			reader, err = os.Open(*file)
			if err == nil {
				defer reader.Close()
				data, err = readContent(reader)
			}
		} else {
			data, err = readContent(os.Stdin)
		}
		if err != nil {
			return err
		}
		if *from && *mime != "" {
			format, ok := packet.Formats[*mime]
			if !ok {
				return fmt.Errorf("desktop representation %q not available", *mime)
			}
			packet.MIME = *mime
			packet.Data = format.Data
		}
		var expected *uint64
		if *base != "" {
			r, err := strconv.ParseUint(*base, 10, 64)
			if err != nil {
				return err
			}
			expected = &r
		}
		if !*from && !*files {
			kind := *mime
			if kind == "" {
				kind = http.DetectContentType(data)
			}
			packet = Packet{MIME: kind, Data: data}
		}
		if *localOnly {
			if expected != nil {
				return fmt.Errorf("--base does not apply to local-only copies")
			}
			return cb.WriteLocalPacket(ctx, packet)
		}
		if *local {
			localPacket, err := announcePacket(ctx, packet)
			if err != nil {
				return err
			}
			if err = cb.WritePacket(ctx, localPacket); err != nil {
				return err
			}
		}
		out, err := c.copyPacket(ctx, packet, expected)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "copied revision %d\n", out.Revision)
	case "paste":
		if *outputDir != "" && (*local || *types || *mime != "") {
			return fmt.Errorf("--files cannot be combined with --local, --list-types or --type")
		}
		out, err := c.get(ctx, *revision)
		if err != nil {
			return err
		}
		if out.Revision == 0 {
			return fmt.Errorf("clipboard is empty")
		}
		if *outputDir != "" {
			var paths []string
			if out.MIME == offerMIME {
				var offer Offer
				offer, err = packetOffer(out.packet())
				if err == nil {
					paths, err = c.extractOffer(ctx, offer, *outputDir)
				}
			} else {
				paths, err = extractFiles(ctx, out.packet(), *outputDir)
			}
			if err != nil {
				return err
			}
			for _, name := range paths {
				fmt.Println(name)
			}
			return nil
		}
		if *types {
			names := packetTypes(out.packet())
			if out.MIME == offerMIME {
				offer, err := packetOffer(out.packet())
				if err != nil {
					return err
				}
				names = nil
				if offer.Kind == "files" {
					names = []string{"text/uri-list", "x-special/gnome-copied-files"}
				} else {
					for _, entry := range offer.Entries {
						names = append(names, entry.Name)
					}
				}
			}
			for _, name := range names {
				fmt.Println(name)
			}
			return nil
		}
		if *local {
			cb, err := getCB()
			if err != nil {
				return err
			}
			return cb.WritePacket(ctx, out.packet())
		}
		if out.MIME == offerMIME {
			offer, err := packetOffer(out.packet())
			if err != nil {
				return err
			}
			return c.streamOfferFormat(ctx, offer, *mime, os.Stdout)
		}
		data := out.Data
		if *mime != "" && *mime != out.MIME {
			format, ok := out.Formats[*mime]
			if !ok {
				return fmt.Errorf("representation %q not available", *mime)
			}
			data = format.Data
		}
		_, err = os.Stdout.Write(data)
		return err
	case "history":
		clips, err := c.history(ctx)
		if err != nil {
			return err
		}
		if *jsonOut {
			return writeJSON(os.Stdout, clips)
		}
		for i := len(clips) - 1; i >= 0; i-- {
			c := clips[i]
			fmt.Printf("%d\t%s\t%q\t%s\t%d bytes\n", c.Revision, c.Created.Format(time.RFC3339), c.Origin, c.MIME, len(c.Data))
		}
	case "retry":
		if *file == "" {
			return fmt.Errorf("retry requires --file")
		}
		return retryPending(ctx, c, *file, *force)
	case "sync":
		if *interval < 100*time.Millisecond {
			return fmt.Errorf("interval must be at least 100ms")
		}
		cb, err := getCB()
		if err != nil {
			return err
		}
		return runSync(ctx, c, cb, syncPath(c, cb.Backend, cb.Selection), *interval, *resolve)
	case "ui":
		return interactive(ctx, c, *backend, *selection, *fileDir, os.Stdin, os.Stdout)
	case "agent":
		return c.runAgent(ctx)
	case "fetch-format":
		offer, err := parseOfferInput(os.Stdin)
		if err != nil {
			return err
		}
		if *formatIndex < 0 || *formatIndex >= len(offer.Entries) {
			return fmt.Errorf("invalid format index")
		}
		return c.copyEntry(ctx, offer, *formatIndex, os.Stdout)
	}
	return nil
}

func serve(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := f.String("listen", "127.0.0.1:2587", "listen address")
	path := f.String("data", filepath.Join(stateDir(), "relay.json"), "durable database snapshot")
	tokenFile := f.String("token-file", "", "token file (or COPIER_TOKEN)")
	cert := f.String("tls-cert", "", "TLS certificate PEM")
	key := f.String("tls-key", "", "TLS private key PEM")
	allow := f.Bool("allow-http", false, "allow non-loopback HTTP on a trusted VPN")
	cacheQuota := f.Int64("cache-quota", 20<<30, "maximum relay chunk-cache bytes (default 20 GiB)")
	if err := f.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("serve takes no positional arguments")
	}
	token := os.Getenv("COPIER_TOKEN")
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		token = strings.TrimSpace(string(b))
	}
	if len(token) < 16 {
		return fmt.Errorf("set COPIER_TOKEN or --token-file to a token of at least 16 characters (copier token)")
	}
	if (*cert == "") != (*key == "") {
		return fmt.Errorf("provide both --tls-cert and --tls-key")
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if *cert == "" && !*allow && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("non-loopback listening requires TLS or explicit --allow-http on a trusted VPN")
	}
	absolute, err := filepath.Abs(*path)
	if err != nil {
		return err
	}
	lock, err := lockFile(absolute + ".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	relay, err := newRelay(absolute, token)
	if err != nil {
		return err
	}
	if *cacheQuota < chunkSize {
		return fmt.Errorf("cache quota must be at least 1 MiB")
	}
	relay.chunkQuota = *cacheQuota
	server := &http.Server{Addr: *addr, Handler: relay, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() {
		if *cert != "" {
			done <- server.ListenAndServeTLS(*cert, *key)
		} else {
			done <- server.ListenAndServe()
		}
	}()
	fmt.Fprintf(os.Stderr, "relay listening on %s; database %s\n", *addr, absolute)
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

func join(args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Println("copier join URL --token-file PATH [--board NAME] [--allow-http]")
		return nil
	}
	// Accept the URL before flags to make `copier join URL --token-file ...` natural.
	if len(args) == 0 {
		return fmt.Errorf("usage: copier join URL --token-file PATH [--board NAME]")
	}
	url := args[0]
	args = args[1:]
	f := flag.NewFlagSet("join", flag.ContinueOnError)
	file := f.String("token-file", "", "read token from private file (or COPIER_TOKEN)")
	board := f.String("board", "default", "shared clipboard name")
	allow := f.Bool("allow-http", false, "validate a trusted VPN HTTP URL")
	if err := f.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	token := os.Getenv("COPIER_TOKEN")
	if *file != "" {
		b, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		token = strings.TrimSpace(string(b))
	}
	c, err := newClient(Config{url, token, *board}, *allow)
	if err != nil {
		return err
	}
	path := filepath.Join(configDir(), "config.json")
	if err = saveJSON(path, c.config); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "saved client configuration to %s\n", path)
	return nil
}
