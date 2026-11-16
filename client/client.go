package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cheetahbyte/soma"
	"github.com/cheetahbyte/soma/config"
	"github.com/cheetahbyte/soma/provider"
	httpwire "github.com/cheetahbyte/soma/provider/wire"
	"github.com/cheetahbyte/soma/wire"
)

func init() {
	provider.Register("soma", func(cfg *config.Config, p config.ProviderConfig) (soma.Provider, error) {
		return newClient(cfg, p)
	})
	provider.RegisterLister("soma", fetchModels)
}

type client struct {
	cfg      *config.Config
	entry    config.ProviderConfig
	baseURL  string
	upstream string
	token    string
	http     *http.Client
	idle     time.Duration
	infoMu   sync.Mutex
}

func newClient(cfg *config.Config, p config.ProviderConfig) (*client, error) {
	token, err := cfg.Key(p)
	if err != nil {
		return nil, fmt.Errorf("soma provider %q: %w", p.Name, err)
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, httpwire.Redact(fmt.Errorf("soma provider %q: baseUrl must be an http(s) server URL without credentials, query, or fragment", p.Name), token)
	}
	upstream := p.Upstream
	if upstream == "" {
		upstream = p.Name
	}
	h, idle := httpwire.NewHTTPClient()
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client{cfg: cfg, entry: p, baseURL: strings.TrimRight(u.String(), "/"), upstream: upstream, token: token, http: h, idle: idle}, nil
}

func fetchModels(ctx context.Context, cfg *config.Config, p config.ProviderConfig) ([]config.ModelInfo, error) {
	c, err := newClient(cfg, p)
	if err != nil {
		_ = config.SaveCatalog(cfg.Root, p.Name, config.Catalog{Fetched: time.Now(), Error: err.Error()})
		return nil, err
	}
	info, err := c.fetchAndCacheInfo(ctx)
	return info.Models, err
}

func (c *client) fetchAndCacheInfo(ctx context.Context) (wire.Info, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := c.fetchInfo(ctx)
	cat := config.Catalog{Fetched: time.Now(), Models: info.Models, HoldsConversation: info.HoldsConversation}
	if err != nil {
		err = httpwire.Redact(err, c.token)
		cat = config.Catalog{Fetched: cat.Fetched, Error: err.Error()}
	}
	_ = config.SaveCatalog(c.cfg.Root, c.entry.Name, cat)
	return info, err
}

func (c *client) fetchInfo(ctx context.Context) (wire.Info, error) {
	res, err := c.request(ctx, http.MethodGet, "/v1/info?upstream="+url.QueryEscape(c.upstream), nil)
	if err != nil {
		return wire.Info{}, c.networkError(err)
	}
	defer res.Body.Close()
	if err := c.checkProtocol(res); err != nil {
		return wire.Info{}, err
	}
	if res.StatusCode != http.StatusOK {
		return wire.Info{}, c.responseError(res)
	}
	dec := json.NewDecoder(res.Body)
	dec.DisallowUnknownFields()
	var info *wire.Info
	if err := dec.Decode(&info); err != nil {
		return wire.Info{}, c.networkError(fmt.Errorf("invalid provider information: %w", err))
	}
	if info == nil {
		return wire.Info{}, c.networkError(errors.New("missing provider information"))
	}
	if err := endJSON(dec); err != nil {
		return wire.Info{}, c.networkError(err)
	}
	return *info, nil
}

func (c *client) HoldsConversation() bool {
	c.infoMu.Lock()
	defer c.infoMu.Unlock()
	cat, ok := config.LoadCatalog(c.cfg.Root, c.entry.Name)
	if !c.entry.NeedsCatalog() && (!ok || cat.Stale(time.Now())) {
		if _, err := c.fetchAndCacheInfo(context.Background()); err != nil {
			return false
		}
		cat, ok = config.LoadCatalog(c.cfg.Root, c.entry.Name)
	}
	return ok && cat.Error == "" && cat.HoldsConversation
}

func (c *client) EndRun(sessionID string) {
	cat, ok := config.LoadCatalog(c.cfg.Root, c.entry.Name)
	if !ok || cat.Error != "" || !cat.HoldsConversation {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, err := json.Marshal(wire.EndRun{Upstream: c.upstream, SessionID: sessionID})
	if err != nil {
		return
	}
	res, err := c.request(ctx, http.MethodPost, "/v1/endrun", body)
	if err == nil {
		res.Body.Close()
	}
}

func (c *client) Stream(ctx context.Context, req soma.Request, onDelta func(soma.Delta)) (soma.Message, error) {
	msg, err := c.stream(ctx, req, onDelta)
	if ctx.Err() != nil {
		if msg.Role == "" {
			msg = soma.Message{Role: soma.RoleAssistant, Model: req.Model}
		}
		msg.StopReason = soma.StopAborted
		return msg, ctx.Err()
	}
	return msg, httpwire.Redact(err, c.token)
}

func (c *client) stream(ctx context.Context, req soma.Request, onDelta func(soma.Delta)) (soma.Message, error) {
	messages := append([]soma.Message(nil), req.Messages...)
	for i := range messages {
		if messages[i].Provider == c.entry.Name {
			messages[i].Provider = c.upstream
		}
	}
	tools := make([]wire.Tool, len(req.Tools))
	for i, tool := range req.Tools {
		tools[i] = wire.Tool{Name: tool.Name, Description: tool.Description, Schema: tool.Schema}
	}
	blobs, names := hashBlobs(req)
	body, err := json.Marshal(wire.StreamRequest{Upstream: c.upstream, Blobs: blobs, Request: wire.Request{
		Model: req.Model, System: req.System, Messages: messages, Tools: tools,
		Thinking: req.Thinking, SessionID: req.SessionID, Epoch: req.Epoch,
	}})
	if err != nil {
		return soma.Message{}, c.somaError(soma.ErrOther, 0, err.Error())
	}
	callerCtx := ctx
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cancel(nil)
	a := &aborter{c: c, ctx: ctx, cancel: cancel}
	defer a.finish()
	defer context.AfterFunc(callerCtx, a.abort)()
	if callerCtx.Err() != nil {
		return soma.Message{}, callerCtx.Err()
	}
	res, err := c.request(ctx, http.MethodPost, "/v1/stream", body)
	if err == nil && res.StatusCode == http.StatusConflict {
		err = c.uploadMissing(ctx, res, names, req.LoadBlob)
		res.Body.Close()
		if err != nil {
			return soma.Message{}, err
		}
		if callerCtx.Err() != nil {
			return soma.Message{}, callerCtx.Err()
		}
		res, err = c.request(ctx, http.MethodPost, "/v1/stream", body)
		if err == nil && res.StatusCode == http.StatusConflict {
			res.Body.Close()
			return soma.Message{}, c.somaError(soma.ErrOther, res.StatusCode, "server still lacks blobs after they were uploaded")
		}
	}
	if err != nil {
		return soma.Message{}, c.networkError(err)
	}
	defer res.Body.Close()
	timer := time.AfterFunc(c.idle, func() { cancel(httpwire.ErrStreamIdle) })
	defer timer.Stop()
	if err := c.checkProtocol(res); err != nil {
		return soma.Message{}, err
	}
	if res.StatusCode != http.StatusOK {
		return soma.Message{}, c.responseError(res)
	}
	contentType, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || contentType != "application/x-ndjson" {
		return soma.Message{}, c.networkError(errors.New("expected application/x-ndjson response"))
	}
	r := bufio.NewReader(res.Body)
	started := false
	for {
		data, err := r.ReadBytes('\n')
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				err = cause
			} else if errors.Is(err, io.EOF) {
				err = httpwire.ErrIncompleteStream
			}
			return soma.Message{}, c.networkError(err)
		}
		timer.Reset(c.idle)
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		var line wire.Line
		if err := dec.Decode(&line); err != nil {
			return soma.Message{}, c.networkError(fmt.Errorf("invalid stream line: %w", err))
		}
		if err := endJSON(dec); err != nil {
			return soma.Message{}, c.networkError(err)
		}
		switch {
		case !started && line.Stream != "" && line.Delta == nil && !line.KeepAlive && !line.Done && line.Message == nil && line.Error == nil:
			started = true
			a.started(line.Stream)
		case !started || line.Stream != "":
			return soma.Message{}, c.networkError(errors.New("missing or unexpected stream id"))
		case line.Done && line.Message != nil && line.Delta == nil && !line.KeepAlive:
			if line.Error != nil {
				return *line.Message, c.wireError(*line.Error)
			}
			return *line.Message, nil
		case line.KeepAlive && !line.Done && line.Delta == nil && line.Message == nil && line.Error == nil:
		case line.Delta != nil && !line.Done && !line.KeepAlive && line.Message == nil && line.Error == nil:
			delta := soma.Delta{Kind: soma.DeltaKind(line.Delta.Kind), Text: line.Delta.Text}
			switch delta.Kind {
			case soma.DeltaText, soma.DeltaThinking, soma.DeltaToolCall, soma.DeltaNotice:
			default:
				return soma.Message{}, c.networkError(fmt.Errorf("unknown delta kind %q", delta.Kind))
			}
			if onDelta != nil && callerCtx.Err() == nil {
				onDelta(delta)
			}
		default:
			return soma.Message{}, c.networkError(errors.New("incomplete or invalid stream line"))
		}
	}
}

var errAbortUnanswered = errors.New("stream did not end within 5 seconds of its abort")

// aborter turns the caller's cancellation into an abort request, sent once
// the stream id is known, so the final line still brings the partial message.
// Dropping the connection instead would make the server abandon the run.
type aborter struct {
	c      *client
	ctx    context.Context
	cancel context.CancelCauseFunc

	mu       sync.Mutex
	stream   string
	aborted  bool
	finished bool
	deadline *time.Timer
}

func (a *aborter) started(stream string) {
	a.mu.Lock()
	a.stream = stream
	send := a.aborted
	a.mu.Unlock()
	if send {
		a.send(stream)
	}
}

func (a *aborter) abort() {
	a.mu.Lock()
	if a.finished || a.aborted {
		a.mu.Unlock()
		return
	}
	a.aborted = true
	a.deadline = time.AfterFunc(5*time.Second, func() { a.cancel(errAbortUnanswered) })
	stream := a.stream
	a.mu.Unlock()
	if stream != "" {
		a.send(stream)
	}
}

func (a *aborter) send(stream string) {
	body, err := json.Marshal(wire.Abort{Stream: stream})
	if err != nil {
		a.cancel(err)
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
	defer cancel()
	res, err := a.c.request(ctx, http.MethodPost, "/v1/abort", body)
	if err == nil {
		res.Body.Close()
	}
}

func (a *aborter) finish() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.finished = true
	if a.deadline != nil {
		a.deadline.Stop()
	}
}

// hashBlobs maps every blob the messages reference to its SHA-256; a blob
// that cannot be loaded is left out, so the server's provider reports it
// as a local provider would.
func hashBlobs(req soma.Request) (map[string]string, map[string]string) {
	if req.LoadBlob == nil {
		return nil, nil
	}
	blobs, names := map[string]string{}, map[string]string{}
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if b.Blob == "" {
				continue
			}
			if _, ok := blobs[b.Blob]; ok {
				continue
			}
			data, err := req.LoadBlob(b.Blob)
			if err != nil {
				continue
			}
			sum := sha256.Sum256(data)
			h := hex.EncodeToString(sum[:])
			blobs[b.Blob], names[h] = h, b.Blob
		}
	}
	if len(blobs) == 0 {
		return nil, nil
	}
	return blobs, names
}

func (c *client) uploadMissing(ctx context.Context, res *http.Response, names map[string]string, load func(string) ([]byte, error)) error {
	if err := c.checkProtocol(res); err != nil {
		return err
	}
	dec := json.NewDecoder(io.LimitReader(res.Body, 1024*1024))
	dec.DisallowUnknownFields()
	var missing wire.MissingBlobs
	if err := dec.Decode(&missing); err != nil {
		return c.networkError(fmt.Errorf("invalid missing-blob answer: %w", err))
	}
	if err := endJSON(dec); err != nil {
		return c.networkError(err)
	}
	for _, h := range missing.Missing {
		name, ok := names[h]
		if !ok {
			return c.somaError(soma.ErrOther, res.StatusCode, fmt.Sprintf("server asked for blob %s, which the request does not name", h))
		}
		data, err := load(name)
		if err != nil {
			return c.somaError(soma.ErrOther, 0, fmt.Sprintf("load blob %s: %v", name, err))
		}
		put, err := c.request(ctx, http.MethodPut, "/v1/blobs/"+h, data)
		if err != nil {
			return c.networkError(err)
		}
		err = c.checkProtocol(put)
		if err == nil && put.StatusCode != http.StatusNoContent {
			err = c.responseError(put)
		}
		put.Body.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *client) request(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set(wire.HeaderProtocol, strconv.Itoa(wire.Version))
	req.Header.Set("User-Agent", httpwire.UserAgent())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

func (c *client) checkProtocol(res *http.Response) error {
	version := res.Header.Get(wire.HeaderProtocol)
	if version == strconv.Itoa(wire.Version) {
		return nil
	}
	older := ""
	if n, err := strconv.Atoi(version); err == nil {
		if n < wire.Version {
			older = "; server is older"
		} else if n > wire.Version {
			older = "; client is older"
		}
	}
	return c.somaError(soma.ErrOther, res.StatusCode, fmt.Sprintf("server protocol %q is incompatible with client protocol %d%s", version, wire.Version, older))
}

func (c *client) responseError(res *http.Response) error {
	dec := json.NewDecoder(io.LimitReader(res.Body, 1024*1024))
	dec.DisallowUnknownFields()
	var remote *wire.Error
	err := dec.Decode(&remote)
	if err == nil {
		err = endJSON(dec)
	}
	if err == nil && remote != nil && remote.Kind != "" && remote.Message != "" {
		if !remote.Provider && res.StatusCode == http.StatusUnauthorized {
			remote.Kind = string(soma.ErrAuth)
			remote.Status = res.StatusCode
		}
		return c.wireError(*remote)
	}
	kind := soma.ErrOther
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		kind = soma.ErrAuth
	} else if res.StatusCode >= 500 {
		kind = soma.ErrServer
	}
	message := "invalid error response"
	if err != nil {
		message += ": " + err.Error()
	}
	return c.somaError(kind, res.StatusCode, message)
}

func (c *client) wireError(e wire.Error) error {
	out := &soma.ProviderError{Kind: soma.ErrorKind(e.Kind), Status: e.Status, RetryAfter: time.Duration(e.RetryAfterMS) * time.Millisecond, Message: e.Message}
	if !e.Provider {
		out.Message = fmt.Sprintf("soma server %s: %s", c.baseURL, out.Message)
		if out.Status == http.StatusUnauthorized {
			out.Kind = soma.ErrAuth
		}
	}
	return httpwire.Redact(out, c.token)
}

func (c *client) networkError(err error) error {
	return c.somaError(soma.ErrNetwork, 0, err.Error())
}

func (c *client) somaError(kind soma.ErrorKind, status int, message string) error {
	return httpwire.Redact(&soma.ProviderError{Kind: kind, Status: status, Message: fmt.Sprintf("soma server %s: %s", c.baseURL, message)}, c.token)
}

func endJSON(dec *json.Decoder) error {
	var extra json.RawMessage
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("unexpected trailing JSON")
	}
	return err
}
