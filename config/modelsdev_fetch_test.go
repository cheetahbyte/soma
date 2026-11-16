package config

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

const fakeCatalog = `{"skip":{"models":{}},"openai":{"models":{"m":{"limit":{"context":500,"output":50},"cost":{"input":1,"output":2,"cache_read":0.1}}}}}`

type fakeCatalogServer struct {
	mu     sync.Mutex
	status int
	reqs   []http.Header
	srv    *httptest.Server
}

func startFake(t *testing.T) *fakeCatalogServer {
	f := &fakeCatalogServer{status: 200}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reqs = append(f.reqs, r.Header.Clone())
		if f.status != 200 {
			w.WriteHeader(f.status)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte(fakeCatalog))
	}))
	old := ModelsDevURL
	ModelsDevURL = f.srv.URL
	t.Cleanup(func() { ModelsDevURL = old; f.srv.Close() })
	return f
}

func TestFetchModelsDevConditionalAndFailure(t *testing.T) {
	f := startFake(t)
	cfg := &Config{Root: t.TempDir()}
	if err := FetchModelsDev(cfg, "openai", time.Second); err != nil {
		t.Fatal(err)
	}
	md := cfg.ModelsDev("openai")
	if md.ETag != `"v1"` || md.Models["m"].ContextWindow != 500 || md.Error != "" {
		t.Fatalf("cache = %+v", md)
	}
	h := f.reqs[0]
	if h.Get("Authorization") != "" || h.Get("Cookie") != "" || h.Get("User-Agent") == "" || h.Get("If-None-Match") != "" {
		t.Errorf("first request headers = %v", h)
	}

	md.Fetched = md.Fetched.Add(-48 * time.Hour)
	if err := FetchModelsDev(cfg, "openai", time.Second); err != nil {
		t.Fatal(err)
	}
	if f.reqs[1].Get("If-None-Match") != `"v1"` {
		t.Errorf("second request sent no If-None-Match: %v", f.reqs[1])
	}
	if got := cfg.ModelsDev("openai"); time.Since(got.Fetched) > time.Minute || got.Models["m"].InputPrice != 1 {
		t.Errorf("after 304: %+v", got)
	}

	f.mu.Lock()
	f.status = 500
	f.mu.Unlock()
	if err := FetchModelsDev(cfg, "openai", time.Second); err == nil {
		t.Fatal("HTTP 500: no error")
	}
	got := LoadModelsDev(cfg.Root, "openai")
	if got.Error == "" || got.Models["m"].InputPrice != 1 || ShouldFetchModelsDev(got, time.Now()) {
		t.Errorf("after failure: %+v, want old data kept and a retry later", got)
	}
}

func TestFetchModelsDevFailureWithoutCache(t *testing.T) {
	f := startFake(t)
	f.status = 503
	cfg := &Config{Root: t.TempDir(), Providers: map[string]ProviderConfig{"p": {Name: "p", ModelsDev: "openai"}}}
	RefreshModelsDev(cfg)
	if md := cfg.ModelsDev("openai"); md == nil || md.Error == "" || len(md.Models) != 0 {
		t.Errorf("cache = %+v, want a failed fetch without model data", md)
	}
	RefreshModelsDev(cfg)
	if len(f.reqs) != 1 {
		t.Errorf("%d requests, want the retry to wait ten minutes", len(f.reqs))
	}
}

func TestRefreshModelsDevSkipsProvidersWithoutField(t *testing.T) {
	f := startFake(t)
	RefreshModelsDev(&Config{Root: t.TempDir(), Providers: map[string]ProviderConfig{"p": {Name: "p"}}})
	if len(f.reqs) != 0 {
		t.Errorf("%d requests without an opted-in provider", len(f.reqs))
	}
}
