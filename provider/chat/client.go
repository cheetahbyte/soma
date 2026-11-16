// Package chat speaks the Chat Completions protocol of OpenAI-compatible
// endpoints, and reads their live model list.
package chat

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

const api = "openai-chat"

func init() {
	provider.Register(api, func(cfg *config.Config, p config.ProviderConfig) (soma.Provider, error) {
		if p.BaseURL == "" {
			return nil, fmt.Errorf("provider %q: baseUrl is required for api openai-chat", p.Name)
		}
		key, err := cfg.Key(p)
		if err != nil {
			return nil, err
		}
		return New(p, key, provider.ModelInfoFunc(cfg, p)), nil
	})
	provider.RegisterLister(api, func(ctx context.Context, cfg *config.Config, p config.ProviderConfig) ([]config.ModelInfo, error) {
		key, err := cfg.Key(p)
		if err != nil {
			return nil, err
		}
		return FetchModels(ctx, p, key)
	})
}

type Client struct {
	p       config.ProviderConfig
	baseURL string
	key     string
	info    func(id string) config.ModelInfo
	http    *http.Client
	idle    time.Duration
}

func New(p config.ProviderConfig, key string, info func(id string) config.ModelInfo) *Client {
	client, idle := wire.NewHTTPClient()
	return &Client{
		p:       p,
		baseURL: strings.TrimRight(p.BaseURL, "/"),
		key:     key,
		info:    info,
		http:    client,
		idle:    idle,
	}
}

// request is the body without its messages, which requestBody encodes one
// by one so that a long session is not held in memory several times over.
type request struct {
	Model           string        `json:"model"`
	Stream          bool          `json:"stream"`
	StreamOptions   streamOptions `json:"stream_options"`
	Tools           []tool        `json:"tools,omitempty"`
	MaxTokens       int           `json:"max_tokens,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
	Thinking        *thinking     `json:"thinking,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type tool struct {
	Type     string   `json:"type"`
	Function function `json:"function"`
}

type function struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type thinking struct {
	Type string `json:"type"`
}

func (c *Client) model(id string) config.ModelInfo {
	if c.info == nil {
		return config.ModelInfo{ID: id}
	}
	return c.info(id)
}

func (c *Client) Stream(ctx context.Context, req soma.Request, onDelta func(soma.Delta)) (_ soma.Message, err error) {
	defer func() { err = wire.Redact(err, c.key) }()
	info := c.model(req.Model)
	head, err := requestHead(req, info)
	if err != nil {
		return soma.Message{}, err
	}
	rp := replay{provider: c.p.Name, model: req.Model, as: c.p.ReplayReasoningAs, deepseek: deepseek(info)}
	reqCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.baseURL+"/chat/completions", requestBody(head, req, rp))
	if err != nil {
		return soma.Message{}, err
	}
	httpReq.GetBody = func() (io.ReadCloser, error) { return requestBody(head, req, rp), nil }
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.key)
	}
	for k, v := range c.p.Headers {
		httpReq.Header.Set(k, v)
	}
	if c.p.SessionHeader != "" && req.SessionID != "" {
		httpReq.Header.Set(c.p.SessionHeader, req.SessionID)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return abortedMessage(req.Model), ctx.Err()
		}
		return soma.Message{}, wire.NetworkError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if rerr != nil && ctx.Err() != nil {
			return abortedMessage(req.Model), ctx.Err()
		}
		return soma.Message{}, wire.StatusError(resp, data)
	}

	s := newStreamState(req.Model, info, onDelta)
	timer := time.AfterFunc(c.idle, func() { cancel(wire.ErrStreamIdle) })
	body := &eofReader{r: &wire.IdleReader{R: resp.Body, Timer: timer, Idle: c.idle}}
	err = wire.ReadSSE(body, s.handle)
	timer.Stop()
	if ctx.Err() != nil {
		return s.partial(), ctx.Err()
	}
	if errors.Is(context.Cause(reqCtx), wire.ErrStreamIdle) {
		return soma.Message{}, wire.NetworkError(fmt.Errorf("the stream sent nothing for %s", c.idle))
	}
	// Some endpoints close the stream after the finish chunk without [DONE].
	if err != nil && !(body.eof && s.finish != "") {
		return soma.Message{}, err
	}
	return s.final()
}

type eofReader struct {
	r   io.Reader
	eof bool
}

func (r *eofReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if errors.Is(err, io.EOF) {
		r.eof = true
	}
	return n, err
}

func abortedMessage(model string) soma.Message {
	return soma.Message{Role: soma.RoleAssistant, Model: model, StopReason: soma.StopAborted}
}

func deepseek(info config.ModelInfo) bool {
	return info.ThinkingFormat == "deepseek" && (info.Reasoning == nil || *info.Reasoning)
}

// requestHead encodes every field of the body except the messages.
func requestHead(req soma.Request, info config.ModelInfo) ([]byte, error) {
	r := request{
		Model:         req.Model,
		Stream:        true,
		StreamOptions: streamOptions{IncludeUsage: true},
		MaxTokens:     info.MaxOutput,
	}
	for _, t := range req.Tools {
		params := t.Schema
		if len(bytes.TrimSpace(params)) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		r.Tools = append(r.Tools, tool{Type: "function", Function: function{Name: t.Name, Description: t.Description, Parameters: params}})
	}
	effort, send := info.WireEffort(req.Thinking)
	switch {
	case deepseek(info):
		if level := info.Level(req.Thinking); level == "" || level == "off" {
			r.Thinking = &thinking{Type: "disabled"}
		} else {
			r.Thinking = &thinking{Type: "enabled"}
			if send {
				r.ReasoningEffort = effort
			}
		}
	case send:
		r.ReasoningEffort = effort
	}
	return json.Marshal(r)
}

// requestBody encodes the body while the transport sends it. The transport
// closes the reader on every path, which ends the writing goroutine.
func requestBody(head []byte, req soma.Request, rp replay) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		w := bufio.NewWriterSize(pw, 32*1024)
		w.WriteString(`{"messages":[`)
		err := writeMessages(w, req, rp)
		if err == nil {
			w.WriteString("],")
			w.Write(head[1:])
			err = w.Flush()
		}
		pw.CloseWithError(err)
	}()
	return pr
}
