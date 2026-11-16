package toolserver

import (
	"encoding/json"
	"fmt"
)

const (
	ProtocolVersion = "2025-11-25"
	ClientName      = "axon"
)

var SupportedVersions = map[string]bool{"2025-11-25": true, "2025-06-18": true, "2025-03-26": true, "2024-11-05": true}

type RPCMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"`
	MIMEType string `json:"mimeType"`
	URI      string `json:"uri"`
	Resource *struct {
		URI      string `json:"uri"`
		Text     string `json:"text"`
		MIMEType string `json:"mimeType"`
		Blob     string `json:"blob"`
	} `json:"resource"`
}

type CallResult struct {
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}
