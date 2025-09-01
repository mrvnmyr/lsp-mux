package mux

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// JSON-RPC/LSP minimal message structure
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// readFrame reads one LSP frame from r and returns the raw body.
func readFrame(r *bufio.Reader) ([]byte, error) {
	var contentLen = -1
	// Read headers
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "content-length:") {
			var n int
			_, _ = fmt.Sscanf(line, "Content-Length: %d", &n)
			contentLen = n
		}
		if line == "\r\n" {
			break
		}
	}
	if contentLen < 0 {
		return nil, fmt.Errorf("missing Content-Length header")
	}
	body := make([]byte, contentLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func writeBody(w io.Writer, body []byte) error {
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

func mustMarshal(v any) []byte {
	bs, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return bs
}

// replaceID returns a copy of msg with ID replaced.
func replaceID(msg []byte, newID json.RawMessage) ([]byte, error) {
	var m Message
	if err := json.Unmarshal(msg, &m); err != nil {
		return nil, err
	}
	m.ID = newID
	return json.Marshal(m)
}

func isRequest(m *Message) bool {
 	return m.Method != "" && len(m.ID) > 0
}

func isNotification(m *Message) bool {
	return m.Method != "" && len(m.ID) == 0
}

func isResponse(m *Message) bool {
	return m.Method == "" && (len(m.Result) > 0 || len(m.Error) > 0) && len(m.ID) > 0
}
