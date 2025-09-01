package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/example/lsp-mux/internal/mux"
)

// ------- CLI parsing helpers -------

type cliConfig struct {
	modeServe  bool
	socketPath string
	rootDir    string
	lang       string
	serverCmd  []string
	socketDir  string
	tagFile    string
	linger     time.Duration
	verbose    bool
}

func parseCLI() (*cliConfig, []string) {
	cfg := &cliConfig{
		socketDir: defaultSocketDir(),
		tagFile:   ".lspmux",
		linger:    10 * time.Minute,
	}

	// We need to capture args after "--" as the real LSP server command-line suffix.
	// Split os.Args manually for reliability.
	args := os.Args[1:]
	serverSuffix := []string{}
	if i := indexOf(args, "--"); i >= 0 {
		serverSuffix = append(serverSuffix, args[i+1:]...)
		args = args[:i]
	}

	fs := flag.NewFlagSet("lsp-mux", flag.ContinueOnError)
	fs.BoolVar(&cfg.modeServe, "serve", false, "Run as background mux server (internal)")
	fs.StringVar(&cfg.socketPath, "socket", "", "Unix socket path (internal or override)")
	fs.StringVar(&cfg.rootDir, "root", "", "Project root directory (internal)")
	fs.StringVar(&cfg.lang, "lang", "", "Language tag/key (e.g. kotlin, nim)")
	fs.StringVar(&cfg.socketDir, "socket-dir", cfg.socketDir, "Directory to store mux sockets")
	fs.StringVar(&cfg.tagFile, "tag-file", cfg.tagFile, "Marker file to detect project root")
	fs.DurationVar(&cfg.linger, "linger", cfg.linger, "Linger time after last client disconnect")
	fs.BoolVar(&cfg.verbose, "v", false, "Verbose logging to stderr")
	serverBin := fs.String("server", "", "Path to real LSP server binary (required in client mode)")
	if err := fs.Parse(args); err != nil {
		// flag package already wrote error
		os.Exit(2)
	}

	rest := fs.Args()
	if !cfg.modeServe {
		// client/entry mode
		if *serverBin != "" {
			cfg.serverCmd = append([]string{*serverBin}, serverSuffix...)
		} else if len(rest) > 0 {
			// Allow passing server command directly without --server, e.g.:
			// lsp-mux --lang kotlin -- /usr/bin/kotlin-lsp --stdio
			cfg.serverCmd = append([]string{rest[0]}, append(rest[1:], serverSuffix...)...)
		} else {
			// If nothing specified, try serverSuffix as full command.
			if len(serverSuffix) > 0 {
				cfg.serverCmd = append([]string{serverSuffix[0]}, serverSuffix[1:]...)
			}
		}
	} else {
		// serve mode: remaining args are the server command
		if len(rest) > 0 {
			cfg.serverCmd = rest
		} else if len(serverSuffix) > 0 {
			cfg.serverCmd = serverSuffix
		}
	}

	return cfg, rest
}

func indexOf(xs []string, s string) int {
	for i, v := range xs {
		if v == s {
			return i
		}
	}
	return -1
}

func defaultSocketDir() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "lsp-mux")
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "."
	}
	return filepath.Join(home, ".cache", "lsp-mux")
}

// --------- LSP frame utils (client-side bootstrap) ---------

// readOneFrame reads a single LSP frame (headers + body) from r.
// It returns the raw payload body and the full raw frame (headers + body).
func readOneFrame(r *bufio.Reader) (body []byte, rawFrame []byte, err error) {
	// Read headers
	var hdr bytes.Buffer
	contentLen := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, hdr.Bytes(), err
		}
		hdr.WriteString(line)
		if line == "\r\n" {
			break
		}
		ll := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(ll), "content-length:") {
			var n int
			if _, err := fmt.Sscanf(ll, "Content-Length: %d", &n); err == nil {
				contentLen = n
			}
		}
	}
	if contentLen < 0 {
		return nil, hdr.Bytes(), fmt.Errorf("missing Content-Length header")
	}
	body = make([]byte, contentLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, append(hdr.Bytes(), body...), err
	}
	raw := append(hdr.Bytes(), body...)
	return body, raw, nil
}

// --------- init param helpers ---------

type initializeParams struct {
	RootURI          string `json:"rootUri"`
	RootPath         string `json:"rootPath"`
	WorkspaceFolders []struct {
		URI  string `json:"uri"`
		Name string `json:"name"`
	} `json:"workspaceFolders"`
}

type jsonrpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

func fileURIToPath(uri string) string {
	if strings.HasPrefix(uri, "file://") {
		// Handle URI-encoded paths minimally (%20 etc)
		p := uri[len("file://"):]
		// On Windows, file:///C:/... -> /C:/...
		p = strings.ReplaceAll(p, "%20", " ")
		p = strings.ReplaceAll(p, "%5B", "[")
		p = strings.ReplaceAll(p, "%5D", "]")
		p = strings.ReplaceAll(p, "%7B", "{")
		p = strings.ReplaceAll(p, "%7D", "}")
		return p
	}
	return uri
}

func findProjectRoot(start string, tagFile string) (string, error) {
	dir := start
	info, err := os.Stat(dir)
	if err == nil && !info.IsDir() {
		dir = filepath.Dir(dir)
	}
	for {
		marker := filepath.Join(dir, tagFile)
		if st, err := os.Stat(marker); err == nil && !st.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("no %s found upwards from %s", tagFile, start)
}

// --------- client (entry) mode ---------

func clientMain(cfg *cliConfig) int {
	if cfg.lang == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --lang is required (e.g. --lang=kotlin)")
		return 2
	}
	if len(cfg.serverCmd) == 0 {
		fmt.Fprintln(os.Stderr, "ERROR: LSP server command not given. Use --server <bin> [-- ...args]")
		return 2
	}

	// 1) Peek first message from STDIN to get initialize params
	stdinR := bufio.NewReader(os.Stdin)
	firstBody, firstRaw, err := readOneFrame(stdinR)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: reading first LSP frame: %v\n", err)
		return 1
	}

	var first jsonrpcMsg
	_ = json.Unmarshal(firstBody, &first)
	if first.Method != "initialize" {
		// Still try to proceed; fall back to CWD for root detection.
		if cfg.verbose {
			fmt.Fprintln(os.Stderr, "WARN: first message wasn't initialize; proceeding with CWD as hint")
		}
	}

	var hintPath string
	if first.Method == "initialize" && len(first.Params) > 0 {
		var p initializeParams
		_ = json.Unmarshal(first.Params, &p)
		if p.RootURI != "" {
			hintPath = fileURIToPath(p.RootURI)
		} else if p.RootPath != "" {
			hintPath = p.RootPath
		} else if len(p.WorkspaceFolders) > 0 {
			hintPath = fileURIToPath(p.WorkspaceFolders[0].URI)
		}
	}
	if hintPath == "" {
		cwd, _ := os.Getwd()
		hintPath = cwd
	}

	root, err := findProjectRoot(hintPath, cfg.tagFile)
	if err != nil {
		// As a fallback, use the nearest directory (does not enforce marker)
		if cfg.verbose {
			fmt.Fprintf(os.Stderr, "WARN: %v; falling back to %s\n", err, filepath.Dir(hintPath))
		}
		root = filepath.Dir(hintPath)
	}

	if cfg.socketPath == "" {
		if err := os.MkdirAll(cfg.socketDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: create socket dir: %v\n", err)
			return 1
		}
		cfg.socketPath = mux.SocketPathFor(cfg.socketDir, cfg.lang, root)
	}

	// 2) Try connect to existing mux server
	conn, err := net.Dial("unix", cfg.socketPath)
	if err != nil {
		// 3) Start a background mux server and retry
		if cfg.verbose {
			fmt.Fprintf(os.Stderr, "Starting mux server for %s@%s\n", cfg.lang, root)
		}
		if code := spawnServerAndWait(cfg, root); code != 0 {
			return code
		}
		// retry connect
		for i := 0; i < 50; i++ { // ~2.5s
			conn, err = net.Dial("unix", cfg.socketPath)
			if err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: could not connect to mux server: %v\n", err)
			return 1
		}
	}
	defer conn.Close()

	// 4) Pump first buffered frame, then stream the rest both ways
	if _, err := conn.Write(firstRaw); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: write initial frame to mux: %v\n", err)
		return 1
	}

	// forward remaining stdin -> socket
	done := make(chan struct{}, 2)
	go func() {
		// Any residual bytes buffered in stdinR should be forwarded too.
		if stdinR.Buffered() > 0 {
			if _, err := io.Copy(conn, stdinR); err != nil && cfg.verbose {
				fmt.Fprintf(os.Stderr, "stdin->sock copy error: %v\n", err)
			}
		}
		if _, err := io.Copy(conn, os.Stdin); err != nil && cfg.verbose {
			fmt.Fprintf(os.Stderr, "stdin->sock copy error: %v\n", err)
		}
		_ = conn.(*net.UnixConn).CloseWrite()
		done <- struct{}{}
	}()

	go func() {
		if _, err := io.Copy(os.Stdout, conn); err != nil && cfg.verbose {
			fmt.Fprintf(os.Stderr, "sock->stdout copy error: %v\n", err)
		}
		done <- struct{}{}
	}()

	<-done
	return 0
}

func spawnServerAndWait(cfg *cliConfig, root string) int {
	args := []string{
		"--serve",
		"--socket", cfg.socketPath,
		"--root", root,
		"--lang", cfg.lang,
		"--linger", cfg.linger.String(),
	}
	if cfg.verbose {
		args = append(args, "-v")
	}
	// Append server command after "--"
	args = append(args, "--")
	args = append(args, cfg.serverCmd...)
	cmd := exec.Command(os.Args[0], args...)
	// Detach: we'll let it inherit stderr for logs
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: start mux server: %v\n", err)
		return 1
	}
	// don't wait; server will be reachable via socket
	return 0
}

// --------- serve mode ---------

func serveMain(cfg *cliConfig) int {
	if cfg.socketPath == "" {
		if cfg.rootDir == "" || cfg.lang == "" {
			fmt.Fprintln(os.Stderr, "ERROR: --socket or (--root and --lang) must be provided in --serve mode")
			return 2
		}
		if err := os.MkdirAll(cfg.socketDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: create socket dir: %v\n", err)
			return 1
		}
		cfg.socketPath = mux.SocketPathFor(cfg.socketDir, cfg.lang, cfg.rootDir)
	}
	// Ensure previous stale socket is gone
	_ = os.Remove(cfg.socketPath)

	if cfg.verbose {
		fmt.Fprintf(os.Stderr, "[serve] socket=%s\n", cfg.socketPath)
	}

	s, err := mux.NewServer(mux.ServerConfig{
		SocketPath: cfg.socketPath,
		Lang:       cfg.lang,
		RootDir:    cfg.rootDir,
		Linger:     cfg.linger,
		ServerCmd:  cfg.serverCmd,
		Verbose:    cfg.verbose,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: create server: %v\n", err)
		return 1
	}
	defer s.Close()
	return s.Serve()
}

func main() {
	cfg, _ := parseCLI()
	if cfg.modeServe {
		os.Exit(serveMain(cfg))
	}
	os.Exit(clientMain(cfg))
}
