package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cheetahbyte/soma"
	"github.com/cheetahbyte/soma/config"
	"github.com/cheetahbyte/soma/provider"
	"github.com/cheetahbyte/soma/provider/wire"
)

const DefaultBaseURL = "https://api.openai.com/v1"

// OpenAI rejects prompt cache keys longer than 64 characters.
const maxCacheKey = 64

func init() {
	provider.Register("openai-responses", func(cfg *config.Config, p config.ProviderConfig) (soma.Provider, error) {
		key, err := cfg.Key(p)
		if err != nil {
			return nil, err
		}
		return New(p.Name, p.BaseURL, key, p.Headers, provider.ModelInfoFunc(cfg, p)), nil
	})
}

type Client struct {
	name    string
	api     string
	baseURL string
	key     string
	headers map[string]string
	info    func(id string) config.ModelInfo
	http    *http.Client
	idle    time.Duration
	tokens  TokenSource // set in Codex mode
}

func New(name, baseURL, key string, headers map[string]string, info func(id string) config.ModelInfo) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	client, idle := wire.NewHTTPClient()
	return &Client{
		name:    name,
		api:     "openai-responses",
		baseURL: strings.TrimRight(baseURL, "/"),
		key:     key,
		headers: headers,
		info:    info,
		http:    client,
		idle:    idle,
	}
}

// request is the body without its input, which requestBody encodes item
// by item so that a long session is not held in memory several times over.
type request struct {
	Model             string     `json:"model"`
	Instructions      string     `json:"instructions,omitempty"`
	Tools             []tool     `json:"tools,omitempty"`
	Stream            bool       `json:"stream"`
	Store             bool       `json:"store"`
	PromptCacheKey    string     `json:"prompt_cache_key,omitempty"`
	Reasoning         *reasoning `json:"reasoning,omitempty"`
	Include           []string   `json:"include,omitempty"`
	Text              *textOpts  `json:"text,omitempty"`
	ToolChoice        string     `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool      `json:"parallel_tool_calls,omitempty"`
}

type textOpts struct {
	Verbosity string `json:"verbosity"`
}

type tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type reasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary,omitempty"`
}

func (c *Client) Stream(ctx context.Context, req soma.Request, onDelta func(soma.Delta)) (_ soma.Message, err error) {
	var cred codexCredential
	defer func() { err = wire.Redact(err, c.key, cred.access) }()
	head, err := c.requestHead(req)
	if err != nil {
		return soma.Message{}, err
	}
	reqCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	if c.tokens != nil {
		if cred, err = c.token(ctx, ""); err != nil {
			return c.tokenFailure(ctx, req, err)
		}
	}
	resp, err := c.send(reqCtx, head, req, cred)
	if err == nil && resp.StatusCode == http.StatusUnauthorized && c.tokens != nil {
		// The token may have been revoked or rotated before its stated
		// expiry; one forced refresh recovers, a second 401 is reported.
		resp.Body.Close()
		if cred, err = c.token(ctx, cred.access); err != nil {
			return c.tokenFailure(ctx, req, err)
		}
		resp, err = c.send(reqCtx, head, req, cred)
	}
	if err != nil {
		if ctx.Err() != nil {
			return abortedMessage(req.Model), ctx.Err()
		}
		return soma.Message{}, &soma.ProviderError{Kind: soma.ErrNetwork, Message: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if rerr != nil && ctx.Err() != nil {
			return abortedMessage(req.Model), ctx.Err()
		}
		if c.tokens != nil {
			if e := codexUsageLimit(resp.StatusCode, data); e != nil {
				return soma.Message{}, e
			}
		}
		return soma.Message{}, wire.StatusError(resp, data)
	}

	s := newStreamState(req.Model, c.api, c.model(req.Model), onDelta)
	timer := time.AfterFunc(c.idle, func() { cancel(wire.ErrStreamIdle) })
	err = wire.ReadSSE(&wire.IdleReader{R: resp.Body, Timer: timer, Idle: c.idle}, s.handle)
	timer.Stop()
	if ctx.Err() != nil {
		return s.partial(), ctx.Err()
	}
	if errors.Is(context.Cause(reqCtx), wire.ErrStreamIdle) {
		return soma.Message{}, wire.NetworkError(fmt.Errorf("the stream sent nothing for %s", c.idle))
	}
	if err != nil {
		return soma.Message{}, err
	}
	return s.final()
}

func (c *Client) send(ctx context.Context, head []byte, req soma.Request, cred codexCredential) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(), requestBody(head, req, c.source()))
	if err != nil {
		return nil, err
	}
	httpReq.GetBody = func() (io.ReadCloser, error) { return requestBody(head, req, c.source()), nil }
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.key)
	}
	httpReq.Header.Set("User-Agent", wire.UserAgent())
	if c.tokens != nil {
		httpReq.Header.Set("originator", "harness")
	}
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}
	if c.tokens != nil {
		c.codexHeaders(httpReq.Header, req, cred)
	}
	return c.http.Do(httpReq)
}

func (c *Client) url() string {
	if c.tokens != nil {
		return codexURL(c.baseURL)
	}
	return c.baseURL + "/responses"
}

func abortedMessage(model string) soma.Message {
	return soma.Message{Role: soma.RoleAssistant, Model: model, StopReason: soma.StopAborted}
}

func (c *Client) model(id string) config.ModelInfo {
	if c.info == nil {
		return config.ModelInfo{ID: id}
	}
	return c.info(id)
}

// requestHead encodes every field of the body except the input.
func (c *Client) requestHead(req soma.Request) ([]byte, error) {
	r := request{
		Model:          req.Model,
		Instructions:   req.System,
		Stream:         true,
		Store:          false,
		PromptCacheKey: clampRunes(req.SessionID, maxCacheKey),
	}
	for _, t := range req.Tools {
		params := t.Schema
		if len(bytes.TrimSpace(params)) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		r.Tools = append(r.Tools, tool{Type: "function", Name: t.Name, Description: t.Description, Parameters: params})
	}
	info := c.model(req.Model)
	if effort, ok := info.WireEffort(req.Thinking); ok {
		// At off a mapped effort such as "none" is sent alone: there is no
		// reasoning to summarize or replay.
		if info.Level(req.Thinking) == "off" {
			r.Reasoning = &reasoning{Effort: effort}
		} else {
			r.Reasoning = &reasoning{Effort: effort, Summary: "auto"}
			r.Include = []string{"reasoning.encrypted_content"}
		}
	}
	if c.tokens != nil {
		codexBody(&r)
		// Pi's Codex client: at off, a reasoning model gets effort none, so
		// the backend's default effort does not apply; "" disables it.
		if _, mapped := info.Effort["off"]; r.Reasoning == nil && !mapped && info.Level(req.Thinking) == "off" && (info.Reasoning == nil || *info.Reasoning) {
			r.Reasoning = &reasoning{Effort: "none"}
		}
	}
	return json.Marshal(r)
}

// requestBody encodes the body while the transport sends it, so a long
// session is never held in memory as one request. The transport closes the
// reader on every path, which ends the writing goroutine.
func requestBody(head []byte, req soma.Request, src source) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		w := bufio.NewWriterSize(pw, 32*1024)
		w.WriteString(`{"input":[`)
		err := writeInput(w, req, src)
		if err == nil {
			w.WriteString("],")
			w.Write(head[1:])
			err = w.Flush()
		}
		pw.CloseWithError(err)
	}()
	return pr
}

func clampRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

type inputText struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Detail   string `json:"detail,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type outputText struct {
	Type        string     `json:"type"`
	Text        string     `json:"text"`
	Annotations []struct{} `json:"annotations"`
}

// The input item shapes this client sends. Each is its own type, so that
// long text is encoded once, straight into the request body.
type (
	userItem struct {
		Role    string      `json:"role"`
		Content []inputText `json:"content"`
	}
	assistantTextItem struct {
		Role    string `json:"role"`
		Content string `json:"content,omitempty"`
	}
	messageItem struct {
		Type    string       `json:"type"`
		Role    string       `json:"role"`
		ID      string       `json:"id"`
		Status  string       `json:"status"`
		Phase   string       `json:"phase,omitempty"`
		Content []outputText `json:"content"`
	}
	callItem struct {
		Type      string `json:"type"`
		ID        string `json:"id,omitempty"`
		CallID    string `json:"call_id,omitempty"`
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	}
	outputItemText struct {
		Type   string `json:"type"`
		CallID string `json:"call_id,omitempty"`
		Output string `json:"output"`
	}
	outputItemParts struct {
		Type   string      `json:"type"`
		CallID string      `json:"call_id,omitempty"`
		Output []inputText `json:"output"`
	}
)

type itemShape interface {
	userItem | assistantTextItem | messageItem | callItem | outputItemText | outputItemParts
}

type itemWriter struct {
	buf *bufio.Writer
	enc *json.Encoder
	n   int
}

func (w *itemWriter) sep() {
	if w.n > 0 {
		w.buf.WriteByte(',')
	}
	w.n++
}

func writeItem[T itemShape](w *itemWriter, it T) error {
	w.sep()
	return w.enc.Encode(it)
}

// source names the provider and protocol a request goes to.
type source struct{ name, api string }

func (c *Client) source() source { return source{c.name, c.api} }

// writeInput writes the input items for req.Messages to buf, separated by
// commas, without the surrounding brackets.
func writeInput(buf *bufio.Writer, req soma.Request, src source) error {
	w := &itemWriter{buf: buf, enc: json.NewEncoder(buf)}
	// Call ids of other providers are normalized; their results must follow.
	ids := map[string]string{}
	for _, m := range req.Messages {
		var err error
		switch m.Role {
		case soma.RoleUser:
			parts := contentParts(m.Blocks, req.LoadBlob)
			if len(parts) == 0 {
				continue
			}
			err = writeItem(w, userItem{Role: "user", Content: parts})
		case soma.RoleAssistant:
			replayable := wire.Replayable(m, src.name, src.api, req.Model)
			for _, b := range m.Blocks {
				var ref wire.ItemRef
				if replayable && len(b.Opaque) > 0 && b.Type != soma.BlockThinking {
					_ = json.Unmarshal(b.Opaque, &ref)
				}
				switch b.Type {
				case soma.BlockThinking:
					if replayable && len(b.Opaque) > 0 {
						w.sep()
						buf.Write(b.Opaque)
					}
				case soma.BlockText:
					if ref.ID == "" {
						err = writeItem(w, assistantTextItem{Role: "assistant", Content: b.Text})
					} else {
						err = writeItem(w, messageItem{
							Type:    "message",
							Role:    "assistant",
							ID:      ref.ID,
							Status:  "completed",
							Phase:   ref.Phase,
							Content: []outputText{{Type: "output_text", Text: b.Text, Annotations: []struct{}{}}},
						})
					}
				case soma.BlockToolCall:
					id := b.CallID
					if !replayable {
						id = wire.ResponsesCallID(b.CallID)
						ids[b.CallID] = id
					}
					err = writeItem(w, callItem{Type: "function_call", ID: ref.ID, CallID: id, Name: b.Name, Arguments: wire.CallArguments(b.Args)})
				}
				if err != nil {
					return err
				}
			}
		case soma.RoleTool:
			id := m.CallID
			if n, ok := ids[m.CallID]; ok {
				id = n
			}
			if parts, ok := toolOutputParts(m.Blocks, req.LoadBlob); ok {
				err = writeItem(w, outputItemParts{Type: "function_call_output", CallID: id, Output: parts})
			} else {
				err = writeItem(w, outputItemText{Type: "function_call_output", CallID: id, Output: wire.ToolOutputText(m.Blocks)})
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func contentParts(blocks []soma.Block, load func(string) ([]byte, error)) []inputText {
	var parts []inputText
	for _, b := range blocks {
		switch b.Type {
		case soma.BlockText:
			parts = append(parts, inputText{Type: "input_text", Text: b.Text})
		case soma.BlockImage:
			parts = append(parts, imagePart(b, load))
		}
	}
	return parts
}

func imagePart(b soma.Block, load func(string) ([]byte, error)) inputText {
	url, err := wire.DataURL(b, load)
	if err != nil {
		return inputText{Type: "input_text", Text: "(image unavailable: " + err.Error() + ")"}
	}
	return inputText{Type: "input_image", Detail: "auto", ImageURL: url}
}

// toolOutputParts returns the output as parts when it holds an image.
func toolOutputParts(blocks []soma.Block, load func(string) ([]byte, error)) ([]inputText, bool) {
	for _, b := range blocks {
		if b.Type == soma.BlockImage {
			return contentParts(blocks, load), true
		}
	}
	return nil, false
}

func (s *streamState) final() (soma.Message, error) {
	if !s.done {
		return soma.Message{}, wire.NetworkError(wire.ErrIncompleteStream)
	}
	if s.failure != nil {
		return soma.Message{}, s.failure
	}
	m := s.message(false)
	hasCalls := false
	for _, b := range m.Blocks {
		if b.Type == soma.BlockToolCall {
			hasCalls = true
		}
	}
	switch {
	case s.status == "incomplete" && s.incompleteReason == "max_output_tokens":
		m.StopReason = soma.StopLength
	case s.status == "incomplete":
		return soma.Message{}, &soma.ProviderError{Kind: soma.ErrOther, Message: fmt.Sprintf("response incomplete: %s", s.incompleteReason)}
	case hasCalls:
		if n := s.unfinishedCalls(); n > 0 {
			return soma.Message{}, &soma.ProviderError{Kind: soma.ErrOther, Message: fmt.Sprintf("response completed with %d unfinished tool call(s)", n)}
		}
		m.StopReason = soma.StopToolCalls
	default:
		m.StopReason = soma.StopEnd
	}
	return m, nil
}

func (s *streamState) partial() soma.Message {
	m := s.message(true)
	m.StopReason = soma.StopAborted
	return m
}
