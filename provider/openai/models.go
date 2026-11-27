package openai

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"

	"github.com/cheetahbyte/soma/config"
	"github.com/cheetahbyte/soma/provider/wire"
)

// A model list larger than this is not a model list; ChatGPT's is about
// half a megabyte for ten models.
const maxModelListBytes = 16 << 20

type codexModel struct {
	Slug            string       `json:"slug"`
	ContextWindow   float64      `json:"context_window"`
	Levels          []codexLevel `json:"supported_reasoning_levels"`
	Visibility      string       `json:"visibility"`
	Priority        *float64     `json:"priority"`
	InputModalities []string     `json:"input_modalities"`
}

type codexLevel struct {
	Effort string `json:"effort"`
}

// rank orders by priority, 1 first; a model without one comes last.
func (m codexModel) rank() float64 {
	if m.Priority == nil {
		return math.Inf(1)
	}
	return *m.Priority
}

// codexLevels are the ChatGPT reasoning efforts that have a thinking level
// of the same name.
var codexLevels = []string{"low", "medium", "high", "xhigh"}

// codexModels reads the ChatGPT model list, leaving out the models it does
// not list, in the order of their priority.
func (c *Client) codexModels(ctx context.Context) (_ []config.ModelInfo, err error) {
	var cred codexCredential
	var rejected []string
	defer func() { err = wire.Redact(err, append(rejected, cred.access)...) }()
	if cred, err = c.token(ctx, ""); err != nil {
		return nil, err
	}
	resp, err := c.getModels(ctx, cred)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		rejected = append(rejected, cred.access)
		if cred, err = c.token(ctx, cred.access); err != nil {
			return nil, err
		}
		resp, err = c.getModels(ctx, cred)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model list request failed: HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxModelListBytes))
	if err != nil {
		return nil, err
	}
	var body struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("model list is not valid JSON: %v", err)
	}
	var listed []codexModel
	for _, raw := range body.Models {
		var m codexModel
		if json.Unmarshal(raw, &m) != nil || m.Slug == "" || m.Visibility != "list" {
			continue
		}
		listed = append(listed, m)
	}
	slices.SortStableFunc(listed, func(a, b codexModel) int { return cmp.Compare(a.rank(), b.rank()) })
	models := make([]config.ModelInfo, 0, len(listed))
	for _, m := range listed {
		models = append(models, m.info())
	}
	return models, nil
}

// info offers minimal, sent as low, below the supported efforts. Off is not
// offered: the list does not say whether a model accepts effort none.
func (m codexModel) info() config.ModelInfo {
	info := config.ModelInfo{ID: m.Slug, ContextWindow: int(m.ContextWindow)}
	for _, mod := range m.InputModalities {
		if (mod == "text" || mod == "image") && !slices.Contains(info.Input, mod) {
			info.Input = append(info.Input, mod)
		}
	}
	for _, l := range codexLevels {
		if slices.Contains(m.Levels, codexLevel{l}) {
			info.Thinking = append(info.Thinking, l)
		}
	}
	if len(info.Thinking) == 0 {
		off := false
		info.Reasoning = &off
		return info
	}
	if info.Thinking[0] == "low" {
		info.Thinking = append([]string{"minimal"}, info.Thinking...)
		info.Effort = map[string]string{"minimal": "low"}
	}
	return info
}

func (c *Client) getModels(ctx context.Context, cred codexCredential) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexModelsURL(c.baseURL), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", wire.UserAgent())
	req.Header.Set("originator", "harness")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+cred.access)
	req.Header.Set("chatgpt-account-id", cred.accountID)
	return c.http.Do(req)
}
