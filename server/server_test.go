package server_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cheetahbyte/soma"
	_ "github.com/cheetahbyte/soma/client"
	"github.com/cheetahbyte/soma/config"
	"github.com/cheetahbyte/soma/provider"
	"github.com/cheetahbyte/soma/server"
	testmodel "github.com/cheetahbyte/soma/somatest/model"
	"github.com/cheetahbyte/soma/wire"
)

const token = "local-test-token"

type gateway struct {
	srv *httptest.Server
	cfg *config.Config
	pc  config.ProviderConfig
}

func startGateway(t *testing.T, pc config.ProviderConfig, o server.Options) *gateway {
	t.Helper()
	root := t.TempDir()
	pc.Name = "test"
	data, err := json.Marshal(struct {
		Providers map[string]config.ProviderConfig `json:"providers"`
	}{map[string]config.ProviderConfig{"test": pc}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "models.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	o.Root, o.Token = root, token
	s, err := server.New(o)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		h.Close()
	})
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return &gateway{srv: h, cfg: cfg, pc: cfg.Providers["test"]}
}

func modelConfig(baseURL string) config.ProviderConfig {
	return config.ProviderConfig{
		API: "openai-responses", BaseURL: baseURL, APIKey: "fake-key",
		Models: []config.ModelInfo{{ID: "m", ContextWindow: 4096, MaxOutput: 512, InputPrice: 2, OutputPrice: 10}},
	}
}

func remote(t *testing.T, g *gateway, key string) (soma.Provider, *config.Config, config.ProviderConfig) {
	t.Helper()
	pc := config.ProviderConfig{Name: "home", API: "soma", BaseURL: g.srv.URL, APIKey: key, Upstream: "test"}
	cfg := &config.Config{Root: t.TempDir(), Providers: map[string]config.ProviderConfig{pc.Name: pc}}
	p, err := provider.New(cfg, pc)
	if err != nil {
		t.Fatal(err)
	}
	return p, cfg, pc
}

func request() soma.Request {
	return soma.Request{Model: "m", System: "system", SessionID: "session", Epoch: "epoch", Thinking: "high",
		Messages: []soma.Message{{Role: soma.RoleUser, Blocks: []soma.Block{{Type: soma.BlockText, Text: "hello"}}}},
		Tools:    []soma.ToolSpec{{Name: "read", Description: "Read", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
	}
}

func call(t *testing.T, p soma.Provider, req soma.Request) (soma.Message, []soma.Delta, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var deltas []soma.Delta
	m, err := p.Stream(ctx, req, func(d soma.Delta) { deltas = append(deltas, d) })
	return m, deltas, err
}

func rawRequest(t *testing.T, g *gateway, method, path, body, version, key string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, method, g.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set(wire.HeaderProtocol, version)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestStreamReturnsReply(t *testing.T) {
	step := testmodel.Step{Text: "hello back", Thinking: "reasoning", ToolCalls: []testmodel.ToolCall{{Name: "read", Args: `{"path":"file"}`}}}
	directModel := testmodel.Start([]testmodel.Step{step})
	defer directModel.Close()
	remoteModel := testmodel.Start([]testmodel.Step{step})
	defer remoteModel.Close()
	g := startGateway(t, modelConfig(remoteModel.URL), server.Options{})
	pc := modelConfig(directModel.URL)
	pc.Name = "test"
	cfg := &config.Config{Root: t.TempDir(), Providers: map[string]config.ProviderConfig{pc.Name: pc}}
	direct, err := provider.New(cfg, pc)
	if err != nil {
		t.Fatal(err)
	}
	want, wantDeltas, err := call(t, direct, request())
	if err != nil {
		t.Fatal(err)
	}
	p, _, _ := remote(t, g, token)
	got, gotDeltas, err := call(t, p, request())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotDeltas, wantDeltas) {
		t.Fatalf("remote = %+v, %v; direct = %+v, %v", got, gotDeltas, want, wantDeltas)
	}
	if got.Text() != "hello back" || got.StopReason != soma.StopToolCalls || got.Usage == nil || got.Usage.Cost <= 0 || len(gotDeltas) < 3 {
		t.Fatalf("reply lacks text, calls, deltas, or usage: %+v, %v", got, gotDeltas)
	}
}

func TestDeltasArriveBeforeFinalReply(t *testing.T) {
	sawDelta := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"first\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-sawDelta:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg\",\"content\":[{\"type\":\"output_text\",\"text\":\"first\"}]}}\n\n")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer upstream.Close()
	g := startGateway(t, modelConfig(upstream.URL), server.Options{})
	p, _, _ := remote(t, g, token)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m, err := p.Stream(ctx, request(), func(d soma.Delta) {
		if d.Kind == soma.DeltaText && d.Text == "first" {
			close(sawDelta)
		}
	})
	if err != nil || m.Text() != "first" {
		t.Fatalf("stream did not deliver its delta before the final reply: %+v, %v", m, err)
	}
}

func TestWrongTokenIsAuthError(t *testing.T) {
	model := testmodel.Start(nil)
	defer model.Close()
	g := startGateway(t, modelConfig(model.URL), server.Options{})
	p, _, _ := remote(t, g, "wrong")
	_, _, err := call(t, p, request())
	var pe *soma.ProviderError
	if !errors.As(err, &pe) || pe.Kind != soma.ErrAuth || pe.Status != http.StatusUnauthorized || !strings.Contains(strings.ToLower(pe.Message), "soma") {
		t.Fatalf("wrong token: %v", err)
	}
	if len(model.Requests()) != 0 {
		t.Fatal("unauthorized request reached the provider")
	}
}

func TestVersionMismatch(t *testing.T) {
	g := startGateway(t, modelConfig("http://127.0.0.1:1"), server.Options{})
	for _, version := range []string{"999", "0", ""} {
		t.Run(version, func(t *testing.T) {
			res := rawRequest(t, g, http.MethodPost, "/v1/stream", `{}`, version, token)
			body, _ := io.ReadAll(res.Body)
			if res.StatusCode != http.StatusUpgradeRequired || !bytes.Contains(body, []byte(strconv.Itoa(wire.Version))) {
				t.Fatalf("version %q: status %d body %s", version, res.StatusCode, body)
			}
			older := "server"
			if version != "999" {
				older = "client"
			}
			if !strings.Contains(string(body), older) || !strings.Contains(string(body), "older") {
				t.Fatalf("version mismatch does not name the older side: %s", body)
			}
		})
	}
}

func TestUnknownFieldRefused(t *testing.T) {
	g := startGateway(t, modelConfig("http://127.0.0.1:1"), server.Options{})
	for _, body := range []string{
		`{"upstream":"test","request":{"model":"m"},"surprise":true}`,
		`{"upstream":"test","request":{"model":"m","surprise":true}}`,
		`{"upstream":"test","request":{"messages":[{"role":"user","surprise":true}]}}`,
		`{"upstream":"test","request":{"model":"m"}} {}`,
	} {
		res := rawRequest(t, g, http.MethodPost, "/v1/stream", body, strconv.Itoa(wire.Version), token)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %s: status %d", body, res.StatusCode)
		}
	}
}

func TestProviderErrorKeepsKind(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After-Ms", "1234")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"slow down","type":"rate_limit"}}`)
	}))
	defer upstream.Close()
	g := startGateway(t, modelConfig(upstream.URL), server.Options{})
	p, _, _ := remote(t, g, token)
	_, _, err := call(t, p, request())
	var pe *soma.ProviderError
	if !errors.As(err, &pe) || pe.Kind != soma.ErrRateLimit || pe.Status != 429 || pe.RetryAfter != 1234*time.Millisecond || pe.Message != "rate_limit: slow down" {
		t.Fatalf("provider error = %#v, %v", pe, err)
	}
}

func TestNamesAreTranslated(t *testing.T) {
	model := testmodel.Start([]testmodel.Step{{Text: "ok"}})
	defer model.Close()
	g := startGateway(t, modelConfig(model.URL), server.Options{})
	p, _, _ := remote(t, g, token)
	req := request()
	reasoning := json.RawMessage(`{"type":"reasoning","id":"reasoning-id","encrypted_content":"preserved-secret","summary":[]}`)
	previous := soma.Message{Role: soma.RoleAssistant, Provider: "home", API: "openai-responses", Model: "m", StopReason: soma.StopEnd,
		Blocks: []soma.Block{{Type: soma.BlockThinking, Opaque: reasoning}}}
	req.Messages = append([]soma.Message{previous}, req.Messages...)
	if _, _, err := call(t, p, req); err != nil {
		t.Fatal(err)
	}
	bodies := model.Requests()
	if len(bodies) != 1 || !bytes.Contains(bodies[0], []byte(`"encrypted_content":"preserved-secret"`)) {
		t.Fatalf("reasoning was not recognized as the upstream's own reply: %s", bodies)
	}
	if req.Messages[0].Provider != "home" || !bytes.Equal(req.Messages[0].Blocks[0].Opaque, reasoning) {
		t.Fatal("client mutated the session's original messages")
	}
}

func TestInfoFillsCatalog(t *testing.T) {
	g := startGateway(t, modelConfig("http://127.0.0.1:1"), server.Options{})
	_, cfg, pc := remote(t, g, token)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	models, err := provider.FetchModels(ctx, cfg, pc)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(models, g.pc.Models) {
		t.Fatalf("model information = %+v, want %+v", models, g.pc.Models)
	}
	cat, ok := config.LoadCatalog(cfg.Root, pc.Name)
	if !ok || cat.Error != "" || cat.HoldsConversation || !reflect.DeepEqual(cat.Models, models) || cat.Fetched.IsZero() {
		t.Fatalf("catalog = %+v, %v", cat, ok)
	}
	if !pc.NeedsCatalog() {
		t.Fatal("soma without configured models must need a catalog")
	}
	pc.Models = models
	if pc.NeedsCatalog() {
		t.Fatal("soma with configured models must not need a catalog")
	}
}

func TestOldCatalogFileReads(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cache", "models")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "old.json"), []byte(`{"models":[{"id":"m"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cat, ok := config.LoadCatalog(root, "old")
	if !ok || cat.Error != "" || cat.HoldsConversation || len(cat.Models) != 1 || cat.Models[0].ID != "m" {
		t.Fatalf("old catalog = %+v, %v", cat, ok)
	}
}

func delayedModel(t *testing.T, delay time.Duration) string {
	t.Helper()
	model := testmodel.Start([]testmodel.Step{{Text: "eventually"}, {Text: "eventually"}})
	t.Cleanup(model.Close)
	target, err := url.Parse(model.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
			proxy.ServeHTTP(w, r)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestKeepAlive(t *testing.T) {
	g := startGateway(t, modelConfig(delayedModel(t, 100*time.Millisecond)), server.Options{KeepAlive: 20 * time.Millisecond})
	body, err := json.Marshal(wire.StreamRequest{Upstream: "test", Request: wire.Request{Model: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	res := rawRequest(t, g, http.MethodPost, "/v1/stream", string(body), strconv.Itoa(wire.Version), token)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream status %d", res.StatusCode)
	}
	scan := bufio.NewScanner(res.Body)
	keep, delta, done := false, false, false
	for scan.Scan() {
		var line wire.Line
		if err := json.Unmarshal(scan.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		if line.KeepAlive && !delta {
			keep = true
		}
		delta = delta || line.Delta != nil
		done = done || line.Done
	}
	if scan.Err() != nil || !keep || !delta || !done {
		t.Fatalf("keep-alive before delta = %v, delta = %v, done = %v, error = %v", keep, delta, done, scan.Err())
	}
	p, _, _ := remote(t, g, token)
	m, _, err := call(t, p, request())
	if err != nil || m.Text() != "eventually" {
		t.Fatalf("client with keep-alives: %+v, %v", m, err)
	}
}

func TestQuietStreamIsNetworkError(t *testing.T) {
	t.Setenv("SOMA_IDLE_TIMEOUT_MS", "50")
	g := startGateway(t, modelConfig(delayedModel(t, time.Second)), server.Options{KeepAlive: time.Hour})
	p, _, _ := remote(t, g, token)
	_, _, err := call(t, p, request())
	var pe *soma.ProviderError
	if !errors.As(err, &pe) || pe.Kind != soma.ErrNetwork || !strings.Contains(pe.Message, "soma") || !strings.Contains(pe.Message, g.srv.URL) {
		t.Fatalf("quiet stream error = %v", err)
	}
}

func TestTokenRequired(t *testing.T) {
	if _, err := server.New(server.Options{Root: t.TempDir()}); err == nil {
		t.Fatal("server started without a token")
	}
}

func TestAuthenticationGuardsEveryRoute(t *testing.T) {
	g := startGateway(t, modelConfig("http://127.0.0.1:1"), server.Options{})
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/v1/stream"},
		{http.MethodGet, "/v1/info?upstream=test"},
		{http.MethodPost, "/v1/endrun"},
		{http.MethodPost, "/v1/abort"},
		{http.MethodPut, "/v1/blobs/hash"},
	} {
		res := rawRequest(t, g, route.method, route.path, `{}`, strconv.Itoa(wire.Version), "wrong")
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s: status %d", route.method, route.path, res.StatusCode)
		}
	}
}

func TestEarlyStreamEndIsNetworkError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(wire.HeaderProtocol, strconv.Itoa(wire.Version))
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, "{\"stream\":\"id\"}\n{\"delta\":{\"kind\":\"text\",\"text\":\"partial\"}}\n")
	}))
	defer upstream.Close()
	pc := config.ProviderConfig{Name: "home", API: "soma", BaseURL: upstream.URL, APIKey: token}
	cfg := &config.Config{Root: t.TempDir(), Providers: map[string]config.ProviderConfig{pc.Name: pc}}
	p, err := provider.New(cfg, pc)
	if err != nil {
		t.Fatal(err)
	}
	_, deltas, err := call(t, p, request())
	var pe *soma.ProviderError
	if !errors.As(err, &pe) || pe.Kind != soma.ErrNetwork || !strings.Contains(pe.Message, upstream.URL) {
		t.Fatalf("early EOF = %v", err)
	}
	if len(deltas) != 1 || deltas[0].Text != "partial" {
		t.Fatalf("deltas before EOF = %+v", deltas)
	}
}

type holdingProvider struct{ root string }

func init() {
	provider.Register("test-holder", func(cfg *config.Config, pc config.ProviderConfig) (soma.Provider, error) {
		return holdingProvider{root: cfg.Root}, nil
	})
}

func (holdingProvider) Stream(context.Context, soma.Request, func(soma.Delta)) (soma.Message, error) {
	return soma.Message{Role: soma.RoleAssistant, Blocks: []soma.Block{{Type: soma.BlockText, Text: "ok"}}, StopReason: soma.StopEnd}, nil
}

func (holdingProvider) HoldsConversation() bool { return true }

func (p holdingProvider) EndRun(id string) {
	os.WriteFile(filepath.Join(p.root, "ended"), []byte(id), 0600)
}

func TestExplicitModelsDiscoverConversationAndEndRun(t *testing.T) {
	g := startGateway(t, config.ProviderConfig{API: "test-holder", Models: []config.ModelInfo{{ID: "m", ContextWindow: 1000, InputPrice: 2, MaxOutput: 512}}}, server.Options{})
	pc := config.ProviderConfig{Name: "home", API: "soma", BaseURL: g.srv.URL, APIKey: token, Upstream: "test", Models: []config.ModelInfo{{ID: "m", MaxOutput: 42}}}
	cfg := &config.Config{Root: t.TempDir(), Providers: map[string]config.ProviderConfig{pc.Name: pc}}
	p, err := provider.New(cfg, pc)
	if err != nil {
		t.Fatal(err)
	}
	if !p.(soma.ConversationHolder).HoldsConversation() {
		t.Fatal("an explicit model list hid the upstream's conversation capability")
	}
	cat, ok := config.LoadCatalog(cfg.Root, pc.Name)
	if !ok || !cat.HoldsConversation || cat.Error != "" {
		t.Fatalf("cached conversation capability = %+v, %v", cat, ok)
	}
	_, info, err := cfg.Model("home/m")
	if err != nil || info.ContextWindow != 1000 || info.InputPrice != 2 || info.MaxOutput != 42 {
		t.Fatalf("explicit model must inherit upstream metadata but retain its overrides: %+v, %v", info, err)
	}
	p.(soma.RunEnder).EndRun("run-id")
	ended, err := os.ReadFile(filepath.Join(g.cfg.Root, "ended"))
	if err != nil || string(ended) != "run-id" {
		t.Fatalf("upstream endrun = %q, %v", ended, err)
	}
}

func TestSeveralSessionsStreamConcurrently(t *testing.T) {
	steps := make([]testmodel.Step, 8)
	for i := range steps {
		steps[i].Text = "parallel"
	}
	model := testmodel.Start(steps)
	defer model.Close()
	g := startGateway(t, modelConfig(model.URL), server.Options{})
	p, _, _ := remote(t, g, token)
	errs := make(chan error, len(steps))
	for i := range steps {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req := request()
			req.SessionID = strconv.Itoa(i)
			m, err := p.Stream(ctx, req, nil)
			if err == nil && m.Text() != "parallel" {
				err = errors.New("concurrent session received an incorrect reply")
			}
			errs <- err
		}()
	}
	for range steps {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	if len(model.Requests()) != len(steps) {
		t.Fatalf("upstream received %d requests, want %d", len(model.Requests()), len(steps))
	}
}

func TestInfoFailureIsCachedForTenMinutes(t *testing.T) {
	g := startGateway(t, modelConfig("http://127.0.0.1:1"), server.Options{})
	_, cfg, pc := remote(t, g, "wrong")
	if _, err := provider.FetchModels(context.Background(), cfg, pc); err == nil {
		t.Fatal("model information succeeded with a wrong token")
	}
	cat, ok := config.LoadCatalog(cfg.Root, pc.Name)
	if !ok || cat.Error == "" || cat.HoldsConversation || cat.Stale(time.Now()) || !cat.Stale(time.Now().Add(11*time.Minute)) {
		t.Fatalf("failed metadata cache = %+v, %v", cat, ok)
	}
}

func TestUpstreamDefaultsToEntryName(t *testing.T) {
	model := testmodel.Start([]testmodel.Step{{Text: "default upstream"}})
	defer model.Close()
	g := startGateway(t, modelConfig(model.URL), server.Options{})
	pc := config.ProviderConfig{Name: "test", API: "soma", BaseURL: g.srv.URL, APIKey: token}
	cfg := &config.Config{Root: t.TempDir(), Providers: map[string]config.ProviderConfig{pc.Name: pc}}
	p, err := provider.New(cfg, pc)
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := call(t, p, request())
	if err != nil || m.Text() != "default upstream" {
		t.Fatalf("default upstream reply = %+v, %v", m, err)
	}
}

func TestInvalidServerURLDoesNotExposeCredentials(t *testing.T) {
	for _, baseURL := range []string{"http://user:private-password@127.0.0.1", "http://127.0.0.1?key=private-query", "http://127.0.0.1#private-fragment"} {
		pc := config.ProviderConfig{Name: "home", API: "soma", BaseURL: baseURL, APIKey: token}
		cfg := &config.Config{Root: t.TempDir(), Providers: map[string]config.ProviderConfig{pc.Name: pc}}
		_, err := provider.New(cfg, pc)
		if err == nil || strings.Contains(err.Error(), "private-") {
			t.Fatalf("invalid URL exposed credentials or was accepted: %v", err)
		}
	}
}

func TestRedirectDoesNotForwardToken(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a redirect forwarded a Soma request to another server")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(wire.HeaderProtocol, strconv.Itoa(wire.Version))
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	pc := config.ProviderConfig{Name: "home", API: "soma", BaseURL: redirect.URL, APIKey: token}
	cfg := &config.Config{Root: t.TempDir(), Providers: map[string]config.ProviderConfig{pc.Name: pc}}
	p, err := provider.New(cfg, pc)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := call(t, p, request()); err == nil {
		t.Fatal("redirect was accepted as a successful stream")
	}
}

func TestUnknownResponseFieldIsNetworkError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(wire.HeaderProtocol, strconv.Itoa(wire.Version))
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, "{\"stream\":\"id\"}\n{\"keepAlive\":true,\"unknown\":true}\n")
	}))
	defer upstream.Close()
	pc := config.ProviderConfig{Name: "home", API: "soma", BaseURL: upstream.URL, APIKey: token}
	cfg := &config.Config{Root: t.TempDir(), Providers: map[string]config.ProviderConfig{pc.Name: pc}}
	p, err := provider.New(cfg, pc)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = call(t, p, request())
	var pe *soma.ProviderError
	if !errors.As(err, &pe) || pe.Kind != soma.ErrNetwork {
		t.Fatalf("unknown response field = %v", err)
	}
}

func TestEndRunTimeoutIsBounded(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { close(release) })
	pc := config.ProviderConfig{Name: "home", API: "soma", BaseURL: upstream.URL, APIKey: token}
	cfg := &config.Config{Root: t.TempDir(), Providers: map[string]config.ProviderConfig{pc.Name: pc}}
	if err := config.SaveCatalog(cfg.Root, pc.Name, config.Catalog{HoldsConversation: true, Fetched: time.Now()}); err != nil {
		t.Fatal(err)
	}
	p, err := provider.New(cfg, pc)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		p.(soma.RunEnder).EndRun("run-id")
		close(done)
	}()
	timer := time.NewTimer(7 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Fatal("endrun exceeded its five-second limit")
	}
}

func TestEndRunDoesNotFetchUnknownCapability(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unknown conversation capability sent %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	pc := config.ProviderConfig{Name: "home", API: "soma", BaseURL: upstream.URL, APIKey: token}
	cfg := &config.Config{Root: t.TempDir(), Providers: map[string]config.ProviderConfig{pc.Name: pc}}
	p, err := provider.New(cfg, pc)
	if err != nil {
		t.Fatal(err)
	}
	p.(soma.RunEnder).EndRun("run-id")
}

type exchange struct {
	method, path string
	status       int
	body         []byte
}

func recordingProxy(t *testing.T, target string) (string, func() []exchange) {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []exchange
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.FlushInterval = -1
	proxy.ModifyResponse = func(res *http.Response) error {
		e := exchange{method: res.Request.Method, path: res.Request.URL.Path, status: res.StatusCode}
		if res.StatusCode != http.StatusOK {
			body, err := io.ReadAll(res.Body)
			if err != nil {
				return err
			}
			res.Body.Close()
			res.Body = io.NopCloser(bytes.NewReader(body))
			e.body = body
		}
		mu.Lock()
		seen = append(seen, e)
		mu.Unlock()
		return nil
	}
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	return srv.URL, func() []exchange {
		mu.Lock()
		defer mu.Unlock()
		return append([]exchange(nil), seen...)
	}
}

func remoteAt(t *testing.T, baseURL string) soma.Provider {
	t.Helper()
	pc := config.ProviderConfig{Name: "home", API: "soma", BaseURL: baseURL, APIKey: token, Upstream: "test"}
	cfg := &config.Config{Root: t.TempDir(), Providers: map[string]config.ProviderConfig{pc.Name: pc}}
	p, err := provider.New(cfg, pc)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func imageRequest(name string, data []byte) soma.Request {
	req := request()
	req.Messages = []soma.Message{{Role: soma.RoleUser, Blocks: []soma.Block{
		{Type: soma.BlockText, Text: "look"}, {Type: soma.BlockImage, Blob: name, MIME: "image/png"},
	}}}
	req.LoadBlob = func(n string) ([]byte, error) {
		if n != name {
			return nil, os.ErrNotExist
		}
		return data, nil
	}
	return req
}

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func storedBlobs(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "blobs"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestBlobUploadedOnce(t *testing.T) {
	model := testmodel.Start([]testmodel.Step{{Text: "one"}, {Text: "two"}})
	defer model.Close()
	g := startGateway(t, modelConfig(model.URL), server.Options{})
	proxyURL, seen := recordingProxy(t, g.srv.URL)
	p := remoteAt(t, proxyURL)
	image := []byte("png bytes of a screenshot")
	for _, name := range []string{"shot", "same-image-again"} {
		if m, _, err := call(t, p, imageRequest(name, image)); err != nil {
			t.Fatalf("request naming %s: %+v, %v", name, m, err)
		}
	}
	puts := 0
	for _, e := range seen() {
		if e.method == http.MethodPut {
			puts++
			if e.path != "/v1/blobs/"+hashOf(image) || e.status != http.StatusNoContent {
				t.Fatalf("upload = %+v", e)
			}
		}
	}
	if puts != 1 {
		t.Fatalf("image uploaded %d times, want once: %+v", puts, seen())
	}
	if len(model.Requests()) != 2 {
		t.Fatalf("upstream received %d requests, want 2", len(model.Requests()))
	}
}

func TestMissingBlobAnswered(t *testing.T) {
	model := testmodel.Start([]testmodel.Step{{Text: "seen"}})
	defer model.Close()
	g := startGateway(t, modelConfig(model.URL), server.Options{})
	image := []byte("\x89PNG image payload")
	hash := hashOf(image)
	body, err := json.Marshal(wire.StreamRequest{Upstream: "test", Request: wire.Request{Model: "m"}, Blobs: map[string]string{"shot": hash}})
	if err != nil {
		t.Fatal(err)
	}
	res := rawRequest(t, g, http.MethodPost, "/v1/stream", string(body), strconv.Itoa(wire.Version), token)
	var missing wire.MissingBlobs
	if err := json.NewDecoder(res.Body).Decode(&missing); res.StatusCode != http.StatusConflict || err != nil || !reflect.DeepEqual(missing.Missing, []string{hash}) {
		t.Fatalf("missing blob answer: status %d, %+v, %v", res.StatusCode, missing, err)
	}
	if len(model.Requests()) != 0 {
		t.Fatal("a request with a missing blob reached the provider")
	}
	proxyURL, seen := recordingProxy(t, g.srv.URL)
	m, _, err := call(t, remoteAt(t, proxyURL), imageRequest("shot", image))
	if err != nil || m.Text() != "seen" {
		t.Fatalf("retried request: %+v, %v", m, err)
	}
	var trace []string
	for _, e := range seen() {
		trace = append(trace, e.method+" "+e.path+" "+strconv.Itoa(e.status))
	}
	want := []string{"POST /v1/stream 409", "PUT /v1/blobs/" + hash + " 204", "POST /v1/stream 200"}
	if !reflect.DeepEqual(trace, want) {
		t.Fatalf("exchanges = %v, want %v", trace, want)
	}
	if first := seen()[0]; !bytes.Contains(first.body, []byte(hash)) {
		t.Fatalf("409 body does not name the hash: %s", first.body)
	}
	bodies := model.Requests()
	if len(bodies) != 1 || !bytes.Contains(bodies[0], []byte(base64.StdEncoding.EncodeToString(image))) {
		t.Fatalf("provider did not receive the image bytes: %s", bodies)
	}
}

func TestBlobHashChecked(t *testing.T) {
	g := startGateway(t, modelConfig("http://127.0.0.1:1"), server.Options{})
	version := strconv.Itoa(wire.Version)
	for _, path := range []string{"/v1/blobs/" + hashOf([]byte("expected")), "/v1/blobs/not-a-hash", "/v1/blobs/" + strings.ToUpper(hashOf([]byte("other")))} {
		res := rawRequest(t, g, http.MethodPut, path, "different", version, token)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("PUT %s: status %d", path, res.StatusCode)
		}
	}
	if names := storedBlobs(t, g.cfg.Root); len(names) != 0 {
		t.Fatalf("rejected uploads left blobs: %v", names)
	}
	res := rawRequest(t, g, http.MethodPut, "/v1/blobs/"+hashOf([]byte("matching")), "matching", version, token)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("matching upload: status %d", res.StatusCode)
	}
	if names := storedBlobs(t, g.cfg.Root); !reflect.DeepEqual(names, []string{hashOf([]byte("matching"))}) {
		t.Fatalf("stored blobs = %v", names)
	}
}

func TestBlobCacheBounded(t *testing.T) {
	g := startGateway(t, modelConfig("http://127.0.0.1:1"), server.Options{BlobCacheBytes: 10})
	version := strconv.Itoa(wire.Version)
	older, newer := []byte("8 bytes!"), []byte("8 more!!")
	for _, data := range [][]byte{older, newer} {
		res := rawRequest(t, g, http.MethodPut, "/v1/blobs/"+hashOf(data), string(data), version, token)
		if res.StatusCode != http.StatusNoContent {
			t.Fatalf("upload: status %d", res.StatusCode)
		}
	}
	if names := storedBlobs(t, g.cfg.Root); !reflect.DeepEqual(names, []string{hashOf(newer)}) {
		t.Fatalf("stored blobs = %v, want only the newer %s", names, hashOf(newer))
	}
}
