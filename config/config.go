package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

const DefaultContextWindow = 128000

type ModelInfo struct {
	ID             string            `json:"id"`
	ContextWindow  int               `json:"contextWindow"`
	MaxOutput      int               `json:"maxOutput"`      // output token limit; 0 sends none
	Thinking       []string          `json:"thinking"`       // supported levels; empty means all
	Reasoning      *bool             `json:"reasoning"`      // false: the model does not reason
	Effort         map[string]string `json:"effort"`         // level -> wire value
	ThinkingFormat string            `json:"thinkingFormat"` // openai-chat: "" or "deepseek"
	InputPrice     float64           `json:"inputPrice"`     // per million tokens
	OutputPrice    float64           `json:"outputPrice"`
	CachedPrice    float64           `json:"cachedPrice"`
}

type ProviderConfig struct {
	Name              string               `json:"-"`
	API               string               `json:"api"`
	Upstream          string               `json:"upstream"`
	BaseURL           string               `json:"baseUrl"`
	APIKey            string               `json:"apiKey"` // literal, or "$ENV_NAME"
	Headers           map[string]string    `json:"headers"`
	Models            []ModelInfo          `json:"models"`
	ModelOverrides    map[string]ModelInfo `json:"modelOverrides"`
	ModelsURL         string               `json:"modelsUrl"`         // openai-chat; default {baseUrl}/models
	ModelsDev         string               `json:"modelsDev"`         // provider id on models.dev whose metadata fills unset model fields
	SessionHeader     string               `json:"sessionHeader"`     // header that carries the session id
	ReplayReasoningAs string               `json:"replayReasoningAs"` // openai-chat: field for replayed reasoning; "" keeps the arrival field
	Command           string               `json:"command"`           // claude-code: program to run; default "claude"
	AllowExtraUsage   bool                 `json:"allowExtraUsage"`   // claude-code: continue when the plan reports extra usage
}

// NeedsCatalog reports whether the provider's models come from its live model list.
func (p ProviderConfig) NeedsCatalog() bool {
	return p.API == "openai-chat" && len(p.Models) == 0
}

type Config struct {
	Root      string
	ClaudeDir string
	Providers map[string]ProviderConfig

	suggestedDefault string

	mu  sync.Mutex
	mdc map[string]*ModelsDevCache // by models.dev provider id; nil value: no cache file
}

func Load(root string) (*Config, error) {
	c := &Config{Root: root, Providers: map[string]ProviderConfig{}}
	var models struct {
		Providers map[string]ProviderConfig `json:"providers"`
	}
	if err := readJSON(filepath.Join(c.Root, "models.json"), &models); err != nil {
		return nil, err
	}
	for name, p := range models.Providers {
		p.Name = name
		c.Providers[name] = p
	}
	c.suggestedDefault = c.subscriptionDefaults()
	return c, nil
}

func (c *Config) SuggestedDefault() string { return c.suggestedDefault }

func (c *Config) Model(ref string) (ProviderConfig, ModelInfo, error) {
	name, id, ok := strings.Cut(ref, "/")
	if !ok || name == "" || id == "" {
		return ProviderConfig{}, ModelInfo{}, fmt.Errorf("model %q is not in the form provider/model", ref)
	}
	p, ok := c.Providers[name]
	if !ok {
		return ProviderConfig{}, ModelInfo{}, fmt.Errorf("provider %q is not defined in %s", name, filepath.Join(c.Root, "models.json"))
	}
	var m ModelInfo
	if p.ModelsDev != "" {
		if md := c.ModelsDev(p.ModelsDev); md != nil {
			m = md.Models[id].info(id)
		}
	}
	listed, found := ModelInfo{}, false
	for _, l := range p.Models {
		if l.ID == id {
			listed, found = l, true
			break
		}
	}
	if !found && p.NeedsCatalog() {
		if cat, ok := LoadCatalog(c.Root, name); ok {
			for _, cached := range cat.Models {
				if cached.ID == id {
					listed = cached
					break
				}
			}
		}
	}
	if o, ok := p.ModelOverrides[id]; ok {
		listed = listed.override(o)
	}
	if len(listed.Thinking) > 0 && listed.Reasoning == nil {
		m.Reasoning = nil
	}
	m = m.override(listed)
	m.ID = id
	if m.ContextWindow == 0 {
		m.ContextWindow = DefaultContextWindow
	}
	return p, m, nil
}

// Listed reports whether ref's provider has a model list, in models.json or
// cached, and whether ref's model is on it. A provider without a list takes
// any model by id.
func (c *Config) Listed(ref string) (hasList, listed bool) {
	name, id, _ := strings.Cut(ref, "/")
	p, ok := c.Providers[name]
	if !ok {
		return false, false
	}
	ids := make([]string, 0, len(p.Models))
	for _, m := range p.Models {
		ids = append(ids, m.ID)
	}
	if p.NeedsCatalog() {
		if cat, ok := LoadCatalog(c.Root, name); ok {
			for _, m := range cat.Models {
				ids = append(ids, m.ID)
			}
		}
	}
	return len(ids) > 0, slices.Contains(ids, id)
}

// ModelRefs lists every model of models.json and of the cached model lists
// as "provider/model", sorted.
func (c *Config) ModelRefs() []string {
	var refs []string
	for name, p := range c.Providers {
		for _, m := range p.Models {
			refs = append(refs, name+"/"+m.ID)
		}
		if p.NeedsCatalog() {
			cat, _ := LoadCatalog(c.Root, name)
			for _, m := range cat.Models {
				refs = append(refs, name+"/"+m.ID)
			}
		}
	}
	slices.Sort(refs)
	return refs
}

func (c *Config) Key(p ProviderConfig) (string, error) {
	if env, ok := strings.CutPrefix(p.APIKey, "$"); ok {
		if v := os.Getenv(env); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("provider %q: environment variable %s is not set", p.Name, env)
	}
	if p.APIKey != "" {
		return p.APIKey, nil
	}
	path := filepath.Join(c.Root, "auth.json")
	var auth map[string]struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	if err := readJSON(path, &auth); err != nil {
		return "", err
	}
	if a, ok := auth[p.Name]; ok && a.Type == "api_key" && a.Key != "" {
		return a.Key, nil
	}
	return "", fmt.Errorf("provider %q has no API key: set apiKey in %s or add it to %s",
		p.Name, filepath.Join(c.Root, "models.json"), path)
}

// readJSON leaves v unchanged when the file does not exist.
func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
