package claudecode

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cheetahbyte/soma"
)

type eventKind int

const (
	evUnknown eventKind = iota
	evInit
	evTextDelta
	evThinkingStart
	evToolUse
	evMessageEnd
	evResult
	evRateLimit
	evRetry
	evMessageStop
)

type event struct {
	kind eventKind

	sessionID    string
	mcpStatus    map[string]string
	apiKeySource string

	text string

	id   string
	name string
	args json.RawMessage

	stopReason string
	usage      soma.Usage

	write1h int // cache write tokens kept for an hour, part of usage.CacheWrite

	isError   bool
	totalCost float64
	allTokens int // tokens of every request Claude Code counts in totalCost

	overage  bool
	status   string
	resetsAt int64

	attempt    int
	maxRetries int
	delay      time.Duration
	errorKind  string
}

type usageBody struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	OutputDetails       struct {
		ThinkingTokens int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
	CacheCreation *cacheCreation `json:"cache_creation"`
	Iterations    []struct {
		CacheCreation *cacheCreation `json:"cache_creation"`
	} `json:"iterations"`
}

type cacheCreation struct {
	OneHour int `json:"ephemeral_1h_input_tokens"`
}

// write1h is the part of the cache write kept for an hour. message_delta
// reports it only per iteration, message_start at the top.
func (u usageBody) write1h() int {
	if u.CacheCreation != nil {
		return u.CacheCreation.OneHour
	}
	n := 0
	for _, it := range u.Iterations {
		if it.CacheCreation != nil {
			n += it.CacheCreation.OneHour
		}
	}
	return n
}

type modelUsage struct {
	InputTokens              int `json:"inputTokens"`
	OutputTokens             int `json:"outputTokens"`
	CacheReadInputTokens     int `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int `json:"cacheCreationInputTokens"`
}

func (u usageBody) usage() soma.Usage {
	return soma.Usage{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.CacheReadTokens,
		CacheWrite: u.CacheCreationTokens,
		Reasoning:  u.OutputDetails.ThinkingTokens,
	}
}

type mcpServer struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type rateLimitInfo struct {
	Status         string `json:"status"`
	ResetsAt       int64  `json:"resetsAt"`
	IsUsingOverage bool   `json:"isUsingOverage"`
}

type line struct {
	Type            string                `json:"type"`
	Subtype         string                `json:"subtype"`
	SessionID       string                `json:"session_id"`
	ParentToolUseID *string               `json:"parent_tool_use_id"`
	MCPServers      []mcpServer           `json:"mcp_servers"`
	APIKeySource    string                `json:"apiKeySource"`
	Event           json.RawMessage       `json:"event"`
	Message         json.RawMessage       `json:"message"`
	IsError         bool                  `json:"is_error"`
	Result          string                `json:"result"`
	Errors          []string              `json:"errors"`
	TotalCostUSD    float64               `json:"total_cost_usd"`
	ModelUsage      map[string]modelUsage `json:"modelUsage"`
	RateLimitInfo   *rateLimitInfo        `json:"rate_limit_info"`
	Attempt         int                   `json:"attempt"`
	MaxRetries      int                   `json:"max_retries"`
	RetryDelayMS    float64               `json:"retry_delay_ms"`
	Error           json.RawMessage       `json:"error"`
}

type streamEvent struct {
	Type         string `json:"type"`
	ContentBlock struct {
		Type string `json:"type"`
	} `json:"content_block"`
	Delta struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage *usageBody `json:"usage"`
}

type assistantMessage struct {
	Content []struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
}

// parseLine decodes one stdout line of `claude -p --output-format stream-json`.
// Lines of subagents, other stream events and unlisted types are evUnknown.
func parseLine(data []byte) (event, error) {
	var l line
	if err := json.Unmarshal(data, &l); err != nil {
		return event{}, fmt.Errorf("claude output line: %w", err)
	}
	switch l.Type {
	case "system":
		return parseSystem(l), nil
	case "stream_event":
		if l.ParentToolUseID != nil {
			return event{}, nil
		}
		return parseStream(l.Event)
	case "assistant":
		if l.ParentToolUseID != nil {
			return event{}, nil
		}
		var m assistantMessage
		if err := json.Unmarshal(l.Message, &m); err != nil {
			return event{}, fmt.Errorf("claude assistant line: %w", err)
		}
		for _, b := range m.Content {
			if b.Type == "tool_use" {
				return event{kind: evToolUse, id: b.ID, name: b.Name, args: b.Input}, nil
			}
		}
	case "result":
		text := l.Result
		if text == "" {
			text = strings.Join(l.Errors, "\n")
		}
		all := 0
		for _, m := range l.ModelUsage {
			all += m.InputTokens + m.OutputTokens + m.CacheReadInputTokens + m.CacheCreationInputTokens
		}
		return event{kind: evResult, isError: l.IsError, text: text, totalCost: l.TotalCostUSD, allTokens: all}, nil
	case "rate_limit_event":
		if i := l.RateLimitInfo; i != nil {
			return event{kind: evRateLimit, overage: i.IsUsingOverage, status: i.Status, resetsAt: i.ResetsAt}, nil
		}
	}
	return event{}, nil
}

func parseSystem(l line) event {
	switch l.Subtype {
	case "init":
		status := make(map[string]string, len(l.MCPServers))
		for _, s := range l.MCPServers {
			status[s.Name] = s.Status
		}
		return event{kind: evInit, sessionID: l.SessionID, mcpStatus: status, apiKeySource: l.APIKeySource}
	case "api_retry":
		var kind string
		_ = json.Unmarshal(l.Error, &kind)
		return event{
			kind:       evRetry,
			attempt:    l.Attempt,
			maxRetries: l.MaxRetries,
			delay:      time.Duration(l.RetryDelayMS * float64(time.Millisecond)),
			errorKind:  kind,
		}
	}
	return event{}
}

func parseStream(raw json.RawMessage) (event, error) {
	var e streamEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		return event{}, fmt.Errorf("claude stream event: %w", err)
	}
	switch e.Type {
	case "content_block_start":
		if e.ContentBlock.Type == "thinking" {
			return event{kind: evThinkingStart}, nil
		}
	case "content_block_delta":
		if e.Delta.Type == "text_delta" {
			return event{kind: evTextDelta, text: e.Delta.Text}, nil
		}
	case "message_stop":
		return event{kind: evMessageStop}, nil
	case "message_delta":
		ev := event{kind: evMessageEnd, stopReason: e.Delta.StopReason}
		if e.Usage != nil {
			ev.usage, ev.write1h = e.Usage.usage(), e.Usage.write1h()
		}
		return ev, nil
	}
	return event{}, nil
}

const mcpPrefix = "mcp__axon__"

func toolName(mcpName string) string { return strings.TrimPrefix(mcpName, mcpPrefix) }

func mcpName(tool string) string { return mcpPrefix + tool }

func effort(thinking string) string {
	switch thinking {
	case "low", "medium", "high", "xhigh":
		return thinking
	case "minimal", "off":
		return "low"
	}
	return ""
}

var readOnly = map[string]bool{
	"read":           true,
	"grep":           true,
	"find":           true,
	"multi_grep":     true,
	"ls":             true,
	"fetch_content":  true,
	"web_search":     true,
	"context_recall": true,
}

func mapError(text string) *soma.ProviderError {
	l := strings.ToLower(text)
	kind := soma.ErrOther
	switch {
	case containsAny(l, "not logged in", "login expired", "/login", "auth login", "authentication_failed"):
		kind = soma.ErrAuth
	case containsAny(l, "prompt is too long", "context window", "context_length", "context length"):
		kind = soma.ErrContextOverflow
	case containsAny(l, "rate_limit", "rate limit", "usage limit", "limit reached"):
		kind = soma.ErrQuota
	}
	return &soma.ProviderError{Kind: kind, Message: text}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
