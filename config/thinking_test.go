package config

import (
	"slices"
	"testing"
	"time"
)

func TestLevelClampsToSupported(t *testing.T) {
	m := ModelInfo{Thinking: []string{"low", "high"}}
	for requested, want := range map[string]string{"medium": "high", "minimal": "low", "xhigh": "high", "": ""} {
		if got := m.Level(requested); got != want {
			t.Errorf("Level(%q) = %q, want %q", requested, got, want)
		}
	}
}

func TestNonReasoningModelOffersOnlyOff(t *testing.T) {
	no := false
	m := ModelInfo{Reasoning: &no}
	if got := m.Levels(); !slices.Equal(got, []string{"off"}) {
		t.Errorf("Levels() = %q, want [off]", got)
	}
	if got := m.Level("high"); got != "off" {
		t.Errorf(`Level("high") = %q, want "off"`, got)
	}
	if v, ok := m.WireEffort("high"); v != "" || ok {
		t.Errorf(`WireEffort("high") = (%q, %v), want ("", false)`, v, ok)
	}
}

func TestWireEffortMapsOff(t *testing.T) {
	m := ModelInfo{Effort: map[string]string{"off": "none", "minimal": "low"}}
	for requested, want := range map[string]string{"off": "none", "minimal": "low", "high": "high"} {
		if v, ok := m.WireEffort(requested); v != want || !ok {
			t.Errorf("WireEffort(%q) = (%q, %v), want (%q, true)", requested, v, ok, want)
		}
	}
	if v, ok := (ModelInfo{}).WireEffort("off"); v != "" || ok {
		t.Errorf(`without a mapping, WireEffort("off") = (%q, %v), want ("", false)`, v, ok)
	}
}

func TestOverridesApplyFieldByField(t *testing.T) {
	c := &Config{Root: t.TempDir(), Providers: map[string]ProviderConfig{"p": {
		Name:   "p",
		API:    "openai-responses",
		Models: []ModelInfo{{ID: "m", ContextWindow: 1000, InputPrice: 1}},
		ModelOverrides: map[string]ModelInfo{
			"m": {OutputPrice: 2, Thinking: []string{"low"}},
			"u": {ContextWindow: 5},
		},
	}}}
	_, m, err := c.Model("p/m")
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "m" || m.ContextWindow != 1000 || m.InputPrice != 1 || m.OutputPrice != 2 || !slices.Equal(m.Thinking, []string{"low"}) {
		t.Errorf("Model(p/m) = %+v, want window 1000, input 1, output 2, thinking [low]", m)
	}
	if _, m, _ := c.Model("p/u"); m.ID != "u" || m.ContextWindow != 5 {
		t.Errorf("Model(p/u) = %+v, want window 5", m)
	}
	if _, m, _ := c.Model("p/x"); m.ID != "x" || m.ContextWindow != DefaultContextWindow {
		t.Errorf("Model(p/x) = %+v, want the default window", m)
	}
}

func TestModelUsesCachedCatalog(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	if err := SaveCatalog(root, "p", Catalog{Fetched: now, Models: []ModelInfo{{ID: "c", ContextWindow: 7}}}); err != nil {
		t.Fatal(err)
	}
	c := &Config{Root: root, Providers: map[string]ProviderConfig{"p": {Name: "p", API: "openai-chat"}}}
	if _, m, err := c.Model("p/c"); err != nil || m.ContextWindow != 7 {
		t.Errorf("Model(p/c) = %+v, %v; want window 7", m, err)
	}
	cat, ok := LoadCatalog(root, "p")
	if !ok || !cat.Fetched.Equal(now) || len(cat.Models) != 1 || cat.Models[0].ID != "c" {
		t.Errorf("LoadCatalog = %+v, %v; want the saved catalog", cat, ok)
	}
	if _, ok := LoadCatalog(root, "q"); ok {
		t.Error("LoadCatalog reports a cache for a provider without one")
	}
	if err := SaveCatalog(root, "a/b", Catalog{}); err == nil {
		t.Error("SaveCatalog accepted a provider name with a path separator")
	}
}

func TestCatalogStale(t *testing.T) {
	now := time.Now()
	good := Catalog{Fetched: now.Add(-23 * time.Hour)}
	if good.Stale(now) {
		t.Error("a good catalog is stale after 23 hours")
	}
	if good.Fetched = now.Add(-24*time.Hour - time.Second); !good.Stale(now) {
		t.Error("a good catalog is not stale after 24 hours")
	}
	failed := Catalog{Fetched: now.Add(-9 * time.Minute), Error: "down"}
	if failed.Stale(now) {
		t.Error("a failed catalog is stale after 9 minutes")
	}
	if failed.Fetched = now.Add(-10*time.Minute - time.Second); !failed.Stale(now) {
		t.Error("a failed catalog is not stale after 10 minutes")
	}
}
