package soma

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []ToolSpec
	Thinking  string // "off", "minimal", "low", "medium", "high", "xhigh"
	SessionID string
	Epoch     string // id of the log's latest compaction entry; "" when none
	LoadBlob  func(name string) ([]byte, error)
}

type DeltaKind string

const (
	DeltaText     DeltaKind = "text"
	DeltaThinking DeltaKind = "thinking"
	DeltaToolCall DeltaKind = "tool_call"
	DeltaNotice   DeltaKind = "notice"
)

type Delta struct {
	Kind DeltaKind
	Text string // a fragment; for DeltaToolCall the tool name, sent once when the call starts
}

type Provider interface {
	Stream(ctx context.Context, req Request, onDelta func(Delta)) (Message, error)
}

// RunEnder is implemented by a provider that keeps state for the length of a
// run; the agent calls EndRun when a run that used it returns.
type RunEnder interface{ EndRun(sessionID string) }

// ConversationHolder is implemented by a provider that keeps the
// conversation itself; true means the history is not collapsed for it.
type ConversationHolder interface{ HoldsConversation() bool }

type ErrorKind string

const (
	ErrRateLimit       ErrorKind = "rate_limit"
	ErrServer          ErrorKind = "server"
	ErrContextOverflow ErrorKind = "context_overflow"
	ErrAuth            ErrorKind = "auth"
	ErrOther           ErrorKind = "other"
	ErrNetwork         ErrorKind = "network" // connection failed or dropped; retried
	ErrQuota           ErrorKind = "quota"   // a 429 that reports an exhausted quota; not retried
)

type ProviderError struct {
	Kind       ErrorKind
	Status     int
	RetryAfter time.Duration
	Message    string
}

func (e *ProviderError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s (HTTP %d): %s", e.Kind, e.Status, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}
