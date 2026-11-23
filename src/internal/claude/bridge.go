package claude

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/atb00ker/claude-max-to-api/src/internal/logging"
)

// BridgeServerName is the MCP server name under which client-supplied
// (OpenAI "tools") functions are exposed to Claude. Claude sees them as
// mcp__<BridgeServerName>__<function>.
const BridgeServerName = "oai"

// BridgeToolPrefix is the prefix Claude Code puts in front of bridged tools.
const BridgeToolPrefix = "mcp__" + BridgeServerName + "__"

// Tool is a client-side function definition exposed to Claude through MCP.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Bridge is a tiny MCP server (Streamable HTTP transport, JSON responses) on a
// loopback port. Each request registers its tool list under an unguessable
// path. Tool calls are never executed here: the runner sees the tool_use in
// Claude's output stream, returns it to the OpenAI client as tool_calls and
// stops the claude process. Any tools/call that reaches the bridge simply
// waits until that happens.
type Bridge struct {
	listener net.Listener
	server   *http.Server
	baseURL  string

	mu   sync.Mutex
	sets map[string]*toolSet
}

type toolSet struct {
	tools []Tool
	done  chan struct{}
}

// NewBridge starts the bridge on 127.0.0.1 with a random port.
func NewBridge() (*Bridge, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := &Bridge{listener: ln, baseURL: "http://" + ln.Addr().String(), sets: map[string]*toolSet{}}
	srv := &http.Server{Handler: http.HandlerFunc(b.serveHTTP), ReadHeaderTimeout: 10 * time.Second}
	b.server = srv
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error(logging.Event("bridge", "serve", "failed"), logging.Err(err))
		}
	}()
	return b, nil
}

// Close stops the bridge and releases any tool calls still waiting.
func (b *Bridge) Close() error {
	b.mu.Lock()
	for id, ts := range b.sets {
		close(ts.done)
		delete(b.sets, id)
	}
	b.mu.Unlock()
	return b.server.Close()
}

// Register exposes tools to one claude run. It returns the MCP URL and a
// function that must be called when the run ends.
func (b *Bridge) Register(tools []Tool) (string, func()) {
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	id := hex.EncodeToString(buf)
	ts := &toolSet{tools: tools, done: make(chan struct{})}
	b.mu.Lock()
	b.sets[id] = ts
	b.mu.Unlock()
	var once sync.Once
	return b.baseURL + "/mcp/" + id, func() {
		once.Do(func() {
			b.mu.Lock()
			_, live := b.sets[id]
			delete(b.sets, id)
			b.mu.Unlock()
			if live {
				close(ts.done)
			}
		})
	}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (b *Bridge) serveHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutPrefix(r.URL.Path, "/mcp/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	b.mu.Lock()
	ts := b.sets[id]
	b.mu.Unlock()
	if ts == nil {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		return
	default:
		// No server-initiated SSE stream.
		w.Header().Set("Allow", "POST, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	trimmed := strings.TrimSpace(string(body))
	var reqs []rpcRequest
	batch := strings.HasPrefix(trimmed, "[")
	if batch {
		err = json.Unmarshal(body, &reqs)
	} else {
		var one rpcRequest
		err = json.Unmarshal(body, &one)
		reqs = []rpcRequest{one}
	}
	if err != nil {
		writeJSON(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
		return
	}

	var responses []rpcResponse
	for _, req := range reqs {
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue // notification: no response
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.ProtocolVersion == "" {
				p.ProtocolVersion = "2025-06-18"
			}
			resp.Result = map[string]any{
				"protocolVersion": p.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo":      map[string]any{"name": "openai-client-tools", "version": "1.0.0"},
				"instructions":    "These tools are provided by the API client. Call them when they help answer the user.",
			}
		case "ping":
			resp.Result = map[string]any{}
		case "tools/list":
			resp.Result = map[string]any{"tools": ts.tools}
		case "tools/call":
			// The runner stops claude as soon as the tool_use is complete, so
			// this only needs to hold the call open until then.
			select {
			case <-ts.done:
			case <-r.Context().Done():
				return
			}
			resp.Result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": "Tool execution is handled by the API client."}},
				"isError": true,
			}
		default:
			resp.Error = &rpcError{-32601, "method not found: " + req.Method}
		}
		responses = append(responses, resp)
	}

	if len(responses) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if batch {
		writeJSON(w, responses)
	} else {
		writeJSON(w, responses[0])
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
