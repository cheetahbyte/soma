package wire

import (
	"encoding/json"

	"github.com/cheetahbyte/soma"
	"github.com/cheetahbyte/soma/config"
)

const Version = 1
const HeaderProtocol = "Soma-Protocol"

type Request struct {
	Model     string         `json:"model"`
	System    string         `json:"system"`
	Messages  []soma.Message `json:"messages"`
	Tools     []Tool         `json:"tools"`
	Thinking  string         `json:"thinking"`
	SessionID string         `json:"sessionId"`
	Epoch     string         `json:"epoch"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

type StreamRequest struct {
	Upstream          string            `json:"upstream"`
	Request           Request           `json:"request"`
	Blobs             map[string]string `json:"blobs,omitempty"`
	AfterFailedStream bool              `json:"afterFailedStream,omitempty"`
}

type Line struct {
	Stream    string        `json:"stream,omitempty"`
	Delta     *Delta        `json:"delta,omitempty"`
	KeepAlive bool          `json:"keepAlive,omitempty"`
	Done      bool          `json:"done,omitempty"`
	Message   *soma.Message `json:"message,omitempty"`
	Error     *Error        `json:"error,omitempty"`
}

type Delta struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type Error struct {
	Provider     bool   `json:"provider"`
	Kind         string `json:"kind"`
	Status       int    `json:"status"`
	RetryAfterMS int64  `json:"retryAfterMs"`
	Message      string `json:"message"`
}

type MissingBlobs struct {
	Missing []string `json:"missing"`
}

type Abort struct {
	Stream string `json:"stream"`
}

type EndRun struct {
	Upstream  string `json:"upstream"`
	SessionID string `json:"sessionId"`
}

type Info struct {
	HoldsConversation bool               `json:"holdsConversation"`
	Models            []config.ModelInfo `json:"models"`
}
