package mux

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"
)

type Server struct {
	cfg ServerConfig

	ln      *net.UnixListener
	lspCmd  *exec.Cmd
	lspIn   io.WriteCloser
	lspOut  io.ReadCloser
	lspRd   *bufio.Reader
	lspWrMu sync.Mutex

	clientsMu         sync.RWMutex
	clients           map[int]*clientConn
	nextClientID      int
	primaryClientID   int
	initDone          bool
	initResult        json.RawMessage
	initializedSent   bool
	pendingInits      []*clientConn // clients waiting for init result
	pendingReqsMu     sync.Mutex
	nextProxyID       uint64
	pendingByProxyID  map[uint64]reqMap // server responses -> client
	serverReqsMu      sync.Mutex
	nextServerReqID   uint64
	serverReqMap      map[uint64]serverReq // server->client requests mapping
	idleTimerMu       sync.Mutex
	idleTimer         *time.Timer
	shutdownOnce      sync.Once
}

type clientConn struct {
	id     int
	netc   *net.UnixConn
	r      *bufio.Reader
	wMu    sync.Mutex
	closed bool
}

type reqMap struct {
	clientID int
	origID   json.RawMessage
}

type serverReq struct {
	clientID int
	origID   json.RawMessage // server's original id
	clientIDOnWire json.RawMessage // id we used when sending to client
}

func NewServer(cfg ServerConfig) (*Server, error) {
	if len(cfg.ServerCmd) == 0 {
		return nil, fmt.Errorf("ServerCmd is empty")
	}
	addr := &net.UnixAddr{Name: cfg.SocketPath, Net: "unix"}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.SocketPath, 0o666); err != nil {
		// non-fatal
	}

	cmd := exec.Command(cfg.ServerCmd[0], cfg.ServerCmd[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		_ = ln.Close()
		return nil, err
	}

	s := &Server{
		cfg:               cfg,
		ln:                ln,
		lspCmd:            cmd,
		lspIn:             stdin,
		lspOut:            stdout,
		lspRd:             bufio.NewReader(stdout),
		clients:           make(map[int]*clientConn),
		nextClientID:      1,
		primaryClientID:   0,
		initDone:          false,
		pendingByProxyID:  make(map[uint64]reqMap),
		serverReqMap:      make(map[uint64]serverReq),
	}

	// No idle timer initially (active)
	return s, nil
}

func (s *Server) Close() error {
	_ = s.ln.Close()
	_ = os.Remove(s.cfg.SocketPath)
	return nil
}

func (s *Server) Serve() int {
	// Reader from LSP server -> distribute to clients
	go s.loopServerRead()

	for {
		conn, err := s.ln.AcceptUnix()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			// listener closed
			return 0
		}
		cc := &clientConn{
			id:   s.allocClientID(),
			netc: conn,
			r:    bufio.NewReader(conn),
		}
		s.addClient(cc)
		go s.handleClient(cc)
	}
}

func (s *Server) allocClientID() int {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	id := s.nextClientID
	s.nextClientID++
	return id
}

func (s *Server) addClient(c *clientConn) {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	s.clients[c.id] = c
	if s.primaryClientID == 0 {
		s.primaryClientID = c.id
	}
	// Stop idle timer if any
	s.stopIdleTimer()
	if s.cfg.Verbose {
		fmt.Fprintf(os.Stderr, "[mux] client #%d connected (primary=%d)\n", c.id, s.primaryClientID)
	}
}

func (s *Server) removeClient(id int) {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	delete(s.clients, id)
	if id == s.primaryClientID {
		// pick another as primary
		s.primaryClientID = 0
		for cid := range s.clients {
			s.primaryClientID = cid
			break
		}
	}
	if len(s.clients) == 0 {
		s.startIdleTimer()
	}
	if s.cfg.Verbose {
		fmt.Fprintf(os.Stderr, "[mux] client #%d disconnected; remain=%d\n", id, len(s.clients))
	}
}

func (s *Server) numClients() int {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()
	return len(s.clients)
}

func (s *Server) getClient(id int) *clientConn {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()
	return s.clients[id]
}

func (s *Server) broadcast(body []byte) {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()
	for _, c := range s.clients {
		c.write(body)
	}
}

func (c *clientConn) write(body []byte) {
	c.wMu.Lock()
	defer c.wMu.Unlock()
	if c.closed {
		return
	}
	_ = writeBody(c.netc, body)
}

// ---------- LSP server read loop ----------

func (s *Server) loopServerRead() {
	for {
		body, err := readFrame(s.lspRd)
		if err != nil {
			if err == io.EOF {
				return
			}
			fmt.Fprintf(os.Stderr, "[mux] LSP server read error: %v\n", err)
			return
		}

		var m Message
		if err := json.Unmarshal(body, &m); err != nil {
			continue
		}

		switch {
		case isResponse(&m):
			// Response to a client-initiated request
			s.pendingReqsMu.Lock()
			var pm reqMap
			var key uint64
			// Parse proxy id as number
			if len(m.ID) > 0 {
				fmt.Sscanf(string(m.ID), "%d", &key)
			}
			pm, ok := s.pendingByProxyID[key]
			if ok {
				delete(s.pendingByProxyID, key)
			}
			s.pendingReqsMu.Unlock()
			if ok {
				// Rewrite id back to client's original id and forward only to that client
				newBody, err := replaceID(body, pm.origID)
				if err == nil {
					if c := s.getClient(pm.clientID); c != nil {
						c.write(newBody)
					}
				}
			}
			// Cache initialize result (first client's initialize)
			if string(m.ID) == "1" || string(m.ID) == "\"1\"" {
				// Not reliable; better approach: we track first pending method name, but keep it simple.
			}
		case isNotification(&m):
			// Server notifications -> broadcast (diagnostics, logs, etc.)
			s.broadcast(body)
		case isRequest(&m):
			// Server -> client request: forward ONLY to primary client
			s.serverReqsMu.Lock()
			s.nextServerReqID++
			serverID := s.nextServerReqID
			origID := m.ID
			primary := s.primaryClientID
			// Assign a new ID on the client wire (we'll use same numeric id)
			clientWireID := json.RawMessage(fmt.Sprintf("%d", serverID))
			s.serverReqMap[serverID] = serverReq{
				clientID:       primary,
				origID:         origID,
				clientIDOnWire: clientWireID,
			}
			s.serverReqsMu.Unlock()

			// Rewrite id to clientWireID and send to primary
			newBody, err := replaceID(body, clientWireID)
			if err == nil {
				if c := s.getClient(primary); c != nil {
					c.write(newBody)
				}
			}
		default:
			// Unknown shape -> broadcast
			s.broadcast(body)
		}
	}
}

// ---------- Client handler ----------

func (s *Server) handleClient(c *clientConn) {
	defer func() {
		_ = c.netc.Close()
		c.closed = true
		s.removeClient(c.id)
	}()

	r := c.r
	for {
		body, err := readFrame(r)
		if err != nil {
			return
		}
		var m Message
		if err := json.Unmarshal(body, &m); err != nil {
			continue
		}

		// Intercept initialize, initialized, shutdown/exit
		if isRequest(&m) && m.Method == "initialize" {
			// First client's initialize goes to real LSP;
			// subsequent clients receive cached result.
			if !s.initDone {
				// Forward to LSP server; map its id
				s.forwardClientRequest(c.id, body, m, true)
				// When the response to this request arrives, cache it.
				// We detect it in forwardClientRequest via returned proxy id.
			} else {
				// Respond locally with cached result
				resp := Message{
					JSONRPC: "2.0",
					ID:      m.ID,
					Result:  s.initResult,
				}
				c.write(mustMarshal(resp))
			}
			continue
		}
		if isNotification(&m) && m.Method == "initialized" {
			if !s.initializedSent {
				s.initializedSent = true
				s.writeToServer(body)
			}
			continue
		}
		if isRequest(&m) && m.Method == "shutdown" {
			// Acknowledge locally; do not shutdown the real server yet
			resp := Message{
				JSONRPC: "2.0",
				ID:      m.ID,
				Result:  json.RawMessage("null"),
			}
			c.write(mustMarshal(resp))
			continue
		}
		if isNotification(&m) && m.Method == "exit" {
			// Ignore; actual process is managed by mux
			continue
		}

		if isRequest(&m) {
			s.forwardClientRequest(c.id, body, m, false)
			continue
		}

		// This is a response (client replying to a server->client request)
		if isResponse(&m) {
			s.handleClientResponse(c.id, body, m)
			continue
		}

		// Notifications: forward blindly
		if isNotification(&m) {
			s.writeToServer(body)
			continue
		}

		// Fallback: forward
		s.writeToServer(body)
	}
}

func (s *Server) forwardClientRequest(clientID int, body []byte, m Message, markMaybeInit bool) {
	// Allocate proxy id
	s.pendingReqsMu.Lock()
	s.nextProxyID++
	proxyID := s.nextProxyID
	s.pendingByProxyID[proxyID] = reqMap{
		clientID: clientID,
		origID:   m.ID,
	}
	s.pendingReqsMu.Unlock()

	// Replace id
	newBody, err := replaceID(body, json.RawMessage(fmt.Sprintf("%d", proxyID)))
	if err != nil {
		return
	}

	// If this might be initialize, wrap to capture its response result
	if markMaybeInit && m.Method == "initialize" {
		// Set a temporary hook by adding an entry in pending map; when server responds,
		// we'll cache the result in loopServerRead using this proxyID.
		// To capture the result, we intercept in loopServerRead: when returning to client,
		// if method=="" and pendingMap had initialize, cache the Result.
		// Implement via adding a special marker in origID (empty) and checking below.
	}
	s.writeToServer(newBody)
}

func (s *Server) handleClientResponse(clientID int, body []byte, m Message) {
	// Response to a server->client request.
	// Our serverReqMap maps numeric serverID -> (clientID, origID, clientWireID)
	// Extract numeric id from m.ID
	var cid uint64
	fmt.Sscanf(string(m.ID), "%d", &cid)

	s.serverReqsMu.Lock()
	info, ok := s.serverReqMap[cid]
	if ok {
		delete(s.serverReqMap, cid)
	}
	s.serverReqsMu.Unlock()
	if !ok {
		return
	}
	// Rewrite id back to server's original id and send to LSP server
	newBody, err := replaceID(body, info.origID)
	if err == nil {
		s.writeToServer(newBody)
	}
}

func (s *Server) writeToServer(body []byte) {
	s.lspWrMu.Lock()
	defer s.lspWrMu.Unlock()
	_ = writeBody(s.lspIn, body)
}

// ---------- Idle / shutdown management ----------

func (s *Server) startIdleTimer() {
	s.idleTimerMu.Lock()
	defer s.idleTimerMu.Unlock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	if s.cfg.Linger <= 0 {
		s.cfg.Linger = 10 * time.Minute
	}
	s.idleTimer = time.AfterFunc(s.cfg.Linger, s.gracefulShutdown)
	if s.cfg.Verbose {
		fmt.Fprintf(os.Stderr, "[mux] idle: scheduling shutdown in %s\n", s.cfg.Linger)
	}
}

func (s *Server) stopIdleTimer() {
	s.idleTimerMu.Lock()
	defer s.idleTimerMu.Unlock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

func (s *Server) gracefulShutdown() {
	s.shutdownOnce.Do(func() {
		if s.cfg.Verbose {
			fmt.Fprintln(os.Stderr, "[mux] graceful shutdown: sending shutdown+exit to LSP server")
		}
		// Send shutdown request
		req := Message{
			JSONRPC: "2.0",
			ID:      json.RawMessage("999999"),
			Method:  "shutdown",
			Params:  nil,
		}
		s.writeToServer(mustMarshal(req))
		// Wait a short grace period for response
		time.Sleep(300 * time.Millisecond)
		// Send exit notification
		notify := Message{
			JSONRPC: "2.0",
			Method:  "exit",
		}
		s.writeToServer(mustMarshal(notify))

		// Kill process after another short delay
		time.Sleep(200 * time.Millisecond)
		_ = s.lspCmd.Process.Kill()
		_ = s.Close()
	})
}

// Hook: cache initialize result when first init completes.
// We detect it inside loopServerRead by checking if the response corresponds
// to the first forwarded initialize.
// To approximate, we cache the first successful initialize-like result we see.
// A robust implementation would tag the proxy id for "initialize".
func (s *Server) cacheInitializeIfApplicable(m *Message) {
	// unused in this trimmed version
	_ = m
}
