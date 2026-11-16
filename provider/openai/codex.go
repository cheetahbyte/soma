package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cheetahbyte/soma"
	"github.com/cheetahbyte/soma/auth"
	"github.com/cheetahbyte/soma/config"
	"github.com/cheetahbyte/soma/provider"
)

const DefaultCodexBaseURL = "https://chatgpt.com/backend-api"

// Pi's isTerminalRateLimitError, plus the ChatGPT backend's own codes.
var codexLimitPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)usage_limit_reached|usage_not_included|GoUsageLimitError|FreeUsageLimitError|Monthly usage limit reached|available balance|insufficient_quota|out of budget|quota exceeded|billing`)
})

func init() {
	provider.Register("codex", func(cfg *config.Config, p config.ProviderConfig) (soma.Provider, error) {
		if _, ok, err := auth.LoadCodex(cfg.Root); err != nil {
			return nil, fmt.Errorf("provider %q: %w", p.Name, err)
		} else if !ok {
			return nil, auth.ErrNoLogin
		}
		return NewCodex(p.Name, p.BaseURL, auth.NewCodexSource(cfg.Root, auth.Issuer), p.Headers, provider.ModelInfoFunc(cfg, p)), nil
	})
}

type TokenSource interface {
	Token(ctx context.Context, rejected string) (access, accountID string, err error)
}

// NewCodex returns a client for the Responses API on the ChatGPT backend,
// authenticated by tokens.
func NewCodex(name, baseURL string, tokens TokenSource, headers map[string]string, info func(id string) config.ModelInfo) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultCodexBaseURL
	}
	c := New(name, baseURL, "", headers, info)
	c.api = "codex"
	c.tokens = tokens
	return c
}

type codexCredential struct {
	access    string
	accountID string
}

func (c *Client) token(ctx context.Context, rejected string) (codexCredential, error) {
	access, account, err := c.tokens.Token(ctx, rejected)
	return codexCredential{access: access, accountID: account}, err
}

// tokenFailure maps a failure to get a token. Its message never holds a
// token: auth builds it from the status and error fields only.
func (c *Client) tokenFailure(ctx context.Context, req soma.Request, err error) (soma.Message, error) {
	if ctx.Err() != nil {
		return abortedMessage(req.Model), ctx.Err()
	}
	kind := soma.ErrAuth
	if errors.Is(err, auth.ErrUnreachable) {
		kind = soma.ErrNetwork
	}
	return soma.Message{}, &soma.ProviderError{Kind: kind, Message: err.Error()}
}

func (c *Client) codexHeaders(h http.Header, req soma.Request, cred codexCredential) {
	h.Set("Authorization", "Bearer "+cred.access)
	h.Set("chatgpt-account-id", cred.accountID)
	h.Set("OpenAI-Beta", "responses=experimental")
	if req.SessionID != "" {
		h.Set("session-id", req.SessionID)
	}
}

// codexBody sets the fields the ChatGPT backend expects. Encrypted reasoning
// is always included, also at off, as Pi and fx do.
func codexBody(r *request) {
	if r.Instructions == "" {
		r.Instructions = "You are a helpful assistant."
	}
	r.Text = &textOpts{Verbosity: "low"}
	r.ToolChoice = "auto"
	parallel := true
	r.ParallelToolCalls = &parallel
	r.Include = []string{"reasoning.encrypted_content"}
}

// codexURL resolves the endpoint as Pi's resolveCodexUrl does.
func codexURL(base string) string {
	base = strings.TrimRight(base, "/")
	switch {
	case strings.HasSuffix(base, "/codex/responses"):
		return base
	case strings.HasSuffix(base, "/codex"):
		return base + "/responses"
	}
	return base + "/codex/responses"
}

// codexUsageLimit maps a 429 that reports an exhausted ChatGPT plan to a
// quota error, which is not retried.
func codexUsageLimit(status int, body []byte) error {
	if status != http.StatusTooManyRequests || !codexLimitPattern().Match(body) {
		return nil
	}
	var b struct {
		Error struct {
			PlanType string  `json:"plan_type"`
			ResetsAt float64 `json:"resets_at"`
		} `json:"error"`
	}
	json.Unmarshal(body, &b)
	msg := "You have hit your ChatGPT usage limit"
	if b.Error.PlanType != "" {
		msg += " (" + strings.ToLower(b.Error.PlanType) + " plan)"
	}
	msg += "."
	if b.Error.ResetsAt > 0 {
		mins := math.Round(time.Until(time.UnixMilli(int64(b.Error.ResetsAt * 1000))).Minutes())
		msg += fmt.Sprintf(" Try again in ~%d min.", int(max(0, mins)))
	}
	return &soma.ProviderError{Kind: soma.ErrQuota, Status: status, Message: msg}
}
