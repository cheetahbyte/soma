package config

import (
	"context"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/cheetahbyte/soma/provider/wire"
)

// ModelsDevURL is the public catalog. Only the test build can change it.
var ModelsDevURL = "https://models.dev/api.json"

const (
	modelsDevMaxBytes   = 32 << 20
	modelsDevFetchLimit = 30 * time.Second
)

// RefreshModelsDev fetches the models.dev catalog for every opted-in provider
// whose cached data is missing or due (ShouldFetchModelsDev), once per
// catalog provider id. Failures are cached, not shown. Interactive mode calls
// it once after the first prompt; axon ask never does.
func RefreshModelsDev(cfg *Config) {
	done := map[string]bool{}
	for _, pc := range cfg.Providers {
		id := pc.ModelsDev
		if id == "" || done[id] {
			continue
		}
		done[id] = true
		if ShouldFetchModelsDev(cfg.ModelsDev(id), time.Now()) {
			FetchModelsDev(cfg, id, modelsDevFetchLimit)
		}
	}
	if len(done) > 0 {
		debug.FreeOSMemory()
	}
}

// FetchModelsDev reads the catalog, keeps only provider id, and caches it.
// A failed fetch keeps the earlier data and records the failure.
func FetchModelsDev(cfg *Config, id string, limit time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	old := cfg.ModelsDev(id)
	now := time.Now()
	next, err := requestModelsDev(ctx, id, old)
	if err != nil {
		next = ModelsDevCache{Models: map[string]ModelsDevModel{}}
		if old != nil {
			next = *old
		}
		next.Attempted, next.Error = now, err.Error()
	} else {
		next.Attempted = now
	}
	if serr := SaveModelsDev(cfg.Root, id, next); serr != nil && err == nil {
		err = serr
	}
	cfg.SetModelsDev(id, &next)
	return err
}

func requestModelsDev(ctx context.Context, id string, old *ModelsDevCache) (ModelsDevCache, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelsDevURL, nil)
	if err != nil {
		return ModelsDevCache{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", wire.UserAgent())
	if old != nil && old.Error == "" && len(old.Models) > 0 {
		if old.ETag != "" {
			req.Header.Set("If-None-Match", old.ETag)
		} else if old.LastModified != "" {
			req.Header.Set("If-Modified-Since", old.LastModified)
		}
	} else {
		old = nil
	}
	client, _ := wire.NewHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return ModelsDevCache{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified && req.Header.Get("If-None-Match")+req.Header.Get("If-Modified-Since") != "":
		next := *old
		next.Fetched, next.Error = time.Now(), ""
		return next, nil
	case resp.StatusCode != http.StatusOK:
		return ModelsDevCache{}, fmt.Errorf("models.dev request failed: HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	case resp.ContentLength > modelsDevMaxBytes:
		return ModelsDevCache{}, ErrModelsDevTooLarge
	}
	models, err := DecodeModelsDev(resp.Body, id, modelsDevMaxBytes)
	if err != nil {
		return ModelsDevCache{}, err
	}
	return ModelsDevCache{
		Fetched:      time.Now(),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		Models:       models,
	}, nil
}
