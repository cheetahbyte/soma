package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cheetahbyte/soma/config"
	"github.com/cheetahbyte/soma/provider"
	"github.com/cheetahbyte/soma/server"
	testmodel "github.com/cheetahbyte/soma/somatest/model"
)

func pricingCatalog(t *testing.T, status int) *atomic.Int32 {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("provider credentials reached the pricing catalog")
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			fmt.Fprint(w, `{"openai":{"models":{"m":{"cost":{"input":2,"output":10,"cache_read":0.1}}}}}`)
		}
	}))
	old := config.ModelsDevURL
	config.ModelsDevURL = srv.URL
	t.Cleanup(func() { config.ModelsDevURL = old; srv.Close() })
	return &requests
}

func TestModelsDevPricesReachRemoteReplies(t *testing.T) {
	for _, first := range []string{"stream", "info"} {
		t.Run(first+" first", func(t *testing.T) {
			fetches := pricingCatalog(t, http.StatusOK)
			model := testmodel.Start([]testmodel.Step{{Text: "priced reply", InputTokens: 6289}})
			defer model.Close()
			pc := modelConfig(model.URL)
			pc.ModelsDev = "openai"
			pc.Models[0].InputPrice, pc.Models[0].OutputPrice = 0, 0
			g := startGateway(t, pc, server.Options{})
			p, cfg, remotePC := remote(t, g, token)
			if first == "info" {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				models, err := provider.FetchModels(ctx, cfg, remotePC)
				if err != nil {
					t.Fatal(err)
				}
				if len(models) != 1 || models[0].InputPrice != 2 || models[0].OutputPrice != 10 || models[0].CachedPrice != 0.1 {
					t.Fatalf("server model prices = %+v", models)
				}
			}
			m, _, err := call(t, p, request())
			if err != nil {
				t.Fatal(err)
			}
			const wantCost = (6289*2 + 10*10) / 1e6
			if m.Usage == nil || m.Usage.Input != 6289 || m.Usage.Output != 10 || m.Usage.Cost != wantCost {
				t.Fatalf("remote usage = %+v, want 6289 input, 10 output, cost %v", m.Usage, wantCost)
			}
			usage := usagePage(t, g, "")
			if len(usage.Entries) != 1 || usage.Entries[0].Usage.Cost != wantCost {
				t.Fatalf("server usage = %+v, want cost %v", usage, wantCost)
			}
			if fetches.Load() != 1 {
				t.Fatalf("catalog fetched %d times, want once", fetches.Load())
			}
		})
	}
}

func TestModelsDevRefreshOnRequests(t *testing.T) {
	for _, tc := range []struct {
		name      string
		age       time.Duration
		status    int
		wantCost  float64
		wantFetch int32
		wantError bool
	}{
		{name: "fresh", age: time.Hour, status: http.StatusOK, wantCost: 0.0001},
		{name: "stale", age: 25 * time.Hour, status: http.StatusOK, wantCost: 0.0003, wantFetch: 1},
		{name: "failure keeps old rates", age: 25 * time.Hour, status: http.StatusServiceUnavailable, wantCost: 0.0001, wantFetch: 1, wantError: true},
		{name: "failure without old rates", status: http.StatusServiceUnavailable, wantFetch: 1, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetches := pricingCatalog(t, tc.status)
			root := t.TempDir()
			if tc.age > 0 {
				if err := config.SaveModelsDev(root, "openai", config.ModelsDevCache{
					Fetched: time.Now().Add(-tc.age),
					Models:  map[string]config.ModelsDevModel{"m": {InputPrice: 1}},
				}); err != nil {
					t.Fatal(err)
				}
			}
			model := testmodel.Start([]testmodel.Step{{Text: "one"}, {Text: "two"}})
			defer model.Close()
			pc := modelConfig(model.URL)
			pc.ModelsDev = "openai"
			pc.Models[0].InputPrice, pc.Models[0].OutputPrice = 0, 0
			g := startGatewayAt(t, root, pc, server.Options{})
			p, _, _ := remote(t, g, token)
			for range 2 {
				m, _, err := call(t, p, request())
				if err != nil {
					t.Fatal(err)
				}
				if m.Usage == nil || m.Usage.Cost != tc.wantCost {
					t.Fatalf("usage = %+v, want cost %v", m.Usage, tc.wantCost)
				}
			}
			if fetches.Load() != tc.wantFetch {
				t.Fatalf("catalog fetched %d times, want %d", fetches.Load(), tc.wantFetch)
			}
			cached := config.LoadModelsDev(root, "openai")
			if cached == nil || (cached.Error != "") != tc.wantError {
				t.Fatalf("cached pricing = %+v, want error %v", cached, tc.wantError)
			}
		})
	}
}

func TestModelsDevPricingAfterReload(t *testing.T) {
	fetches := pricingCatalog(t, http.StatusOK)
	model := testmodel.Start([]testmodel.Step{{Text: "before"}, {Text: "after"}})
	defer model.Close()
	pc := modelConfig(model.URL)
	pc.Models[0].InputPrice, pc.Models[0].OutputPrice = 0, 0
	g := startGateway(t, pc, server.Options{})
	p, _, _ := remote(t, g, token)
	before, _, err := call(t, p, request())
	if err != nil {
		t.Fatal(err)
	}
	if before.Usage == nil || before.Usage.Cost != 0 || fetches.Load() != 0 {
		t.Fatalf("before opt-in: usage %+v, catalog fetches %d", before.Usage, fetches.Load())
	}
	pc.ModelsDev = "openai"
	data, err := json.Marshal(map[string]any{"providers": map[string]config.ProviderConfig{"test": pc}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.cfg.Root, "models.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	after, _, err := call(t, p, request())
	if err != nil {
		t.Fatal(err)
	}
	if after.Usage == nil || after.Usage.Cost != 0.0003 || fetches.Load() != 1 {
		t.Fatalf("after opt-in: usage %+v, catalog fetches %d", after.Usage, fetches.Load())
	}
}

func TestModelsDevConcurrentRequestsShareRefresh(t *testing.T) {
	fetches := pricingCatalog(t, http.StatusOK)
	const workers = 8
	script := make([]testmodel.Step, workers)
	for i := range script {
		script[i].Text = "priced reply"
	}
	model := testmodel.Start(script)
	defer model.Close()
	pc := modelConfig(model.URL)
	pc.ModelsDev = "openai"
	pc.Models[0].InputPrice, pc.Models[0].OutputPrice = 0, 0
	g := startGateway(t, pc, server.Options{})
	p, _, _ := remote(t, g, token)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	errs := make(chan error, workers)
	start := make(chan struct{})
	for i := range workers {
		go func() {
			<-start
			req := request()
			req.SessionID = fmt.Sprintf("session-%d", i)
			m, err := p.Stream(ctx, req, nil)
			if err == nil && (m.Usage == nil || m.Usage.Cost != 0.0003) {
				err = fmt.Errorf("usage = %+v, want cost 0.0003", m.Usage)
			}
			errs <- err
		}()
	}
	close(start)
	for range workers {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	if fetches.Load() != 1 {
		t.Fatalf("catalog fetched %d times for concurrent requests, want once", fetches.Load())
	}
}

func TestModelsDevServerRequiresOptIn(t *testing.T) {
	fetches := pricingCatalog(t, http.StatusOK)
	model := testmodel.Start([]testmodel.Step{{Text: "unpriced reply"}})
	defer model.Close()
	pc := modelConfig(model.URL)
	pc.Models[0].InputPrice, pc.Models[0].OutputPrice = 0, 0
	g := startGateway(t, pc, server.Options{})
	p, _, _ := remote(t, g, token)
	m, _, err := call(t, p, request())
	if err != nil {
		t.Fatal(err)
	}
	if m.Usage == nil || m.Usage.Cost != 0 || fetches.Load() != 0 {
		t.Fatalf("without opt-in: usage %+v, catalog fetches %d", m.Usage, fetches.Load())
	}
}
