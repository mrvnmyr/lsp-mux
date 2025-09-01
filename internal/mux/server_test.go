package mux

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// --- Helper process that acts as a tiny fake LSP server --------------------

// Run with:
//   GO_WANT_LSP_HELPER=1 go test -run TestHelperFakeLSP
func TestHelperFakeLSP(t *testing.T) {
	if os.Getenv("GO_WANT_LSP_HELPER") != "1" {
		return
	}
	runFakeLSPServer()
	os.Exit(0)
}

func runFakeLSPServer() {
	rd := bufio.NewReader(os.Stdin)
	initCount := 0
	initializedCount := 0
	echoCount := 0

	for {
		body, err := readFrame(rd)
		if err != nil {
			// EOF or similar -> exit
			fmt.Fprintf(os.Stderr, "[fake-lsp] read error: %v\n", err)
			return
		}
		var m Message
		_ = json.Unmarshal(body, &m)

		switch {
		case isRequest(&m) && m.Method == "initialize":
			initCount++
			// Reply with simple server capabilities
			res := map[string]any{
				"capabilities": map[string]any{
					"hoverProvider": true,
					"definitionProvider": true,
				},
				"serverInfo": map[string]string{
					"name":    "fake-lsp",
					"version": "1.0",
				},
			}
			reply := Message{JSONRPC: "2.0", ID: m.ID, Result: mustMarshal(res)}
			_ = writeBody(os.Stdout, mustMarshal(reply))

		case isNotification(&m) && m.Method == "initialized":
			initializedCount++
			// no response

		case isRequest(&m) && m.Method == "fake/echo":
			echoCount++
			// Echo back params as result
			reply := Message{JSONRPC: "2.0", ID: m.ID, Result: m.Params}
			_ = writeBody(os.Stdout, mustMarshal(reply))

		case isRequest(&m) && m.Method == "fake/getCounts":
			payload := map[string]int{
				"initialize":  initCount,
				"initialized": initializedCount,
				"echo":        echoCount,
			}
			reply := Message{JSONRPC: "2.0", ID: m.ID, Result: mustMarshal(payload)}
			_ = writeBody(os.Stdout, mustMarshal(reply))

		case isRequest(&m) && m.Method == "shutdown":
			// Respond null result per LSP
			reply := Message{JSONRPC: "2.0", ID: m.ID, Result: json.RawMessage("null")}
			_ = writeBody(os.Stdout, mustMarshal(reply))

		case isNotification(&m) && m.Method == "exit":
			return

		default:
			// Unknown -> send minimal error to be explicit
			errObj := map[string]any{
				"code":    -32601,
				"message": "method not found",
			}
				// NOTE: keep fields to spec
			reply := struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Error   any             `json:"error"`
			}{
				JSONRPC: "2.0",
				ID:      m.ID,
				Error:   errObj,
			}
			_ = writeBody(os.Stdout, mustMarshal(reply))
		}
	}
}

// --- Tests -----------------------------------------------------------------

// TestTripleOpenCloseReuse simulates opening the same file 3 times (sequentially),
// with each "editor" connecting, initializing, sending a request, then exiting.
// We assert that:
//   * Only one initialize reaches the real server (others are served from cache).
//   * Only one 'initialized' notification is forwarded to the real server.
//   * Requests keep working across all 3 sessions.
//   * The same LSP server process is reused.
func TestTripleOpenCloseReuse(t *testing.T) {
	t.Parallel()

	// Prepare socket path in temp dir
	td := t.TempDir()
	socketDir := td
	rootDir := filepath.Join(td, "proj")
	_ = os.MkdirAll(rootDir, 0o755)

	socketPath := SocketPathFor(socketDir, "kotlin", rootDir)

	// Start mux server with fake LSP helper process
	helperCmd := []string{os.Args[0], "-test.run", "TestHelperFakeLSP", "--"}
	cmd := exec.Command(helperCmd[0], helperCmd[1:]...)
	cmd.Env = append(os.Environ(), "GO_WANT_LSP_HELPER=1")

	s, err := NewServer(ServerConfig{
		SocketPath: socketPath,
		Lang:       "kotlin",
		RootDir:    rootDir,
		Linger:     200 * time.Millisecond, // quick linger to help tests clean up
		ServerCmd:  helperCmd,
		Verbose:    true,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// Ensure helper gets the env
	s.lspCmd.Env = append(os.Environ(), "GO_WANT_LSP_HELPER=1")

	defer func() {
		_ = s.Close()
		// Give linger a chance to gracefully tear down the child
		time.Sleep(500 * time.Millisecond)
	}()

	// Run the mux server loop
	done := make(chan struct{})
	go func() {
		_ = s.Serve()
		close(done)
	}()

	// Helper to run a single simulated editor session.
	runSession := func(i int) {
		t.Helper()
		conn, err := net.Dial("unix", socketPath)
		if err != nil {
			t.Fatalf("session %d: dial: %v", i, err)
		}
		defer conn.Close()

		r := bufio.NewReader(conn)

		// 1) initialize
		initID := json.RawMessage(fmt.Sprintf("%d01", i))
		initReq := Message{
			JSONRPC: "2.0",
			ID:      initID,
			Method:  "initialize",
			Params: mustMarshal(map[string]any{
				"rootUri": fmt.Sprintf("file://%s", rootDir),
			}),
		}
		if err := writeBody(conn, mustMarshal(initReq)); err != nil {
			t.Fatalf("session %d: write initialize: %v", i, err)
		}
		// Expect response
		body, err := readFrame(r)
		if err != nil {
			t.Fatalf("session %d: read initialize resp: %v", i, err)
		}
		var resp Message
		_ = json.Unmarshal(body, &resp)
		if len(resp.Result) == 0 {
			t.Fatalf("session %d: initialize missing result", i)
		}

		// 2) initialized notification
		initialized := Message{
			JSONRPC: "2.0",
			Method:  "initialized",
		}
		_ = writeBody(conn, mustMarshal(initialized))

		// 3) echo request to prove functionality
		echoID := json.RawMessage(fmt.Sprintf("%d02", i))
		msg := fmt.Sprintf("hello-%d", i)
		echoReq := Message{
			JSONRPC: "2.0",
			ID:      echoID,
			Method:  "fake/echo",
			Params:  mustMarshal(map[string]string{"msg": msg}),
		}
		if err := writeBody(conn, mustMarshal(echoReq)); err != nil {
			t.Fatalf("session %d: write echo: %v", i, err)
		}
		// Read echo response
		body, err = readFrame(r)
		if err != nil {
			t.Fatalf("session %d: read echo resp: %v", i, err)
		}
		var echoResp Message
		_ = json.Unmarshal(body, &echoResp)
		var got map[string]string
		_ = json.Unmarshal(echoResp.Result, &got)
		if got["msg"] != msg {
			t.Fatalf("session %d: echo mismatch: got %q want %q", i, got["msg"], msg)
		}
	}

	// Session 1, 2, 3
	runSession(1)
	runSession(2)

	// Session 3: also fetch server counts before closing
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("session 3: dial: %v", err)
	}
	r := bufio.NewReader(conn)
	// initialize
	initReq3 := Message{
		JSONRPC: "2.0",
		ID:      json.RawMessage("301"),
		Method:  "initialize",
		Params:  mustMarshal(map[string]any{"rootUri": fmt.Sprintf("file://%s", rootDir)}),
	}
	_ = writeBody(conn, mustMarshal(initReq3))
	_, err = readFrame(r)
	if err != nil {
		t.Fatalf("session 3: read initialize resp: %v", err)
	}
	// initialized (will be dropped by mux if already forwarded once)
	_ = writeBody(conn, mustMarshal(Message{JSONRPC: "2.0", Method: "initialized"}))
	// getCounts
	getCounts := Message{
		JSONRPC: "2.0",
		ID:      json.RawMessage("302"),
		Method:  "fake/getCounts",
	}
	_ = writeBody(conn, mustMarshal(getCounts))
	body, err := readFrame(r)
	if err != nil {
		t.Fatalf("session 3: read getCounts: %v", err)
	}
	var countsResp Message
	_ = json.Unmarshal(body, &countsResp)
	var counts map[string]int
	_ = json.Unmarshal(countsResp.Result, &counts)

	t.Logf("server counts: %+v", counts)

	if counts["initialize"] != 1 {
		t.Fatalf("expected server initialize count=1, got %d", counts["initialize"])
	}
	if counts["initialized"] != 1 {
		t.Fatalf("expected server 'initialized' forwarded once, got %d", counts["initialized"])
	}
	if counts["echo"] != 3 {
		t.Fatalf("expected 3 echo requests handled, got %d", counts["echo"])
	}
	_ = conn.Close()

	// After last client, linger should shut down the LSP server
	time.Sleep(600 * time.Millisecond)
}
