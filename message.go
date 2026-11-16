package soma

import (
	"encoding/json"
	"strings"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type BlockType string

const (
	BlockText     BlockType = "text"
	BlockThinking BlockType = "thinking"
	BlockToolCall BlockType = "tool_call"
	BlockImage    BlockType = "image"
)

type Block struct {
	Type   BlockType       `json:"type"`
	Text   string          `json:"text,omitempty"`
	CallID string          `json:"callId,omitempty"`
	Name   string          `json:"name,omitempty"`
	Args   json.RawMessage `json:"args,omitempty"`
	MIME   string          `json:"mime,omitempty"`
	Blob   string          `json:"blob,omitempty"`
	Opaque json.RawMessage `json:"opaque,omitempty"`
}

type Usage struct {
	Input      int     `json:"input"` // excludes cached tokens
	Output     int     `json:"output"`
	CacheRead  int     `json:"cacheRead"`
	CacheWrite int     `json:"cacheWrite"`
	Reasoning  int     `json:"reasoning"`
	Cost       float64 `json:"cost"`
}

// CacheHitPercent is the share of the input tokens that were read from the
// cache, rounded down. ok is false when no input was used.
func (u Usage) CacheHitPercent() (percent int, ok bool) {
	total := u.Input + u.CacheRead + u.CacheWrite
	if total <= 0 {
		return 0, false
	}
	return u.CacheRead * 100 / total, true
}

// Sub removes o, which an earlier reading of the same total held.
func (u *Usage) Sub(o Usage) {
	u.Input -= o.Input
	u.Output -= o.Output
	u.CacheRead -= o.CacheRead
	u.CacheWrite -= o.CacheWrite
	u.Reasoning -= o.Reasoning
	u.Cost -= o.Cost
}

func (u *Usage) Add(o Usage) {
	u.Input += o.Input
	u.Output += o.Output
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
	u.Reasoning += o.Reasoning
	u.Cost += o.Cost
}

type StopReason string

const (
	StopEnd       StopReason = "stop"
	StopToolCalls StopReason = "tool_calls"
	StopLength    StopReason = "length"
	StopAborted   StopReason = "aborted"
	StopError     StopReason = "error"
)

type Message struct {
	Role       Role       `json:"role"`
	Blocks     []Block    `json:"blocks"`
	CallID     string     `json:"callId,omitempty"`
	ToolName   string     `json:"toolName,omitempty"`
	IsError    bool       `json:"isError,omitempty"`
	Provider   string     `json:"provider,omitempty"`
	API        string     `json:"api,omitempty"`
	Model      string     `json:"model,omitempty"`
	Thinking   string     `json:"thinking,omitempty"`
	Usage      *Usage     `json:"usage,omitempty"`
	StopReason StopReason `json:"stopReason,omitempty"`
}

func (m Message) Text() string {
	var b strings.Builder
	for _, block := range m.Blocks {
		if block.Type == BlockText {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}
