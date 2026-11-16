// Package claude is a scripted stand-in for the "claude" program in
// headless stream-json mode, for tests. See README.md.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	model     = "claude-haiku-5-5"
	version   = "2.1.323"
	toolUseID = "claudecode/toolUseId"
)

type call struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type message struct {
	Text     []string `json:"text"`
	Thinking bool     `json:"thinking"`
	Calls    []call   `json:"calls"`
	Hang     bool     `json:"hang"`
}

type script struct {
	Turns         [][]message `json:"turns"`
	Overage       bool        `json:"overage"`
	FailInit      bool        `json:"failInit"`
	NotLoggedIn   bool        `json:"notLoggedIn"`
	ExitAfter     int         `json:"exitAfter"`
	RejectResume  bool        `json:"rejectResume"`
	RetryNotice   bool        `json:"retryNotice"`
	CostPerTurn   float64     `json:"costPerTurn"`
	CarryOver     bool        `json:"carryOver"`
	ProgressToken bool        `json:"progressToken"`
	APIKeySource  string      `json:"apiKeySource"`
}

type usage struct {
	In, Out, Think int
}

func (u usage) json() map[string]any {
	return map[string]any{"input_tokens": u.In, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0, "output_tokens": u.Out,
		"output_tokens_details": map[string]any{"thinking_tokens": u.Think}}
}

type state struct {
	sc       script
	out      io.Writer
	stderr   io.Writer
	outMu    sync.Mutex
	logPath  string
	logMu    sync.Mutex
	sid      string
	server   string
	mcp      *mcpClient
	tools    map[string]bool
	ctx      context.Context
	msgCount int
	local    int
	tokens   int // tokens of this process's model messages
	prior    int // tokens of the turns before this process, with carryOver
	started  bool
	initLine map[string]any
}

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("testclaude", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Bool("p", false, "")
	inFmt := fs.String("input-format", "", "")
	outFmt := fs.String("output-format", "", "")
	fs.Bool("verbose", false, "")
	fs.Bool("include-partial-messages", false, "")
	fs.String("model", "", "")
	fs.String("effort", "", "")
	fs.String("system-prompt", "", "")
	fs.String("tools", "", "")
	fs.String("setting-sources", "", "")
	fs.Bool("strict-mcp-config", false, "")
	fs.Bool("disable-slash-commands", false, "")
	fs.String("system-prompt-snapshot", "", "")
	mcpConfig := fs.String("mcp-config", "", "")
	fs.String("allowedTools", "", "")
	resume := fs.String("resume", "", "")
	fs.Bool("no-session-persistence", false, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || *inFmt != "stream-json" || *outFmt != "stream-json" {
		fmt.Fprintln(stderr, "testclaude: only the stream-json mode is implemented")
		return 2
	}

	s := &state{out: stdout, stderr: stderr, logPath: os.Getenv("TESTCLAUDE_LOG"), tools: map[string]bool{}}
	if p := os.Getenv("TESTCLAUDE_SCRIPT"); p != "" {
		b, err := os.ReadFile(p)
		if err == nil {
			err = json.Unmarshal(b, &s.sc)
		}
		if err != nil {
			fmt.Fprintln(stderr, "testclaude: script:", err)
			return 2
		}
	}
	cwd, _ := os.Getwd()
	var names []string
	for _, kv := range os.Environ() {
		names = append(names, strings.SplitN(kv, "=", 2)[0])
	}
	sort.Strings(names)
	s.log(map[string]any{"event": "start", "pid": os.Getpid(), "args": args, "env": names, "cwd": cwd})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.ctx = ctx
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)
	go func() {
		select {
		case <-sigs:
			s.log(map[string]any{"event": "interrupt"})
			cancel()
		case <-ctx.Done():
		}
	}()

	lines := make(chan string)
	go func() {
		defer close(lines)
		r := bufio.NewReader(stdin)
		for {
			l, err := r.ReadString('\n')
			if l = strings.TrimRight(l, "\r\n"); l != "" {
				s.log(map[string]any{"event": "stdin", "line": l})
				select {
				case lines <- l:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	code := s.run(*resume, *mcpConfig, lines, stderr)
	s.log(map[string]any{"event": "exit", "code": code})
	return code
}

func (s *state) run(resume, mcpConfig string, lines chan string, stderr io.Writer) int {
	if resume != "" && s.sc.RejectResume {
		fmt.Fprintln(stderr, "No conversation found with session ID:", resume)
		s.sid = resume
		s.emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": "", "errors": []string{"No conversation found with session ID: " + resume}, "session_id": resume})
		return 1
	}
	s.sid = resume
	if s.sid == "" {
		s.sid = uuid()
	}
	if s.sc.NotLoggedIn {
		select {
		case <-lines:
		case <-s.ctx.Done():
			return 0
		}
		s.emit(map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": "Not logged in · Please run /login", "session_id": s.sid})
		return 1
	}

	status := "failed"
	if !s.sc.FailInit {
		if err := s.connect(mcpConfig); err != nil {
			fmt.Fprintln(stderr, "testclaude: mcp:", err)
		} else {
			status = "connected"
		}
	}
	var toolNames, readOnly []string
	for n, ro := range s.tools {
		toolNames = append(toolNames, "mcp__"+s.server+"__"+n)
		if ro {
			readOnly = append(readOnly, n)
		}
	}
	sort.Strings(toolNames)
	sort.Strings(readOnly)
	s.log(map[string]any{"event": "mcp", "server": s.server, "status": status, "tools": toolNames, "readOnly": readOnly})
	s.initLine = map[string]any{"type": "system", "subtype": "init", "tools": toolNames,
		"mcp_servers": []any{map[string]any{"name": s.server, "status": status, "source": "dynamic"}}, "model": model,
		"permissionMode": "default", "apiKeySource": s.apiKeySource(), "claude_code_version": version}

	turns := 0
	for {
		var first string
		select {
		case <-s.ctx.Done():
			return 0
		case l, ok := <-lines:
			if !ok {
				return 0
			}
			first = l
		}
		code, done := s.turn(first, lines, turns)
		if done {
			return code
		}
		turns++
	}
}

func (s *state) apiKeySource() string {
	if s.sc.APIKeySource != "" {
		return s.sc.APIKeySource
	}
	return "none"
}

func (s *state) turnIndex() int {
	if s.logPath == "" {
		s.local++
		return s.local - 1
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	b, _ := os.ReadFile(s.logPath)
	return bytes.Count(b, []byte(`"event":"turn"`))
}

func (s *state) turn(first string, lines chan string, done int) (int, bool) {
	idx := s.turnIndex()
	s.log(map[string]any{"event": "turn", "n": idx})
	cwd, _ := os.Getwd()
	s.initLine["cwd"] = cwd
	s.emit(s.initLine)
	msgs := []message{{Text: []string{"ok"}}}
	if idx < len(s.sc.Turns) {
		msgs = s.sc.Turns[idx]
	}
	if s.sc.RetryNotice && idx == 0 {
		s.emit(map[string]any{"type": "system", "subtype": "api_retry", "attempt": 1, "max_retries": 10, "retry_delay_ms": 500, "error_status": 529, "error": "overloaded", "session_id": s.sid})
	}
	if !s.started {
		s.started = true
		if s.sc.CarryOver {
			s.prior = s.turnTokens(idx)
		}
	}
	var total usage
	var last string
	for _, m := range msgs {
		if s.ctx.Err() != nil {
			break
		}
		if s.mcp == nil {
			m.Calls = nil
		}
		u, text := s.modelMessage(m)
		total.In += u.In
		total.Out += u.Out
		total.Think += u.Think
		s.tokens += u.In + u.Out
		if len(m.Text) > 0 {
			last = text
		}
		s.msgCount++
		if len(m.Calls) > 0 {
			for drained := false; !drained; {
				select {
				case _, ok := <-lines:
					drained = !ok
				default:
					drained = true
				}
			}
		}
		if s.sc.ExitAfter > 0 && s.msgCount >= s.sc.ExitAfter {
			fmt.Fprintln(s.stderr, "testclaude: exiting as scripted")
			return 1, true
		}
	}
	turnsCost := done + 1
	if s.sc.CarryOver {
		turnsCost = idx + 1
	}
	res := map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": last, "stop_reason": "end_turn",
		"terminal_reason": "completed", "num_turns": len(msgs), "duration_ms": 0, "total_cost_usd": s.sc.CostPerTurn * float64(turnsCost), "usage": total.json(), "session_id": s.sid,
		"modelUsage": map[string]any{model: map[string]any{"inputTokens": s.prior + s.tokens, "outputTokens": 0, "cacheReadInputTokens": 0, "cacheCreationInputTokens": 0, "costUSD": s.sc.CostPerTurn * float64(turnsCost)}}}
	if s.ctx.Err() != nil {
		res["subtype"] = "error_during_execution"
		res["is_error"] = true
		delete(res, "result")
		res["stop_reason"] = nil
		res["terminal_reason"] = "aborted_streaming"
		res["errors"] = []string{"[ede_diagnostic] result_type=user last_content_type=n/a stop_reason=null"}
		res["total_cost_usd"] = s.sc.CostPerTurn * float64(done)
		res["usage"] = usage{}.json()
		s.emit(res)
		return 0, true
	}
	s.emit(res)
	return 0, false
}

// turnTokens is the token count of the first n turns of the script; every
// model message uses 100 input and 20 output tokens.
func (s *state) turnTokens(n int) int {
	msgs := 0
	for i := range n {
		if i < len(s.sc.Turns) {
			msgs += len(s.sc.Turns[i])
		} else {
			msgs++
		}
	}
	return msgs * 120
}

func (s *state) modelMessage(m message) (usage, string) {
	id := fmt.Sprintf("msg_%s_%03d", s.sid[:8], s.msgCount)
	u := usage{In: 100, Out: 20}
	base := func(content []any, stop any) map[string]any {
		return map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": content, "stop_reason": stop, "stop_sequence": nil, "usage": u.json()}
	}
	event := func(ev map[string]any) {
		s.emit(map[string]any{"type": "stream_event", "event": ev, "parent_tool_use_id": nil})
	}
	assistant := func(block map[string]any) {
		s.emit(map[string]any{"type": "assistant", "message": base([]any{block}, nil), "parent_tool_use_id": nil})
	}
	s.emit(map[string]any{"type": "system", "subtype": "status", "status": "requesting"})
	first := s.msgCount == 0
	idx := 0
	start := base([]any{}, nil)
	start["usage"] = usage{In: u.In, Out: 1}.json()
	event(map[string]any{"type": "message_start", "message": start})
	if m.Thinking {
		u.Think = 30
		event(map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}})
		s.emit(map[string]any{"type": "system", "subtype": "thinking_tokens", "estimated_tokens": 10, "estimated_tokens_delta": 10})
		event(map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "thinking_delta", "thinking": "", "estimated_tokens": 10}})
		s.emit(map[string]any{"type": "system", "subtype": "thinking_tokens", "estimated_tokens": 30, "estimated_tokens_delta": 20})
		event(map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "signature_delta", "signature": "c2ln"}})
		assistant(map[string]any{"type": "thinking", "thinking": "", "signature": "c2ln"})
		event(map[string]any{"type": "content_block_stop", "index": idx})
		idx++
	}
	text := strings.Join(m.Text, "")
	if len(m.Text) > 0 {
		event(map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "text", "text": ""}})
		for _, c := range m.Text {
			event(map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "text_delta", "text": c}})
		}
		if m.Hang {
			<-s.ctx.Done()
			return u, text
		}
		assistant(map[string]any{"type": "text", "text": text})
		event(map[string]any{"type": "content_block_stop", "index": idx})
		idx++
	}
	d := s.newDispatcher()
	for _, c := range m.Calls {
		args := c.Args
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		tid := "toolu_" + randHex(8)
		name := "mcp__" + s.server + "__" + c.Name
		event(map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "tool_use", "id": tid, "name": name, "input": map[string]any{}}})
		event(map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": ""}})
		event(map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(args)}})
		assistant(map[string]any{"type": "tool_use", "id": tid, "name": name, "input": args})
		event(map[string]any{"type": "content_block_stop", "index": idx})
		d.add(pending{id: tid, name: c.Name, args: args})
		idx++
	}
	stop := "end_turn"
	if len(m.Calls) > 0 {
		stop = "tool_use"
	}
	event(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": u.json()})
	event(map[string]any{"type": "message_stop"})
	status := "allowed"
	if s.sc.Overage {
		status = "allowed_warning"
	}
	if first {
		s.emit(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": status, "resetsAt": time.Now().Add(time.Hour).Unix(),
			"rateLimitType": "five_hour", "isUsingOverage": s.sc.Overage}})
	}
	d.wait()
	return u, text
}

type pending struct {
	id, name string
	args     json.RawMessage
}

type dispatcher struct {
	s    *state
	ch   chan pending
	done chan struct{}
}

func (s *state) newDispatcher() *dispatcher {
	d := &dispatcher{s: s, ch: make(chan pending, 64), done: make(chan struct{})}
	go func() {
		defer close(d.done)
		var wg sync.WaitGroup
		defer wg.Wait()
		for p := range d.ch {
			if s.tools[p.name] {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.callTool(p)
				}()
				continue
			}
			wg.Wait()
			s.callTool(p)
		}
	}()
	return d
}

func (d *dispatcher) add(p pending) { d.ch <- p }

func (d *dispatcher) wait() {
	close(d.ch)
	<-d.done
}

func (s *state) callTool(p pending) {
	params := map[string]any{"name": p.name, "arguments": p.args}
	meta := map[string]any{toolUseID: p.id}
	if s.sc.ProgressToken {
		meta["progressToken"] = p.id
	}
	params["_meta"] = meta
	raw, err := s.mcp.rpc(s.ctx, "tools/call", params)
	if s.ctx.Err() != nil {
		return
	}
	var res struct {
		Content json.RawMessage `json:"content"`
		IsError bool            `json:"isError"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &res)
	}
	if err != nil {
		res.IsError = true
		res.Content, _ = json.Marshal([]any{map[string]any{"type": "text", "text": err.Error()}})
	}
	if len(res.Content) == 0 {
		res.Content = json.RawMessage("[]")
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		MIMEType string `json:"mimeType"`
	}
	json.Unmarshal(res.Content, &parts)
	var text, images []string
	for _, c := range parts {
		if c.Type == "image" {
			images = append(images, c.MIMEType)
			continue
		}
		text = append(text, c.Text)
	}
	s.log(map[string]any{"event": "call", "id": p.id, "name": p.name, "args": p.args, "isError": res.IsError, "text": strings.Join(text, ""), "images": images})
	block := map[string]any{"type": "tool_result", "tool_use_id": p.id, "content": res.Content}
	if res.IsError {
		block["is_error"] = true
	}
	s.emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{block}}, "parent_tool_use_id": nil})
}

func (s *state) emit(v map[string]any) {
	if _, ok := v["session_id"]; !ok {
		v["session_id"] = s.sid
	}
	v["uuid"] = uuid()
	b, _ := json.Marshal(v)
	s.outMu.Lock()
	defer s.outMu.Unlock()
	s.out.Write(append(b, '\n'))
}

func (s *state) log(v map[string]any) {
	if s.logPath == "" {
		return
	}
	b, _ := json.Marshal(v)
	s.logMu.Lock()
	defer s.logMu.Unlock()
	f, err := os.OpenFile(s.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func uuid() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type mcpClient struct {
	url     string
	headers map[string]string
	session string
	next    atomic.Int64
	hc      http.Client
}

func (s *state) connect(config string) error {
	var cfg struct {
		Servers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return err
	}
	if len(cfg.Servers) != 1 {
		return errors.New("mcp-config must name exactly one server")
	}
	for name, e := range cfg.Servers {
		s.server = name
		if e.Type != "http" {
			return errors.New("only http servers are implemented")
		}
		c := &mcpClient{url: e.URL, headers: map[string]string{}}
		for k, v := range e.Headers {
			c.headers[k] = os.Expand(v, os.Getenv)
		}
		s.mcp = c
	}
	ctx := s.ctx
	c := s.mcp
	c.rpc(ctx, "server/discover", map[string]any{})
	_, err := c.rpc(ctx, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "claude-code", "version": version}})
	if err != nil {
		s.mcp = nil
		return err
	}
	if err := c.notify(ctx, "notifications/initialized"); err != nil {
		s.mcp = nil
		return err
	}
	c.get(ctx)
	raw, err := c.rpc(ctx, "tools/list", map[string]any{})
	if err != nil {
		s.mcp = nil
		return err
	}
	var list struct {
		Tools []struct {
			Name        string `json:"name"`
			Annotations struct {
				ReadOnly bool `json:"readOnlyHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		s.mcp = nil
		return err
	}
	for _, t := range list.Tools {
		s.tools[t.Name] = t.Annotations.ReadOnly
	}
	return nil
}

func (c *mcpClient) request(ctx context.Context, method string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
	} else {
		req.Header.Set("Accept", "text/event-stream")
	}
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	return c.hc.Do(req)
}

func (c *mcpClient) notify(ctx context.Context, method string) error {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	resp, err := c.request(ctx, http.MethodPost, b)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: HTTP %d", method, resp.StatusCode)
	}
	return nil
}

func (c *mcpClient) get(ctx context.Context) {
	resp, err := c.request(ctx, http.MethodGet, nil)
	if err == nil {
		resp.Body.Close()
	}
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (r rpcResponse) value() (json.RawMessage, error) {
	if r.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", r.Error.Code, r.Error.Message)
	}
	return r.Result, nil
}

func (c *mcpClient) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := strconv.FormatInt(c.next.Add(1), 10)
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.Number(id), "method": method, "params": params})
	resp, err := c.request(ctx, http.MethodPost, b)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("%s: HTTP %d", method, resp.StatusCode)
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.session = sid
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		var r rpcResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return nil, err
		}
		return r.value()
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var data []string
	flush := func() (rpcResponse, bool) {
		defer func() { data = nil }()
		var r rpcResponse
		if len(data) == 0 || json.Unmarshal([]byte(strings.Join(data, "\n")), &r) != nil {
			return r, false
		}
		return r, string(r.ID) == id
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if r, ok := flush(); ok {
				return r.value()
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if r, ok := flush(); ok {
		return r.value()
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New(method + ": event stream ended without a response")
}
