package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const modelsDevDoc = `{
  "other": {"id":"other","name":"Other","models":{"x":{"id":"x","limit":{"context":1,"output":1},"cost":{"input":99,"output":99}}}},
  "openai": {"id":"openai","name":"OpenAI","env":["OPENAI_API_KEY"],"models":{
    "gpt-5.5": {"id":"gpt-5.5","reasoning":true,
      "reasoning_options":[{"type":"effort","values":["none","low","medium","high","xhigh","max","default"]}],
      "limit":{"context":1050000,"input":922000,"output":128000},
      "cost":{"input":5,"output":30,"cache_read":0.5,"cache_write":6,
        "tiers":[{"input":10,"output":45,"cache_read":1,"tier":{"type":"context","size":272000}}],
        "context_over_200k":{"input":10,"output":45}}},
    "gpt-4o": {"id":"gpt-4o","reasoning":false,"limit":{"context":128000,"output":16384},"cost":{"input":2.5,"output":10,"cache_read":1.25}},
    "bare": {"id":"bare"},
    "multi": {"id":"multi","reasoning":true,"reasoning_options":[{"type":"effort","values":["low"]},{"type":"effort","values":["high"]}]},
    "toggle": {"id":"toggle","reasoning":true,"reasoning_options":[{"type":"toggle","values":["low"]}]},
    "unknown": {"id":"unknown","reasoning":true,"reasoning_options":[{"type":"effort","values":["max","default"]}]}
  }},
  "last": {"id":"last","models":{}}
}`

func decodeFixture(t *testing.T) map[string]ModelsDevModel {
	t.Helper()
	models, err := DecodeModelsDev(strings.NewReader(modelsDevDoc), "openai", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return models
}

func TestModelsDevMapping(t *testing.T) {
	models := decodeFixture(t)
	if len(models) != 6 {
		t.Errorf("decoded %d models, want 6 of the openai provider only", len(models))
	}
	m := models["gpt-5.5"]
	if m.ContextWindow != 1050000 || m.MaxOutput != 128000 || m.InputPrice != 5 || m.OutputPrice != 30 || m.CachedPrice != 0.5 {
		t.Errorf("gpt-5.5 = %+v, want base prices and limits", m)
	}
	if want := []string{"off", "low", "medium", "high", "xhigh"}; !slices.Equal(m.Thinking, want) || m.Reasoning != nil {
		t.Errorf("gpt-5.5 thinking = %v reasoning = %v, want %v and nil", m.Thinking, m.Reasoning, want)
	}
	if o := models["gpt-4o"]; o.Reasoning == nil || *o.Reasoning || o.Thinking != nil || o.CachedPrice != 1.25 {
		t.Errorf("gpt-4o = %+v, want reasoning false", o)
	}
	if b := models["bare"]; b.ContextWindow != 0 || b.InputPrice != 0 || b.Reasoning != nil || b.Thinking != nil {
		t.Errorf("bare = %+v, want nothing set", b)
	}
	for _, id := range []string{"multi", "toggle", "unknown"} {
		if th := models[id].Thinking; th != nil {
			t.Errorf("%s thinking = %v, want unset", id, th)
		}
	}
}

func TestDecodeModelsDevErrors(t *testing.T) {
	if _, err := DecodeModelsDev(strings.NewReader(modelsDevDoc), "missing", 1<<20); err == nil {
		t.Error("missing provider: no error")
	}
	if _, err := DecodeModelsDev(strings.NewReader(`[1]`), "openai", 1<<20); err == nil {
		t.Error("array document: no error")
	}
	if _, err := DecodeModelsDev(strings.NewReader(modelsDevDoc), "last", 100); !errors.Is(err, ErrModelsDevTooLarge) {
		t.Errorf("over the cap: err = %v, want ErrModelsDevTooLarge", err)
	}
	if _, err := DecodeModelsDev(strings.NewReader(modelsDevDoc), "other", int64(len(modelsDevDoc))); err != nil {
		t.Errorf("exactly at the cap: %v", err)
	}
}

func TestDecodeModelsDevStopsAfterProvider(t *testing.T) {
	r := &countReader{r: strings.NewReader(`{"a":{"models":{"m":{}}},"b":` + strings.Repeat(" ", 1<<20) + `{}}`)}
	if _, err := DecodeModelsDev(r, "a", 1<<30); err != nil {
		t.Fatal(err)
	}
	if r.n > 64<<10 {
		t.Errorf("read %d bytes, want the provider found without reading the rest", r.n)
	}
}

type countReader struct {
	r interface{ Read([]byte) (int, error) }
	n int
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestModelsDevCacheRoundTrip(t *testing.T) {
	root := t.TempDir()
	want := ModelsDevCache{
		Fetched: time.Unix(1000, 0).UTC(), Attempted: time.Unix(1000, 0).UTC(),
		ETag: `"abc"`, LastModified: "Fri, 13 Nov 2026 12:00:00 GMT",
		Models: decodeFixture(t),
	}
	if err := SaveModelsDev(root, "openai", want); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "cache", "models", "modelsdev-openai.json")); err != nil {
		t.Fatal(err)
	}
	got := LoadModelsDev(root, "openai")
	if got == nil || got.ETag != want.ETag || got.LastModified != want.LastModified || !got.Fetched.Equal(want.Fetched) ||
		len(got.Models) != 6 || got.Models["gpt-5.5"].InputPrice != 5 || !slices.Equal(got.Models["gpt-5.5"].Thinking, want.Models["gpt-5.5"].Thinking) {
		t.Errorf("round trip = %+v", got)
	}
	if LoadModelsDev(root, "missing") != nil || LoadModelsDev(root, "../x") != nil {
		t.Error("missing or invalid id returned a cache")
	}
	data, _ := os.ReadFile(filepath.Join(root, "cache", "models", "modelsdev-openai.json"))
	if strings.Contains(string(data), "tiers") || strings.Contains(string(data), "other") {
		t.Errorf("cache keeps more than the slimmed fields:\n%s", data)
	}
}

func TestShouldFetchModelsDev(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		c    *ModelsDevCache
		want bool
	}{
		{"no cache", nil, true},
		{"fresh", &ModelsDevCache{Fetched: now.Add(-time.Hour)}, false},
		{"a day old", &ModelsDevCache{Fetched: now.Add(-25 * time.Hour)}, true},
		{"failed just now, old data kept", &ModelsDevCache{Fetched: now.Add(-30 * time.Hour), Attempted: now.Add(-time.Minute), Error: "x"}, false},
		{"failed 11 minutes ago", &ModelsDevCache{Attempted: now.Add(-11 * time.Minute), Error: "x"}, true},
	}
	for _, c := range cases {
		if got := ShouldFetchModelsDev(c.c, now); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func b(v bool) *bool { return &v }

func modelsDevConfig(t *testing.T, p ProviderConfig) *Config {
	t.Helper()
	root := t.TempDir()
	if err := SaveModelsDev(root, "openai", ModelsDevCache{Fetched: time.Now(), Models: decodeFixture(t)}); err != nil {
		t.Fatal(err)
	}
	p.Name, p.ModelsDev = "p", "openai"
	return &Config{Root: root, Providers: map[string]ProviderConfig{"p": p}}
}

func TestModelsDevPrecedence(t *testing.T) {
	c := modelsDevConfig(t, ProviderConfig{
		API: "openai-chat",
		Models: []ModelInfo{
			{ID: "gpt-5.5", InputPrice: 1, ContextWindow: 7},
			{ID: "gpt-4o", Thinking: []string{"low", "high"}},
		},
		ModelOverrides: map[string]ModelInfo{"gpt-5.5": {OutputPrice: 2, MaxOutput: 9}},
	})
	_, m, err := c.Model("p/gpt-5.5")
	if err != nil {
		t.Fatal(err)
	}
	if m.InputPrice != 1 || m.ContextWindow != 7 || m.OutputPrice != 2 || m.MaxOutput != 9 || m.CachedPrice != 0.5 || len(m.Thinking) != 5 {
		t.Errorf("gpt-5.5 = %+v, want entry and override fields winning, the rest from models.dev", m)
	}
	_, o, _ := c.Model("p/gpt-4o")
	if o.Reasoning != nil || !slices.Equal(o.Levels(), []string{"low", "high"}) || o.OutputPrice != 10 {
		t.Errorf("gpt-4o = %+v, want an explicit thinking list to beat the catalog's reasoning false", o)
	}
	_, unlisted, _ := c.Model("p/bare")
	if unlisted.ContextWindow != DefaultContextWindow {
		t.Errorf("model without data: window %d, want the default", unlisted.ContextWindow)
	}
	_, ro, _ := modelsDevConfig(t, ProviderConfig{ModelOverrides: map[string]ModelInfo{"gpt-4o": {Reasoning: b(true)}}}).Model("p/gpt-4o")
	if ro.Reasoning == nil || !*ro.Reasoning {
		t.Errorf("override reasoning true lost to the catalog: %+v", ro)
	}
}

func TestModelsDevBeatsDefaultsButNotLiveList(t *testing.T) {
	c := modelsDevConfig(t, ProviderConfig{API: "openai-chat"})
	if err := SaveCatalog(c.Root, "p", Catalog{Fetched: time.Now(), Models: []ModelInfo{
		{ID: "gpt-5.5", ContextWindow: 200000, InputPrice: 3},
	}}); err != nil {
		t.Fatal(err)
	}
	_, m, _ := c.Model("p/gpt-5.5")
	if m.ContextWindow != 200000 || m.InputPrice != 3 || m.OutputPrice != 30 || m.MaxOutput != 128000 {
		t.Errorf("gpt-5.5 = %+v, want live list fields over models.dev, gaps filled from it", m)
	}
}

func TestNoModelsDevWithoutOptIn(t *testing.T) {
	c := modelsDevConfig(t, ProviderConfig{})
	p := c.Providers["p"]
	p.ModelsDev = ""
	c.Providers["p"] = p
	if _, m, _ := c.Model("p/gpt-5.5"); m.InputPrice != 0 || m.ContextWindow != DefaultContextWindow {
		t.Errorf("without the field the model = %+v", m)
	}
}

func TestSetModelsDevAppliesToLaterLookups(t *testing.T) {
	c := &Config{Root: t.TempDir(), Providers: map[string]ProviderConfig{"p": {Name: "p", ModelsDev: "openai"}}}
	if _, m, _ := c.Model("p/gpt-5.5"); m.InputPrice != 0 {
		t.Fatalf("price %v before any data", m.InputPrice)
	}
	c.SetModelsDev("openai", &ModelsDevCache{Models: decodeFixture(t)})
	if _, m, _ := c.Model("p/gpt-5.5"); m.InputPrice != 5 {
		t.Errorf("price %v after SetModelsDev, want 5", m.InputPrice)
	}
}
