package chat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cheetahbyte/soma"
	"github.com/cheetahbyte/soma/config"
	"github.com/cheetahbyte/soma/provider/wire"
)

type callSlot struct {
	id   string
	name string
	args strings.Builder
}

type streamState struct {
	model   string
	info    config.ModelInfo
	onDelta func(soma.Delta)

	thinking      strings.Builder
	thinkingField string
	text          strings.Builder
	calls         []*callSlot
	byIndex       map[int]*callSlot
	byID          map[string]*callSlot

	finish  string
	usage   *soma.Usage
	failure error
}

func newStreamState(model string, info config.ModelInfo, onDelta func(soma.Delta)) *streamState {
	if onDelta == nil {
		onDelta = func(soma.Delta) {}
	}
	return &streamState{model: model, info: info, onDelta: onDelta, byIndex: map[int]*callSlot{}, byID: map[string]*callSlot{}}
}

type usageBody struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens     *int `json:"cached_tokens"`
		CacheWriteTokens int  `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	PromptCacheHitTokens    *int `json:"prompt_cache_hit_tokens"`
	CachedTokens            *int `json:"cached_tokens"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type callDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chunk struct {
	Choices []struct {
		Delta struct {
			Content          json.RawMessage `json:"content"`
			ReasoningContent json.RawMessage `json:"reasoning_content"`
			Reasoning        json.RawMessage `json:"reasoning"`
			ReasoningText    json.RawMessage `json:"reasoning_text"`
			ToolCalls        []callDelta     `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string     `json:"finish_reason"`
		Usage        *usageBody `json:"usage"`
	} `json:"choices"`
	Usage *usageBody `json:"usage"`
	Error *struct {
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
	} `json:"error"`
}

// handle applies one event and reports whether the stream is finished.
func (s *streamState) handle(data []byte) bool {
	if string(data) == "[DONE]" {
		return true
	}
	var c chunk
	if json.Unmarshal(data, &c) != nil {
		return false
	}
	if c.Error != nil {
		msg := c.Error.Message
		if msg == "" {
			msg = "error event without a message"
		}
		s.failure = wire.ClassifyMessage(msg)
		return true
	}
	if c.Usage != nil {
		s.readUsage(c.Usage)
	}
	if len(c.Choices) == 0 {
		return false
	}
	ch := c.Choices[0]
	if c.Usage == nil && ch.Usage != nil {
		s.readUsage(ch.Usage)
	}
	if ch.FinishReason != "" {
		s.finish = ch.FinishReason
	}
	d := ch.Delta
	// Reasoning before content: some proxies put both in one delta, and the
	// thinking block comes first.
	for i, raw := range []json.RawMessage{d.ReasoningContent, d.Reasoning, d.ReasoningText} {
		var text string
		if json.Unmarshal(raw, &text) != nil || text == "" {
			continue
		}
		if s.thinkingField == "" {
			s.thinkingField = reasoningFields[i]
		}
		s.thinking.WriteString(text)
		s.onDelta(soma.Delta{Kind: soma.DeltaThinking, Text: text})
		break
	}
	var content string
	if json.Unmarshal(d.Content, &content) == nil && content != "" {
		s.text.WriteString(content)
		s.onDelta(soma.Delta{Kind: soma.DeltaText, Text: content})
	}
	for _, tc := range d.ToolCalls {
		s.callFragment(tc)
	}
	return false
}

// callFragment joins a fragment to its call by index, else by id; a
// fragment with neither continues the latest call.
func (s *streamState) callFragment(tc callDelta) {
	var sl *callSlot
	switch {
	case tc.Index != nil:
		sl = s.byIndex[*tc.Index]
	case tc.ID != "":
		sl = s.byID[tc.ID]
	case len(s.calls) > 0:
		sl = s.calls[len(s.calls)-1]
	}
	if sl == nil && tc.ID != "" {
		sl = s.byID[tc.ID]
	}
	if sl == nil {
		sl = &callSlot{}
		s.calls = append(s.calls, sl)
	}
	if tc.Index != nil {
		s.byIndex[*tc.Index] = sl
	}
	if sl.id == "" && tc.ID != "" {
		sl.id = tc.ID
		s.byID[tc.ID] = sl
	}
	if sl.name == "" && tc.Function.Name != "" {
		sl.name = tc.Function.Name
		s.onDelta(soma.Delta{Kind: soma.DeltaToolCall, Text: sl.name})
	}
	sl.args.WriteString(tc.Function.Arguments)
}

func (s *streamState) readUsage(u *usageBody) {
	cached, written := 0, 0
	switch {
	case u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens != nil:
		cached = *u.PromptTokensDetails.CachedTokens
	case u.PromptCacheHitTokens != nil:
		cached = *u.PromptCacheHitTokens
	case u.CachedTokens != nil:
		cached = *u.CachedTokens
	}
	if u.PromptTokensDetails != nil {
		written = u.PromptTokensDetails.CacheWriteTokens
	}
	usage := &soma.Usage{
		Input:      max(0, u.PromptTokens-cached-written),
		Output:     u.CompletionTokens,
		CacheRead:  cached,
		CacheWrite: written,
	}
	if u.CompletionTokensDetails != nil {
		usage.Reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	usage.Cost = (float64(usage.Input+usage.CacheWrite)*s.info.InputPrice +
		float64(usage.Output)*s.info.OutputPrice +
		float64(usage.CacheRead)*s.info.CachedPrice) / 1e6
	s.usage = usage
}

func (s *streamState) final() (soma.Message, error) {
	if s.failure != nil {
		return soma.Message{}, s.failure
	}
	m := s.message(false)
	switch s.finish {
	case "":
		return soma.Message{}, wire.NetworkError(fmt.Errorf("%w: no finish_reason", wire.ErrIncompleteStream))
	case "stop", "end":
		m.StopReason = soma.StopEnd
	case "length":
		m.StopReason = soma.StopLength
	case "tool_calls", "function_call":
		m.StopReason = soma.StopToolCalls
	default:
		return soma.Message{}, &soma.ProviderError{Kind: soma.ErrOther, Message: fmt.Sprintf("the reply ended with finish_reason %q", s.finish)}
	}
	return m, nil
}

func (s *streamState) partial() soma.Message {
	m := s.message(true)
	m.StopReason = soma.StopAborted
	return m
}

// message assembles thinking, text, and calls. For an aborted reply, a call
// is kept only when it has a name and its arguments form a JSON object, the
// only sign Chat Completions gives that a call is complete.
func (s *streamState) message(aborted bool) soma.Message {
	m := soma.Message{Role: soma.RoleAssistant, API: api, Model: s.model, Usage: s.usage, Blocks: []soma.Block{}}
	if s.thinking.Len() > 0 {
		ref, _ := json.Marshal(thinkingRef{Field: s.thinkingField})
		m.Blocks = append(m.Blocks, soma.Block{Type: soma.BlockThinking, Text: s.thinking.String(), Opaque: ref})
	}
	if s.text.Len() > 0 {
		m.Blocks = append(m.Blocks, soma.Block{Type: soma.BlockText, Text: s.text.String()})
	}
	for i, sl := range s.calls {
		args := strings.TrimSpace(sl.args.String())
		if args == "" {
			args = "{}"
		}
		raw := json.RawMessage(args)
		var obj map[string]json.RawMessage
		complete := sl.name != "" && json.Unmarshal(raw, &obj) == nil
		if aborted && !complete {
			continue
		}
		if !json.Valid(raw) {
			raw, _ = json.Marshal(args)
		}
		id := sl.id
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		m.Blocks = append(m.Blocks, soma.Block{Type: soma.BlockToolCall, CallID: id, Name: sl.name, Args: raw})
	}
	return m
}
