package config

import (
	"os/exec"
	"slices"
	"strings"

	"github.com/cheetahbyte/soma/auth"
)

// MeliousProvider is the name of the provider that a Melious login adds,
// and of its key in auth.json. MeliousBaseURL is a variable so that test
// builds can point it at a local server.
const MeliousProvider = "melious"

var MeliousBaseURL = "https://api.melious.ai/v1"

// MeliousConfig is the provider a Melious login adds: OpenAI chat
// completions with the live model list and its metadata.
func MeliousConfig() ProviderConfig {
	return ProviderConfig{
		Name: MeliousProvider, API: "openai-chat", BaseURL: MeliousBaseURL,
		ModelsURL: MeliousBaseURL + "/models?include_meta=true",
	}
}

func subscriptionModels(api string) []ModelInfo {
	switch api {
	case "claude-code":
		return []ModelInfo{
			{ID: "claude-sonnet-5-5", ContextWindow: 1000000},
			{ID: "claude-opus-5-5", ContextWindow: 1000000},
			{ID: "claude-fable-5-2", ContextWindow: 1000000},
			{ID: "claude-haiku-5-5", ContextWindow: 1000000},
		}
	}
	return nil
}

func (c *Config) subscriptionDefaults() string {
	names := make([]string, 0, len(c.Providers))
	apis := make(map[string]bool)
	for name, p := range c.Providers {
		apis[p.API] = true
		if len(p.Models) == 0 {
			p.Models = subscriptionModels(p.API)
			c.Providers[name] = p
		}
		if p.API == "codex" || p.API == "claude-code" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if !apis["codex"] {
		if _, exists := c.Providers["chatgpt"]; !exists {
			if _, loggedIn, err := auth.LoadCodex(c.Root); err == nil && loggedIn {
				c.Providers["chatgpt"] = ProviderConfig{Name: "chatgpt", API: "codex"}
				names = append(names, "chatgpt")
			}
		}
	}
	if !apis["claude-code"] {
		if _, exists := c.Providers["claude"]; !exists {
			if _, err := exec.LookPath("claude"); err == nil {
				c.Providers["claude"] = ProviderConfig{
					Name: "claude", API: "claude-code", Models: subscriptionModels("claude-code"),
				}
				names = append(names, "claude")
			}
		}
	}
	melious := c.addMelious()
	for _, name := range names {
		if id := c.firstModel(c.Providers[name]); id != "" {
			return name + "/" + id
		}
	}
	if melious {
		if id := c.meliousDefault(); id != "" {
			return MeliousProvider + "/" + id
		}
	}
	return ""
}

// firstModel is p's first listed model or, for a live list, the first model
// of its cached list, which for ChatGPT is the server's highest priority.
func (c *Config) firstModel(p ProviderConfig) string {
	if len(p.Models) > 0 {
		return p.Models[0].ID
	}
	if cat, ok := LoadCatalog(c.Root, p.Name); ok && p.NeedsCatalog() && len(cat.Models) > 0 {
		return cat.Models[0].ID
	}
	return ""
}

// addMelious adds the Melious provider when auth.json holds its key and no
// provider has its name or its address, and reports whether it did.
func (c *Config) addMelious() bool {
	if _, exists := c.Providers[MeliousProvider]; exists {
		return false
	}
	host := strings.TrimSuffix(MeliousBaseURL, "/v1")
	for _, p := range c.Providers {
		if strings.HasPrefix(p.BaseURL, host) {
			return false
		}
	}
	if !auth.HasAPIKey(c.Root, MeliousProvider) {
		return false
	}
	c.Providers[MeliousProvider] = MeliousConfig()
	return true
}

// meliousDefault picks the model of a fresh setup from the cached list: the
// reasoning model with the largest context window, ties by id.
func (c *Config) meliousDefault() string {
	cat, ok := LoadCatalog(c.Root, MeliousProvider)
	if !ok {
		return ""
	}
	best := ModelInfo{}
	for _, m := range cat.Models {
		if m.Reasoning != nil && !*m.Reasoning {
			continue
		}
		if best.ID == "" || m.ContextWindow > best.ContextWindow || m.ContextWindow == best.ContextWindow && m.ID < best.ID {
			best = m
		}
	}
	return best.ID
}
