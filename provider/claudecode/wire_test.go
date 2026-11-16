package claudecode

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/cheetahbyte/soma"
)

func TestParseLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want event
	}{
		{
			"init",
			`{"type":"system","subtype":"init","session_id":"ebdc776d-0fe7-4fb6-b84c-f9a75720480b","mcp_servers":[{"name":"axon","status":"connected","source":"dynamic"}],"apiKeySource":"none"}`,
			event{kind: evInit, sessionID: "ebdc776d-0fe7-4fb6-b84c-f9a75720480b", mcpStatus: map[string]string{"axon": "connected"}, apiKeySource: "none"},
		},
		{
			"text delta",
			`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"2+"}},"session_id":"s","parent_tool_use_id":null}`,
			event{kind: evTextDelta, text: "2+"},
		},
		{
			"thinking start",
			`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}},"session_id":"s","parent_tool_use_id":null}`,
			event{kind: evThinkingStart},
		},
		{
			"tool use",
			`{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"tool_use","id":"toolu_012P33TrcEHSsm2zq7DDf9AT","name":"mcp__axon__add","input":{"a":2,"b":3},"caller":{"type":"direct"}}]},"parent_tool_use_id":null,"session_id":"s"}`,
			event{kind: evToolUse, id: "toolu_012P33TrcEHSsm2zq7DDf9AT", name: "mcp__axon__add", args: json.RawMessage(`{"a":2,"b":3}`)},
		},
		{
			"message end",
			`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":2,"cache_creation_input_tokens":651,"cache_read_input_tokens":642,"output_tokens":143,"output_tokens_details":{"thinking_tokens":50}}},"session_id":"s","parent_tool_use_id":null}`,
			event{kind: evMessageEnd, stopReason: "tool_use", usage: soma.Usage{Input: 2, Output: 143, CacheRead: 642, CacheWrite: 651, Reasoning: 50}},
		},
		{
			"result",
			`{"type":"result","subtype":"success","is_error":false,"result":"pong","stop_reason":"end_turn","session_id":"s","total_cost_usd":0.00027845,"usage":{"input_tokens":4}}`,
			event{kind: evResult, text: "pong", totalCost: 0.00027845},
		},
		{
			"result error",
			`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["[ede_diagnostic] result_type=user last_content_type=n/a stop_reason=null"],"total_cost_usd":0}`,
			event{kind: evResult, isError: true, text: "[ede_diagnostic] result_type=user last_content_type=n/a stop_reason=null"},
		},
		{
			"rate limit",
			`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1794920400,"rateLimitType":"seven_day","overageStatus":"rejected","isUsingOverage":true},"session_id":"s"}`,
			event{kind: evRateLimit, overage: true, status: "allowed", resetsAt: 1794920400},
		},
		{
			"retry",
			`{"type":"system","subtype":"api_retry","attempt":2,"max_retries":10,"retry_delay_ms":1500,"error_status":529,"error":"overloaded"}`,
			event{kind: evRetry, attempt: 2, maxRetries: 10, delay: 1500 * time.Millisecond, errorKind: "overloaded"},
		},
		{"status ignored", `{"type":"system","subtype":"status","status":"requesting"}`, event{}},
		{"message start ignored", `{"type":"stream_event","event":{"type":"message_start","message":{}},"parent_tool_use_id":null}`, event{}},
		{"tool input delta ignored", `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}},"parent_tool_use_id":null}`, event{}},
		{"user line ignored", `{"type":"user","message":{"role":"user","content":[]}}`, event{}},
		{"subagent text ignored", `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}},"parent_tool_use_id":"toolu_1"}`, event{}},
		{"text assistant ignored", `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]},"parent_tool_use_id":null}`, event{}},
	}
	for _, c := range cases {
		got, err := parseLine([]byte(c.line))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
	if _, err := parseLine([]byte(`not json`)); err == nil {
		t.Error("malformed line: want an error")
	}
}

func TestEffort(t *testing.T) {
	want := map[string]string{
		"low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh",
		"minimal": "low", "off": "low", "": "", "max": "",
	}
	for in, out := range want {
		if got := effort(in); got != out {
			t.Errorf("effort(%q) = %q, want %q", in, got, out)
		}
	}
}

func TestToolNames(t *testing.T) {
	if got := toolName("mcp__axon__read"); got != "read" {
		t.Errorf("toolName = %q", got)
	}
	if got := toolName("other"); got != "other" {
		t.Errorf("toolName without prefix = %q", got)
	}
	if got := mcpName("multi_grep"); got != "mcp__axon__multi_grep" {
		t.Errorf("mcpName = %q", got)
	}
	if len(readOnly) != 8 || readOnly["bash"] || !readOnly["context_recall"] {
		t.Errorf("readOnly = %v", readOnly)
	}
}

func TestMapError(t *testing.T) {
	cases := []struct {
		text string
		kind soma.ErrorKind
	}{
		{"Login expired · Please run /login", soma.ErrAuth},
		{"Not logged in · Please run /login", soma.ErrAuth},
		{"Prompt is too long", soma.ErrContextOverflow},
		{"You've hit your usage limit", soma.ErrQuota},
		{"rate_limit", soma.ErrQuota},
		{"model not found", soma.ErrOther},
	}
	for _, c := range cases {
		got := mapError(c.text)
		if got.Kind != c.kind || got.Message != c.text || got.Status != 0 {
			t.Errorf("mapError(%q) = %+v, want kind %s", c.text, got, c.kind)
		}
	}
}
