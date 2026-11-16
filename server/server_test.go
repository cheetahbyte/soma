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
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cheetahbyte/soma"
	"github.com/cheetahbyte/soma/auth"
	_ "github.com/cheetahbyte/soma/client"
	"github.com/cheetahbyte/soma/config"
	"github.com/cheetahbyte/soma/provider"
	"github.com/cheetahbyte/soma/provider/claudecode"
	"github.com/cheetahbyte/soma/provider/openai"
	"github.com/cheetahbyte/soma/server"
	testauth "github.com/cheetahbyte/soma/somatest/auth"
	testclaude "github.com/cheetahbyte/soma/somatest/claude"
	testmodel "github.com/cheetahbyte/soma/somatest/model"
	"github.com/cheetahbyte/soma/wire"
)

const token = "local-test-token"

func TestMain(m *testing.M) {
	if os.Getenv("SOMA_TEST_FAKE_CLAUDE") == "1" {
		os.Exit(testclaude.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

type gateway struct {
	s   *server.Server
	srv *httptest.Server
	cfg *config.Config
	pc  config.ProviderConfig
}

func startGateway(t *testing.T, pc config.ProviderConfig, o server.Options) *gateway {
	t.Helper()
	return startGatewayAt(t, t.TempDir(), pc, o)
}

func startGatewayAt(t *testing.T, root string, pc config.ProviderConfig, o server.Options) *gateway {
	t.Helper()
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
	return &gateway{s: s, srv: h, cfg: cfg, pc: cfg.Providers["test"]}
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
		{http.MethodGet, "/v1/usage"},
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

func TestAbortKeepsPartialText(t *testing.T) {
	model := testmodel.Start([]testmodel.Step{{Text: "par", Hang: true}})
	defer model.Close()
	g := startGateway(t, modelConfig(model.URL), server.Options{})
	p, _, _ := remote(t, g, token)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	m, err := p.Stream(ctx, request(), func(d soma.Delta) {
		if d.Kind == soma.DeltaText && d.Text == "par" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("aborted stream error = %v, want the context's error", err)
	}
	if m.Text() != "par" || m.StopReason != soma.StopAborted || m.Role != soma.RoleAssistant {
		t.Fatalf("aborted message = %+v, want the partial text with stop reason aborted", m)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("abort took %v; the server did not end the stream", elapsed)
	}
}

func TestAbortUnknownStream(t *testing.T) {
	g := startGateway(t, modelConfig("http://127.0.0.1:1"), server.Options{})
	res := rawRequest(t, g, http.MethodPost, "/v1/abort", `{"stream":"no-such-stream"}`, strconv.Itoa(wire.Version), token)
	var e wire.Error
	err := json.NewDecoder(res.Body).Decode(&e)
	if res.StatusCode != http.StatusNotFound || err != nil || !strings.Contains(e.Message, "no-such-stream") {
		t.Fatalf("abort of an unknown stream: status %d, %+v, %v", res.StatusCode, e, err)
	}
}

type fakeClaude struct {
	command, log string
}

// newClaude writes a wrapper named claude that runs this test binary as the
// fake claude program with the given script. GORACE keeps a race-enabled
// fake from sleeping a second before it exits.
func newClaude(t *testing.T, script string) *fakeClaude {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c := &fakeClaude{command: filepath.Join(dir, "claude"), log: filepath.Join(dir, "claude.log")}
	scriptPath := filepath.Join(dir, "script.json")
	if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	wrapper := "#!/bin/sh\nGORACE=atexit_sleep_ms=0 SOMA_TEST_FAKE_CLAUDE=1 TESTCLAUDE_LOG=" + strconv.Quote(c.log) + " TESTCLAUDE_SCRIPT=" + strconv.Quote(scriptPath) + " exec " + strconv.Quote(self) + " \"$@\"\n"
	if err := os.WriteFile(c.command, []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *fakeClaude) config() config.ProviderConfig {
	return config.ProviderConfig{API: "claude-code", Command: c.command, Models: []config.ModelInfo{{ID: "m", ContextWindow: 200000}}}
}

type claudeStart struct {
	Pid  int      `json:"pid"`
	Args []string `json:"args"`
	Env  []string `json:"env"`
	Cwd  string   `json:"cwd"`
}

func (c *fakeClaude) starts(t *testing.T) []claudeStart {
	t.Helper()
	data, err := os.ReadFile(c.log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var out []claudeStart
	for _, line := range bytes.Split(data, []byte("\n")) {
		var rec struct {
			Event string `json:"event"`
			claudeStart
		}
		if len(line) == 0 || json.Unmarshal(line, &rec) != nil || rec.Event != "start" {
			continue
		}
		out = append(out, rec.claudeStart)
	}
	return out
}

func running(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitGone(t *testing.T, pid int, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for running(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("fake claude process %d still runs after %v", pid, limit)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func userMessage(text string) soma.Message {
	return soma.Message{Role: soma.RoleUser, Blocks: []soma.Block{{Type: soma.BlockText, Text: text}}}
}

// stamped is a reply as the session log keeps it: under the entry's name.
func stamped(m soma.Message) soma.Message {
	m.Provider = "home"
	return m
}

// streamBody is the body the client would send for req, with the entry's
// name already replaced by the upstream's.
func streamBody(t *testing.T, req soma.Request, afterFailed bool) []byte {
	t.Helper()
	messages := slices.Clone(req.Messages)
	for i := range messages {
		if messages[i].Provider == "home" {
			messages[i].Provider = "test"
		}
	}
	tools := make([]wire.Tool, len(req.Tools))
	for i, tool := range req.Tools {
		tools[i] = wire.Tool{Name: tool.Name, Description: tool.Description, Schema: tool.Schema}
	}
	body, err := json.Marshal(wire.StreamRequest{Upstream: "test", AfterFailedStream: afterFailed, Request: wire.Request{Model: req.Model, System: req.System,
		Messages: messages, Tools: tools, Thinking: req.Thinking, SessionID: req.SessionID, Epoch: req.Epoch}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// dropStream sends req as raw stream request, reads until a text delta with
// text, then closes the connection without an abort request.
func dropStream(t *testing.T, g *gateway, req soma.Request, text string) {
	t.Helper()
	body := streamBody(t, req, false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.srv.URL+"/v1/stream", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	hreq.Header.Set("Authorization", "Bearer "+token)
	hreq.Header.Set(wire.HeaderProtocol, strconv.Itoa(wire.Version))
	res, err := (&http.Client{Transport: &http.Transport{}}).Do(hreq)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	scan := bufio.NewScanner(res.Body)
	for scan.Scan() {
		var line wire.Line
		if err := json.Unmarshal(scan.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		if line.Done {
			t.Fatalf("stream ended before it could be dropped: %+v", line)
		}
		if line.Delta != nil && line.Delta.Text == text {
			return
		}
	}
	t.Fatalf("stream ended without the delta %q: %v", text, scan.Err())
}

func resumes(s claudeStart) bool { return slices.Contains(s.Args, "--resume") }

func TestDroppedStreamStartsNewConversation(t *testing.T) {
	c := newClaude(t, `{"turns":[[{"text":["first"]}],[{"text":["par"],"hang":true}]]}`)
	g := startGateway(t, c.config(), server.Options{})
	p, _, _ := remote(t, g, token)
	req := request()
	first, _, err := call(t, p, req)
	if err != nil || first.Text() != "first" {
		t.Fatalf("first reply = %+v, %v", first, err)
	}
	req.Messages = append(req.Messages, stamped(first), userMessage("continue"))
	dropStream(t, g, req, "par")
	retry, _, err := call(t, p, req)
	if err != nil || retry.Text() != "ok" {
		t.Fatalf("retry = %+v, %v", retry, err)
	}
	starts := c.starts(t)
	if len(starts) != 2 || resumes(starts[0]) || resumes(starts[1]) {
		t.Fatalf("after a dropped stream the retry must start a new conversation; starts = %+v", starts)
	}
}

func TestAfterFailedStreamFlag(t *testing.T) {
	c := newClaude(t, `{}`)
	first := startGateway(t, c.config(), server.Options{})
	p, _, _ := remote(t, first, token)
	req := request()
	reply, _, err := call(t, p, req)
	if err != nil {
		t.Fatal(err)
	}
	req.Messages = append(req.Messages, stamped(reply), userMessage("continue"))
	fresh := startGateway(t, c.config(), server.Options{})
	body := streamBody(t, req, true)
	res := rawRequest(t, fresh, http.MethodPost, "/v1/stream", string(body), strconv.Itoa(wire.Version), token)
	data, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK || !bytes.Contains(data, []byte(`"done":true`)) || bytes.Contains(data, []byte(`"error"`)) {
		t.Fatalf("stream after a failed one: status %d, %s, %v", res.StatusCode, data, err)
	}
	starts := c.starts(t)
	if len(starts) != 2 || resumes(starts[1]) {
		t.Fatalf("a request marked as following a failed stream must start a new conversation; starts = %+v", starts)
	}
}

func TestAbortDoesNotAbandon(t *testing.T) {
	c := newClaude(t, `{"turns":[[{"text":["first"]}],[{"text":["par"],"hang":true}]]}`)
	g := startGateway(t, c.config(), server.Options{})
	p, cfg, pc := remote(t, g, token)
	if _, err := provider.FetchModels(context.Background(), cfg, pc); err != nil {
		t.Fatal(err)
	}
	req := request()
	first, _, err := call(t, p, req)
	if err != nil {
		t.Fatal(err)
	}
	req.Messages = append(req.Messages, stamped(first), userMessage("continue"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	partial, err := p.Stream(ctx, req, func(d soma.Delta) {
		if d.Kind == soma.DeltaText && d.Text == "par" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || partial.Text() != "par" {
		t.Fatalf("aborted reply = %+v, %v", partial, err)
	}
	p.(soma.RunEnder).EndRun(req.SessionID)
	req.Messages = append(req.Messages, stamped(partial), userMessage("go on"))
	if _, _, err := call(t, p, req); err != nil {
		t.Fatal(err)
	}
	starts := c.starts(t)
	if len(starts) != 2 || !resumes(starts[1]) {
		t.Fatalf("after an abort the next request must resume the conversation; starts = %+v", starts)
	}
}

func TestGraceEndsHeldProcess(t *testing.T) {
	c := newClaude(t, `{}`)
	g := startGateway(t, c.config(), server.Options{Grace: 50 * time.Millisecond})
	p, _, _ := remote(t, g, token)
	if _, _, err := call(t, p, request()); err != nil {
		t.Fatal(err)
	}
	starts := c.starts(t)
	if len(starts) != 1 {
		t.Fatalf("starts = %+v", starts)
	}
	waitGone(t, starts[0].Pid, time.Second)
}

func TestShutdownEndsProcesses(t *testing.T) {
	c := newClaude(t, `{}`)
	g := startGateway(t, c.config(), server.Options{})
	p, _, _ := remote(t, g, token)
	if _, _, err := call(t, p, request()); err != nil {
		t.Fatal(err)
	}
	starts := c.starts(t)
	if len(starts) != 1 || !running(starts[0].Pid) {
		t.Fatalf("no held process before shutdown: %+v", starts)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := g.s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if running(starts[0].Pid) {
		t.Fatal("Shutdown returned while the held claude process still runs")
	}
	_, _, err := call(t, p, request())
	var pe *soma.ProviderError
	if !errors.As(err, &pe) || pe.Status != http.StatusServiceUnavailable {
		t.Fatalf("request to a shut-down handler = %v, want its refusal", err)
	}
	g.srv.Close()
	_, _, err = call(t, p, request())
	if !errors.As(err, &pe) || pe.Kind != soma.ErrNetwork {
		t.Fatalf("request after the listener closed = %v, want a connection error", err)
	}
	if len(c.starts(t)) != 1 {
		t.Fatal("a request after shutdown started claude")
	}
}

func TestClientMarksRetryAfterFailedStream(t *testing.T) {
	var mu sync.Mutex
	var flags []bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body wire.StreamRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		flags = append(flags, body.AfterFailedStream)
		n := len(flags)
		mu.Unlock()
		w.Header().Set(wire.HeaderProtocol, strconv.Itoa(wire.Version))
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, "{\"stream\":\"id\"}\n")
		if n != 1 {
			io.WriteString(w, "{\"done\":true,\"message\":{\"Role\":\"assistant\"}}\n")
		}
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
		t.Fatalf("broken stream = %v", err)
	}
	for range 2 {
		if _, _, err := call(t, p, request()); err != nil {
			t.Fatal(err)
		}
	}
	other := request()
	other.SessionID = "other"
	if _, _, err := call(t, p, other); err != nil {
		t.Fatal(err)
	}
	if want := []bool{false, true, false, false}; !reflect.DeepEqual(flags, want) {
		t.Fatalf("afterFailedStream per request = %v, want %v", flags, want)
	}
}

// serveRoot starts a server on root as it is, with or without models.json.
func serveRoot(t *testing.T, root string, o server.Options) *gateway {
	t.Helper()
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
	return &gateway{s: s, srv: h}
}

func remoteUpstream(t *testing.T, g *gateway, upstream string) soma.Provider {
	t.Helper()
	pc := config.ProviderConfig{Name: "home", API: "soma", BaseURL: g.srv.URL, APIKey: token, Upstream: upstream}
	cfg := &config.Config{Root: t.TempDir(), Providers: map[string]config.ProviderConfig{pc.Name: pc}}
	p, err := provider.New(cfg, pc)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func saveLogin(t *testing.T, root string) {
	t.Helper()
	expires := time.Now().Add(time.Hour)
	if err := auth.SaveCodex(root, auth.Codex{Type: "oauth", Access: testauth.JWT("acct_test", expires), Refresh: "rt-1",
		Expires: expires.UnixMilli(), AccountID: "acct_test"}); err != nil {
		t.Fatal(err)
	}
}

func writeModels(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "models.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLoginAfterStart(t *testing.T) {
	program := soma.Program
	soma.Program = "soma"
	t.Cleanup(func() { soma.Program = program })
	model := testmodel.Start([]testmodel.Step{{Text: "logged in"}})
	defer model.Close()
	root := t.TempDir()
	writeModels(t, root, `{"providers":{"chatgpt":{"api":"codex","baseUrl":`+strconv.Quote(model.URL)+`}}}`)
	g := serveRoot(t, root, server.Options{})
	p := remoteUpstream(t, g, "chatgpt")
	_, _, err := call(t, p, request())
	var pe *soma.ProviderError
	if !errors.As(err, &pe) || pe.Kind != soma.ErrAuth || !strings.Contains(pe.Message, "soma login codex") {
		t.Fatalf("request without a login = %v", err)
	}
	saveLogin(t, root)
	m, _, err := call(t, p, request())
	if err != nil || m.Text() != "logged in" {
		t.Fatalf("request after the login = %+v, %v", m, err)
	}
}

func TestNoModelsFileOffersDefaults(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	model := testmodel.Start(nil)
	defer model.Close()
	base := openai.DefaultCodexBaseURL
	openai.DefaultCodexBaseURL = model.URL
	t.Cleanup(func() { openai.DefaultCodexBaseURL = base })
	root := t.TempDir()
	g := serveRoot(t, root, server.Options{})
	info := func() (int, wire.Info) {
		res := rawRequest(t, g, http.MethodGet, "/v1/info?upstream=chatgpt", "", strconv.Itoa(wire.Version), token)
		var out wire.Info
		if res.StatusCode == http.StatusOK {
			if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
		}
		return res.StatusCode, out
	}
	if status, _ := info(); status != http.StatusNotFound {
		t.Fatalf("chatgpt without a login: status %d, want 404", status)
	}
	saveLogin(t, root)
	status, got := info()
	if status != http.StatusOK || len(got.Models) == 0 || got.HoldsConversation {
		t.Fatalf("chatgpt after a login: status %d, %+v", status, got)
	}
	if _, err := os.Stat(filepath.Join(root, "models.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the server wrote models.json: %v", err)
	}
}

const codexModelList = `{"models":[` +
	`{"slug":"later","context_window":272000,"max_context_window":872000,"visibility":"list","priority":3,` +
	`"supported_reasoning_levels":[{"effort":"medium"},{"effort":"high"},{"effort":"max"}]},` +
	`{"slug":"hidden","context_window":272000,"visibility":"hide","priority":0,"supported_reasoning_levels":[{"effort":"low"}]},` +
	`{"slug":"first","context_window":500000,"max_context_window":872000,"visibility":"list","priority":1,"model_messages":{"intro":"long"},` +
	`"supported_reasoning_levels":[{"effort":"low","description":"Fast"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}]},` +
	`{"slug":"plain","context_window":128000,"visibility":"list","priority":2,"supported_reasoning_levels":[]}]}`

func TestInfoListsCodexModels(t *testing.T) {
	model := testmodel.Start(nil)
	defer model.Close()
	model.SetModels(codexModelList)
	root := t.TempDir()
	writeModels(t, root, `{"providers":{"chatgpt":{"api":"codex","baseUrl":`+strconv.Quote(model.URL)+`,"headers":{"X-Extra":"1"}},`+
		`"fixed":{"api":"codex","baseUrl":`+strconv.Quote(model.URL)+`,"models":[{"id":"mine"}]}}}`)
	saveLogin(t, root)
	login, _, err := auth.LoadCodex(root)
	if err != nil {
		t.Fatal(err)
	}
	g := serveRoot(t, root, server.Options{})
	info := func(upstream string) wire.Info {
		t.Helper()
		res := rawRequest(t, g, http.MethodGet, "/v1/info?upstream="+upstream, "", strconv.Itoa(wire.Version), token)
		var out wire.Info
		if res.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(res.Body)
			t.Fatalf("info of %s: status %d, %s", upstream, res.StatusCode, body)
		}
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	off := false
	want := []config.ModelInfo{
		{ID: "first", ContextWindow: 500000, Thinking: []string{"minimal", "low", "medium", "high", "xhigh"}, Effort: map[string]string{"minimal": "low"}},
		{ID: "plain", ContextWindow: 128000, Reasoning: &off},
		{ID: "later", ContextWindow: 272000, Thinking: []string{"medium", "high"}},
	}
	if got := info("chatgpt"); !reflect.DeepEqual(got.Models, want) || got.HoldsConversation {
		t.Fatalf("info = %+v, want models %+v", got, want)
	}
	reqs := model.ModelRequests()
	if len(reqs) != 1 {
		t.Fatalf("%d model list requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.URL.Path != "/codex/models" || r.URL.Query().Get("client_version") != openai.CodexClientVersion || openai.CodexClientVersion != "0.169.0" {
		t.Errorf("model list request %s, want /codex/models?client_version=0.169.0", r.URL)
	}
	for k, v := range map[string]string{"Authorization": "Bearer " + login.Access, "Chatgpt-Account-Id": "acct_test", "X-Extra": "1"} {
		if got := r.Header.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, soma.Program+"/") {
		t.Errorf("User-Agent = %q", ua)
	}
	if got := info("fixed"); len(got.Models) != 1 || got.Models[0].ID != "mine" {
		t.Errorf("explicit models: info = %+v", got)
	}
	info("chatgpt")
	if n := len(model.ModelRequests()); n != 1 || len(model.Requests()) != 0 {
		t.Errorf("%d model list requests and %d model requests after a cached list and explicit models, want 1 and 0", n, len(model.Requests()))
	}
}

func TestSomaUpstreamRefused(t *testing.T) {
	var mu sync.Mutex
	reached := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reached++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer target.Close()
	g := startGateway(t, config.ProviderConfig{API: "soma", BaseURL: target.URL, APIKey: token, Upstream: "other"}, server.Options{})
	p, _, _ := remote(t, g, token)
	_, _, err := call(t, p, request())
	var pe *soma.ProviderError
	if !errors.As(err, &pe) || pe.Status != http.StatusBadRequest || !strings.Contains(pe.Message, "soma") {
		t.Fatalf("stream to a soma upstream = %v", err)
	}
	res := rawRequest(t, g, http.MethodGet, "/v1/info?upstream=test", "", strconv.Itoa(wire.Version), token)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("info of a soma upstream: status %d", res.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if reached != 0 {
		t.Fatalf("the server forwarded %d requests to another soma server", reached)
	}
}

func realPath(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClaudeRunsInWorkDir(t *testing.T) {
	c := newClaude(t, `{}`)
	g := startGateway(t, c.config(), server.Options{})
	p, _, _ := remote(t, g, token)
	if _, _, err := call(t, p, request()); err != nil {
		t.Fatal(err)
	}
	starts := c.starts(t)
	want := filepath.Join(realPath(t, g.cfg.Root), "claude-work")
	if len(starts) != 1 {
		t.Fatalf("starts = %+v", starts)
	}
	if starts[0].Cwd == "" || realPath(t, starts[0].Cwd) != want {
		t.Fatalf("claude ran in %s, want %s", starts[0].Cwd, want)
	}
}

func TestSecretsNotInChild(t *testing.T) {
	t.Setenv("SOMA_TOKEN", "server-token")
	t.Setenv("TS_AUTHKEY", "tailnet-key")
	c := newClaude(t, `{}`)
	startGateway(t, c.config(), server.Options{})
	g := startGateway(t, c.config(), server.Options{})
	p, _, _ := remote(t, g, token)
	if _, _, err := call(t, p, request()); err != nil {
		t.Fatal(err)
	}
	starts := c.starts(t)
	if len(starts) != 1 || len(starts[0].Env) == 0 {
		t.Fatalf("starts = %+v", starts)
	}
	for _, name := range []string{"SOMA_TOKEN", "TS_AUTHKEY"} {
		if slices.Contains(starts[0].Env, name) {
			t.Errorf("the claude child received %s", name)
		}
		if n := len(slices.DeleteFunc(slices.Clone(claudecode.StripEnv), func(s string) bool { return s != name })); n > 1 {
			t.Errorf("claudecode.StripEnv holds %s %d times after two servers", name, n)
		}
	}
}

// touch marks models.json as changed without depending on the clock's
// resolution.
func touch(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "models.json")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	later := fi.ModTime().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

func TestReloadKeepsHeldProcess(t *testing.T) {
	c := newClaude(t, `{}`)
	g := startGateway(t, c.config(), server.Options{})
	p, _, _ := remote(t, g, token)
	req := request()
	first, _, err := call(t, p, req)
	if err != nil {
		t.Fatal(err)
	}
	touch(t, g.cfg.Root)
	req.Messages = append(req.Messages, stamped(first), userMessage("again"))
	if _, _, err := call(t, p, req); err != nil {
		t.Fatal(err)
	}
	other := request()
	other.SessionID = "other"
	if _, _, err := call(t, p, other); err != nil {
		t.Fatal(err)
	}
	starts := c.starts(t)
	if len(starts) != 2 || resumes(starts[1]) {
		t.Fatalf("a reload must keep the held process of a run; starts = %+v", starts)
	}
	touch(t, g.cfg.Root)
	res := rawRequest(t, g, http.MethodPost, "/v1/endrun", `{"upstream":"test","sessionId":"session"}`, strconv.Itoa(wire.Version), token)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("endrun status %d", res.StatusCode)
	}
	waitGone(t, starts[0].Pid, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := g.s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if running(starts[1].Pid) {
		t.Fatal("Shutdown left the process of a run started before a reload")
	}
}

func TestReloadKeepsAbandonMark(t *testing.T) {
	c := newClaude(t, `{"turns":[[{"text":["first"]}],[{"text":["par"],"hang":true}]]}`)
	g := startGateway(t, c.config(), server.Options{})
	p, _, _ := remote(t, g, token)
	req := request()
	first, _, err := call(t, p, req)
	if err != nil {
		t.Fatal(err)
	}
	req.Messages = append(req.Messages, stamped(first), userMessage("continue"))
	dropStream(t, g, req, "par")
	touch(t, g.cfg.Root)
	retry, _, err := call(t, p, req)
	if err != nil || retry.Text() != "ok" {
		t.Fatalf("retry = %+v, %v", retry, err)
	}
	starts := c.starts(t)
	if len(starts) != 2 || resumes(starts[1]) {
		t.Fatalf("a reload must not let the retry of an abandoned run resume it; starts = %+v", starts)
	}
}

func usagePage(t *testing.T, g *gateway, after string) wire.Usage {
	t.Helper()
	path := "/v1/usage"
	if after != "" {
		path += "?after=" + after
	}
	res := rawRequest(t, g, http.MethodGet, path, "", strconv.Itoa(wire.Version), token)
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("GET %s: status %d, %s", path, res.StatusCode, body)
	}
	var u wire.Usage
	if err := json.NewDecoder(res.Body).Decode(&u); err != nil {
		t.Fatal(err)
	}
	return u
}

func seqs(u wire.Usage) []int64 {
	var out []int64
	for _, e := range u.Entries {
		out = append(out, e.Seq)
	}
	return out
}

func steps(n int) []testmodel.Step {
	out := make([]testmodel.Step, n)
	for i := range out {
		out[i] = testmodel.Step{Text: "secret reply", InputTokens: 100 + i}
	}
	return out
}

func requests(t *testing.T, g *gateway, n int) {
	t.Helper()
	p, _, _ := remote(t, g, token)
	for range n {
		if _, _, err := call(t, p, request()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUsageLogRecordsCompletedRequest(t *testing.T) {
	model := testmodel.Start([]testmodel.Step{{Text: "secret reply", InputTokens: 120}})
	defer model.Close()
	g := startGateway(t, modelConfig(model.URL), server.Options{})
	before := time.Now().UTC().Add(-time.Second)
	requests(t, g, 1)
	u := usagePage(t, g, "")
	if len(u.ID) < 16 || u.First != 1 || u.Next != 1 || len(u.Entries) != 1 {
		t.Fatalf("usage = %+v, want one entry with seq 1", u)
	}
	e := u.Entries[0]
	if e.Seq != 1 || e.Upstream != "test" || e.API != "openai-responses" || e.Model != "m" || e.SessionID != "session" ||
		e.Usage.Input != 120 || e.Usage.Output != 10 || e.Usage.Cost <= 0 || e.StopReason != soma.StopEnd ||
		e.Time.Location() != time.UTC || e.Time.Before(before) || e.Time.After(time.Now().Add(time.Second)) {
		t.Fatalf("entry = %+v", e)
	}
	path := filepath.Join(g.cfg.Root, "usage.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("usage log mode = %v, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("secret")) || bytes.Contains(data, []byte("hello")) || bytes.Contains(data, []byte("system")) {
		t.Fatalf("usage log holds message content: %s", data)
	}
	t.Logf("usage.jsonl:\n%s", data)
	res := rawRequest(t, g, http.MethodGet, "/v1/usage", "", strconv.Itoa(wire.Version), token)
	body, _ := io.ReadAll(res.Body)
	t.Logf("GET /v1/usage: %s", body)
}

type abortingProvider struct{}

func init() {
	provider.Register("test-aborting", func(*config.Config, config.ProviderConfig) (soma.Provider, error) {
		return abortingProvider{}, nil
	})
}

func (abortingProvider) Stream(ctx context.Context, _ soma.Request, onDelta func(soma.Delta)) (soma.Message, error) {
	onDelta(soma.Delta{Kind: soma.DeltaText, Text: "par"})
	<-ctx.Done()
	return soma.Message{Role: soma.RoleAssistant, Blocks: []soma.Block{{Type: soma.BlockText, Text: "par"}},
		Usage: &soma.Usage{Input: 7, CacheRead: 3}, StopReason: soma.StopAborted}, ctx.Err()
}

// The model fakes report no usage for a reply cut short, so a provider
// registered here stands in for one that does.
func TestUsageLogRecordsAbortedRequest(t *testing.T) {
	g := startGateway(t, config.ProviderConfig{API: "test-aborting", Models: []config.ModelInfo{{ID: "m", ContextWindow: 1000}}}, server.Options{})
	p, _, _ := remote(t, g, token)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := p.Stream(ctx, request(), func(d soma.Delta) {
		if d.Text == "par" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("aborted stream error = %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		u := usagePage(t, g, "")
		if len(u.Entries) == 1 {
			e := u.Entries[0]
			if e.StopReason != soma.StopAborted || e.Usage.Input != 7 || e.Usage.CacheRead != 3 || e.API != "test-aborting" {
				t.Fatalf("aborted entry = %+v", e)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no usage entry for the aborted request: %+v", u)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUsageLogSkipsReplyWithoutUsage(t *testing.T) {
	g := startGateway(t, config.ProviderConfig{API: "test-holder", Models: []config.ModelInfo{{ID: "m", ContextWindow: 1000}}}, server.Options{})
	p, _, _ := remote(t, g, token)
	req := request()
	req.Tools = nil
	if _, _, err := call(t, p, req); err != nil {
		t.Fatal(err)
	}
	if u := usagePage(t, g, ""); len(u.Entries) != 0 || u.First != 0 || u.Next != 0 {
		t.Fatalf("usage = %+v, want no entry for a reply without usage", u)
	}
}

func TestUsageNeedsToken(t *testing.T) {
	g := startGateway(t, modelConfig("http://127.0.0.1:1"), server.Options{})
	for _, key := range []string{"", "wrong"} {
		res := rawRequest(t, g, http.MethodGet, "/v1/usage", "", strconv.Itoa(wire.Version), key)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q: status %d", key, res.StatusCode)
		}
	}
	if res := rawRequest(t, g, http.MethodGet, "/v1/usage", "", "", token); res.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("without a protocol version: status %d", res.StatusCode)
	}
}

func TestUsagePagesAfter(t *testing.T) {
	model := testmodel.Start(steps(3))
	defer model.Close()
	g := startGateway(t, modelConfig(model.URL), server.Options{})
	requests(t, g, 3)
	for _, c := range []struct {
		after string
		want  []int64
		next  int64
	}{
		{"", []int64{1, 2, 3}, 3},
		{"0", []int64{1, 2, 3}, 3},
		{"1", []int64{2, 3}, 3},
		{"3", nil, 3},
	} {
		u := usagePage(t, g, c.after)
		if !slices.Equal(seqs(u), c.want) || u.Next != c.next || u.First != 1 || u.Entries == nil {
			t.Errorf("after %q: seqs %v, first %d, next %d; want %v, 1, %d", c.after, seqs(u), u.First, u.Next, c.want, c.next)
		}
	}
	if u := usagePage(t, g, "1"); u.Entries[0].Usage.Input != 101 || u.Entries[1].Usage.Input != 102 {
		t.Fatalf("entries out of order: %+v", u.Entries)
	}
}

func TestUsageBadAfter(t *testing.T) {
	g := startGateway(t, modelConfig("http://127.0.0.1:1"), server.Options{})
	for _, after := range []string{"x", "-1", "1.5", "99999999999999999999"} {
		res := rawRequest(t, g, http.MethodGet, "/v1/usage?after="+after, "", strconv.Itoa(wire.Version), token)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("after %q: status %d", after, res.StatusCode)
		}
	}
}

func TestUsageClientAheadOfLog(t *testing.T) {
	model := testmodel.Start(steps(2))
	defer model.Close()
	g := startGateway(t, modelConfig(model.URL), server.Options{})
	if u := usagePage(t, g, "5"); u.ID == "" || u.First != 0 || u.Next != 0 || len(u.Entries) != 0 {
		t.Fatalf("empty log, after 5: %+v", u)
	}
	requests(t, g, 2)
	if u := usagePage(t, g, "5"); u.First != 1 || u.Next != 2 || len(u.Entries) != 0 {
		t.Fatalf("after 5 with newest 2: %+v", u)
	}
}

func TestUsageResponseCapped(t *testing.T) {
	root := t.TempDir()
	var log bytes.Buffer
	log.WriteString(`{"id":"0123456789abcdef0123456789abcdef"}` + "\n")
	const n = 3000
	long := strings.Repeat("s", 1000)
	for i := 1; i <= n; i++ {
		line, err := json.Marshal(wire.UsageEntry{Seq: int64(i), Time: time.Now().UTC(), Upstream: "test", API: "openai-responses", Model: "m", SessionID: long, Usage: soma.Usage{Input: i}, StopReason: soma.StopEnd})
		if err != nil {
			t.Fatal(err)
		}
		log.Write(line)
		log.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(root, "usage.jsonl"), log.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	g := startGatewayAt(t, root, modelConfig("http://127.0.0.1:1"), server.Options{})
	var got []int64
	after, pages := int64(0), 0
	for after < n {
		res := rawRequest(t, g, http.MethodGet, "/v1/usage?after="+strconv.FormatInt(after, 10), "", strconv.Itoa(wire.Version), token)
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > 1<<20+4096 {
			t.Fatalf("page of %d bytes exceeds the cap", len(body))
		}
		var u wire.Usage
		if err := json.Unmarshal(body, &u); err != nil {
			t.Fatal(err)
		}
		if u.ID != "0123456789abcdef0123456789abcdef" || u.First != 1 || len(u.Entries) == 0 || u.Next != u.Entries[len(u.Entries)-1].Seq {
			t.Fatalf("page after %d: id %q, first %d, next %d, %d entries", after, u.ID, u.First, u.Next, len(u.Entries))
		}
		got = append(got, seqs(u)...)
		after = u.Next
		pages++
	}
	if pages < 3 || len(got) != n || got[0] != 1 || got[n-1] != n {
		t.Fatalf("%d pages, %d entries", pages, len(got))
	}
	for i, s := range got {
		if s != int64(i+1) {
			t.Fatalf("entry %d has seq %d", i, s)
		}
	}
}

func TestUsageLogTrimmed(t *testing.T) {
	model := testmodel.Start(steps(21))
	defer model.Close()
	const limit = 2000
	g := startGateway(t, modelConfig(model.URL), server.Options{UsageLogBytes: limit})
	requests(t, g, 1)
	id := usagePage(t, g, "").ID
	requests(t, g, 19)
	info, err := os.Stat(filepath.Join(g.cfg.Root, "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > limit || info.Mode().Perm() != 0600 {
		t.Fatalf("usage log is %d bytes, mode %v; limit %d", info.Size(), info.Mode().Perm(), limit)
	}
	u := usagePage(t, g, "")
	if u.ID != id || u.First <= 1 || u.Next != 20 || len(u.Entries) == 0 || u.Entries[0].Seq != u.First {
		t.Fatalf("after trimming: id %q (was %q), first %d, next %d, seqs %v", u.ID, id, u.First, u.Next, seqs(u))
	}
	for i, e := range u.Entries {
		if e.Seq != u.First+int64(i) || e.Usage.Input != 100+int(e.Seq)-1 {
			t.Fatalf("after trimming: entry %d = %+v", i, e)
		}
	}
	requests(t, g, 1)
	if u := usagePage(t, g, "20"); !slices.Equal(seqs(u), []int64{21}) || u.ID != id {
		t.Fatalf("after trimming the next seq is %v, id %q", seqs(u), u.ID)
	}
}

func TestUsageLogSurvivesRestart(t *testing.T) {
	model := testmodel.Start(steps(3))
	defer model.Close()
	root := t.TempDir()
	first := startGatewayAt(t, root, modelConfig(model.URL), server.Options{})
	requests(t, first, 1)
	id := usagePage(t, first, "").ID
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := first.s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	first.srv.Close()
	again := startGatewayAt(t, root, modelConfig(model.URL), server.Options{})
	requests(t, again, 1)
	if u := usagePage(t, again, ""); u.ID != id || !slices.Equal(seqs(u), []int64{1, 2}) {
		t.Fatalf("after a restart: id %q (was %q), seqs %v", u.ID, id, seqs(u))
	}
	fresh := startGateway(t, modelConfig(model.URL), server.Options{})
	requests(t, fresh, 1)
	if u := usagePage(t, fresh, ""); u.ID == "" || u.ID == id || !slices.Equal(seqs(u), []int64{1}) {
		t.Fatalf("on a fresh root: id %q (other %q), seqs %v", u.ID, id, seqs(u))
	}
}
