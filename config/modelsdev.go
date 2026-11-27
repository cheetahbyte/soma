package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ModelsDevModel is what the cache keeps of a catalog record.
type ModelsDevModel struct {
	ContextWindow int      `json:"contextWindow,omitempty"`
	MaxOutput     int      `json:"maxOutput,omitempty"`
	InputPrice    float64  `json:"inputPrice,omitempty"`
	OutputPrice   float64  `json:"outputPrice,omitempty"`
	CachedPrice   float64  `json:"cachedPrice,omitempty"`
	Reasoning     *bool    `json:"reasoning,omitempty"`
	Thinking      []string `json:"thinking,omitempty"`
	Input         []string `json:"input,omitempty"`
}

func (m ModelsDevModel) info(id string) ModelInfo {
	return ModelInfo{ID: id, ContextWindow: m.ContextWindow, MaxOutput: m.MaxOutput, InputPrice: m.InputPrice,
		OutputPrice: m.OutputPrice, CachedPrice: m.CachedPrice, Reasoning: m.Reasoning, Thinking: m.Thinking, Input: m.Input}
}

// ModelsDevCache holds the slimmed records of one provider of the models.dev
// catalog, already mapped to the fields axon uses.
type ModelsDevCache struct {
	Fetched      time.Time                 `json:"fetched"`   // last successful fetch or revalidation
	Attempted    time.Time                 `json:"attempted"` // last fetch, successful or not
	Error        string                    `json:"error,omitempty"`
	ETag         string                    `json:"etag,omitempty"`
	LastModified string                    `json:"lastModified,omitempty"`
	Models       map[string]ModelsDevModel `json:"models"`
}

const modelsDevRetryAfter = 10 * time.Minute

// ShouldFetchModelsDev reports whether interactive mode should fetch the
// catalog now: there is no cache, the last attempt failed at least ten
// minutes ago, or the data is a day old. A failure keeps the old data.
func ShouldFetchModelsDev(c *ModelsDevCache, now time.Time) bool {
	switch {
	case c == nil:
		return true
	case c.Error != "":
		return now.Sub(c.Attempted) >= modelsDevRetryAfter
	default:
		return now.Sub(c.Fetched) >= catalogMaxAge
	}
}

func modelsDevPath(root, id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return "", fmt.Errorf("models.dev provider id %q cannot name a cache file", id)
	}
	return filepath.Join(root, "cache", "models", "modelsdev-"+id+".json"), nil
}

// LoadModelsDev returns nil when there is no usable cache file.
func LoadModelsDev(root, id string) *ModelsDevCache {
	path, err := modelsDevPath(root, id)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var c ModelsDevCache
	if json.Unmarshal(data, &c) != nil {
		return nil
	}
	return &c
}

func SaveModelsDev(root, id string, c ModelsDevCache) error {
	path, err := modelsDevPath(root, id)
	if err != nil {
		return err
	}
	return writeJSON(path, c)
}

// ModelsDev returns the cached catalog data of the models.dev provider id, or
// nil. The first call reads the file; SetModelsDev replaces it.
func (c *Config) ModelsDev(id string) *ModelsDevCache {
	c.mu.Lock()
	defer c.mu.Unlock()
	if md, ok := c.mdc[id]; ok {
		return md
	}
	md := LoadModelsDev(c.Root, id)
	c.setModelsDev(id, md)
	return md
}

// SetModelsDev makes md the data that later model lookups use.
func (c *Config) SetModelsDev(id string, md *ModelsDevCache) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setModelsDev(id, md)
}

func (c *Config) setModelsDev(id string, md *ModelsDevCache) {
	if c.mdc == nil {
		c.mdc = map[string]*ModelsDevCache{}
	}
	c.mdc[id] = md
}

// ErrModelsDevTooLarge is returned for a document that exceeds the size cap.
var ErrModelsDevTooLarge = errors.New("models.dev response is too large")

type modelsDevRecord struct {
	Reasoning        *bool `json:"reasoning"`
	ReasoningOptions []struct {
		Type   string   `json:"type"`
		Values []string `json:"values"`
	} `json:"reasoning_options"`
	Limit struct {
		Context float64 `json:"context"`
		Output  float64 `json:"output"`
	} `json:"limit"`
	Cost struct {
		Input     float64 `json:"input"`
		Output    float64 `json:"output"`
		CacheRead float64 `json:"cache_read"`
	} `json:"cost"`
	Modalities struct {
		Input []string `json:"input"`
	} `json:"modalities"`
}

// Tiered prices in the record are ignored: the base price is used.
func (r modelsDevRecord) model() ModelsDevModel {
	m := ModelsDevModel{
		ContextWindow: int(r.Limit.Context),
		MaxOutput:     int(r.Limit.Output),
		InputPrice:    r.Cost.Input,
		OutputPrice:   r.Cost.Output,
		CachedPrice:   r.Cost.CacheRead,
	}
	for _, mod := range r.Modalities.Input {
		if (mod == "text" || mod == "image") && !slices.Contains(m.Input, mod) {
			m.Input = append(m.Input, mod)
		}
	}
	if r.Reasoning != nil && !*r.Reasoning {
		off := false
		m.Reasoning = &off
		return m
	}
	m.Thinking = r.thinking()
	return m
}

func (r modelsDevRecord) thinking() []string {
	var values []string
	efforts := 0
	for _, o := range r.ReasoningOptions {
		if o.Type == "effort" {
			efforts++
			values = o.Values
		}
	}
	if efforts != 1 {
		return nil
	}
	var levels []string
	for _, v := range values {
		if v == "none" {
			v = "off"
		}
		if slices.Contains(ThinkingLevels, v) && !slices.Contains(levels, v) {
			levels = append(levels, v)
		}
	}
	slices.SortFunc(levels, func(a, b string) int {
		return slices.Index(ThinkingLevels, a) - slices.Index(ThinkingLevels, b)
	})
	return levels
}

// DecodeModelsDev reads the catalog document from r, which is an object
// keyed by provider id, and returns the mapped models of provider id. Only
// that provider is decoded; r is read no further than the end of its object,
// and a document that reaches maxBytes before that is rejected.
func DecodeModelsDev(r io.Reader, id string, maxBytes int64) (map[string]ModelsDevModel, error) {
	lr := &io.LimitedReader{R: r, N: maxBytes + 1}
	models, err := decodeProvider(json.NewDecoder(lr), id)
	if lr.N <= 0 {
		return nil, ErrModelsDevTooLarge
	}
	return models, err
}

func decodeProvider(dec *json.Decoder, id string) (map[string]ModelsDevModel, error) {
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("models.dev response is not a JSON object")
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if key, _ := t.(string); key != id {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
			continue
		}
		var p struct {
			Models map[string]modelsDevRecord `json:"models"`
		}
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("models.dev provider %q: %w", id, err)
		}
		models := make(map[string]ModelsDevModel, len(p.Models))
		for mid, rec := range p.Models {
			models[mid] = rec.model()
		}
		return models, nil
	}
	return nil, fmt.Errorf("models.dev has no provider %q", id)
}
