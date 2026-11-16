package toolserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

var progressInterval = 30 * time.Second

type ServedTool struct {
	Name, Description string
	Schema            json.RawMessage
	ReadOnly          bool
}

type CallHandler func(ctx context.Context, toolUseID, name string, args json.RawMessage) CallResult

type ToolServer struct {
	tools  []ServedTool
	secret string
	h      CallHandler
	srv    *http.Server
	ln     net.Listener
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func Serve(tools []ServedTool, secret string, h CallHandler) (*ToolServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	base, cancel := context.WithCancel(context.Background())
	s := &ToolServer{tools: tools, secret: secret, h: h, ln: ln, cancel: cancel}
	s.srv = &http.Server{
		Handler:     http.HandlerFunc(s.handle),
		BaseContext: func(net.Listener) context.Context { return base },
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.srv.Serve(ln)
	}()
	return s, nil
}

func (s *ToolServer) URL() string {
	return "http://" + s.ln.Addr().String() + "/mcp"
}

func (s *ToolServer) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.srv.Close()
	s.wg.Wait()
}

func (s *ToolServer) enter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.wg.Add(1)
	return true
}

func (s *ToolServer) handle(w http.ResponseWriter, r *http.Request) {
	if !s.enter() {
		http.Error(w, "closed", http.StatusServiceUnavailable)
		return
	}
	defer s.wg.Done()
	want := "Bearer " + s.secret
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var req RPCMsg
	if json.Unmarshal(body, &req) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		v := ProtocolVersion
		if SupportedVersions[p.ProtocolVersion] {
			v = p.ProtocolVersion
		}
		w.Header().Set("Mcp-Session-Id", "axon")
		writeJSON(w, req.ID, map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": ClientName, "version": "0"},
		}, nil)
	case "ping":
		writeJSON(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		writeJSON(w, req.ID, map[string]any{"tools": s.listTools()}, nil)
	case "tools/call":
		s.call(w, r, req)
	default:
		writeJSON(w, req.ID, nil, &RPCError{Code: -32601, Message: "method not found"})
	}
}

func (s *ToolServer) listTools() []map[string]any {
	out := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		m := map[string]any{"name": t.Name, "description": t.Description, "inputSchema": schema}
		if t.ReadOnly {
			m["annotations"] = map[string]any{"readOnlyHint": true}
		}
		out = append(out, m)
	}
	return out
}

func rpcBytes(id json.RawMessage, result any, rerr *RPCError) []byte {
	m := map[string]any{"jsonrpc": "2.0", "id": id}
	if rerr != nil {
		m["error"] = rerr
	} else {
		m["result"] = result
	}
	b, _ := json.Marshal(m)
	return b
}

func writeJSON(w http.ResponseWriter, id json.RawMessage, result any, rerr *RPCError) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(rpcBytes(id, result, rerr))
}

func (s *ToolServer) call(w http.ResponseWriter, r *http.Request, req RPCMsg) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			ToolUseID     string          `json:"claudecode/toolUseId"`
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if json.Unmarshal(req.Params, &p) != nil || p.Name == "" {
		writeJSON(w, req.ID, nil, &RPCError{Code: -32602, Message: "invalid params"})
		return
	}
	ctx := r.Context()
	done := make(chan CallResult, 1)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		done <- s.h(ctx, p.Meta.ToolUseID, p.Name, p.Arguments)
	}()

	token := p.Meta.ProgressToken
	streaming := len(token) > 0 && string(token) != "null"
	var flusher http.Flusher
	var tick <-chan time.Time
	if streaming {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ = w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
		t := time.NewTicker(progressInterval)
		defer t.Stop()
		tick = t.C
	}
	n := 0
	for {
		select {
		case res := <-done:
			if res.Content == nil {
				res.Content = []Content{}
			}
			if streaming {
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", rpcBytes(req.ID, wireResult(res), nil))
				if flusher != nil {
					flusher.Flush()
				}
			} else {
				writeJSON(w, req.ID, wireResult(res), nil)
			}
			return
		case <-tick:
			n++
			b, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "method": "notifications/progress",
				"params": map[string]any{"progressToken": token, "progress": n},
			})
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		case <-ctx.Done():
			return
		}
	}
}

// wireResult encodes a result with only the fields each content type has:
// Claude Code validates results and rejects a null structuredContent.
func wireResult(res CallResult) map[string]any {
	content := make([]map[string]any, 0, len(res.Content))
	for _, c := range res.Content {
		switch c.Type {
		case "image", "audio":
			content = append(content, map[string]any{"type": c.Type, "data": c.Data, "mimeType": c.MIMEType})
		default:
			content = append(content, map[string]any{"type": "text", "text": c.Text})
		}
	}
	out := map[string]any{"content": content}
	if res.IsError {
		out["isError"] = true
	}
	if len(res.StructuredContent) > 0 && string(res.StructuredContent) != "null" {
		out["structuredContent"] = res.StructuredContent
	}
	return out
}
