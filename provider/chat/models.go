package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cheetahbyte/soma/config"
	"github.com/cheetahbyte/soma/provider/wire"
)

// A catalog larger than this is not a model list.
const maxCatalogBytes = 16 << 20

type catalogEntry struct {
	ID            string       `json:"id"`
	ContextLength float64      `json:"context_length"`
	Meta          *catalogMeta `json:"_meta"`
}

// catalogMeta is the part of Melious's _meta axon uses
// (melious-provider.ts, MeliousMeta).
type catalogMeta struct {
	Type            string   `json:"type"`
	ContextLength   float64  `json:"context_length"`
	MaxOutputTokens float64  `json:"max_output_tokens"`
	ReasoningType   string   `json:"reasoning_type"`
	InputModalities []string `json:"input_modalities"`
	Capabilities    struct {
		Reasoning  *bool `json:"reasoning"`
		Vision     *bool `json:"vision"`
		ImageInput *bool `json:"image_input"`
	} `json:"capabilities"`
	Pricing struct {
		Input     float64 `json:"input_cost_per_million_eur"`
		Output    float64 `json:"output_cost_per_million_eur"`
		CacheRead float64 `json:"cache_read_cost_per_million_eur"`
	} `json:"pricing"`
}

// FetchModels reads GET {modelsUrl}, by default {baseUrl}/models. Entries
// whose _meta names a type other than chat are skipped.
func FetchModels(ctx context.Context, p config.ProviderConfig, key string) ([]config.ModelInfo, error) {
	url := p.ModelsURL
	if url == "" {
		url = strings.TrimRight(p.BaseURL, "/") + "/models"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	client, _ := wire.NewHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model list request failed: HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes))
	if err != nil {
		return nil, err
	}
	var body struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("model list is not valid JSON: %v", err)
	}
	models := []config.ModelInfo{}
	for _, raw := range body.Data {
		var e catalogEntry
		if json.Unmarshal(raw, &e) != nil || e.ID == "" {
			continue
		}
		if m, ok := e.model(); ok {
			models = append(models, m)
		}
	}
	return models, nil
}

func (e catalogEntry) model() (config.ModelInfo, bool) {
	m := config.ModelInfo{ID: e.ID, ContextWindow: int(e.ContextLength)}
	meta := e.Meta
	if meta == nil {
		return m, true
	}
	if meta.Type != "" && meta.Type != "chat" {
		return config.ModelInfo{}, false
	}
	if meta.ContextLength > 0 {
		m.ContextWindow = int(meta.ContextLength)
	}
	m.MaxOutput = int(meta.MaxOutputTokens)
	m.InputPrice = meta.Pricing.Input
	m.OutputPrice = meta.Pricing.Output
	m.CachedPrice = meta.Pricing.CacheRead
	// melious-provider.ts, supportsReasoning: reasoning_type decides when
	// present, else capabilities.reasoning, else the model does not reason.
	var reasons bool
	if meta.ReasoningType != "" {
		reasons = meta.ReasoningType != "non_reasoning"
	} else {
		reasons = meta.Capabilities.Reasoning != nil && *meta.Capabilities.Reasoning
	}
	m.Reasoning = &reasons
	// Melious turns a hybrid model's reasoning off only when told "none".
	if meta.ReasoningType == "hybrid" {
		m.Effort = map[string]string{"off": "none"}
	}
	if len(meta.InputModalities) > 0 {
		for _, mod := range meta.InputModalities {
			if mod == "text" || mod == "image" {
				exists := false
				for _, seen := range m.Input {
					if seen == mod {
						exists = true
						break
					}
				}
				if !exists {
					m.Input = append(m.Input, mod)
				}
			}
		}
	} else if (meta.Capabilities.Vision != nil && *meta.Capabilities.Vision) || (meta.Capabilities.ImageInput != nil && *meta.Capabilities.ImageInput) {
		m.Input = []string{"text", "image"}
	} else if meta.Capabilities.Vision != nil || meta.Capabilities.ImageInput != nil {
		m.Input = []string{"text"}
	}
	return m, true
}
