package openai

import (
	"encoding/json"
	"strings"

	"github.com/cheetahbyte/soma"
	"github.com/cheetahbyte/soma/config"
	"github.com/cheetahbyte/soma/provider/wire"
)

type slot struct {
	block    soma.Block
	args     strings.Builder
	finished bool
}

type streamState struct {
	model   string
	api     string
	info    config.ModelInfo
	onDelta func(soma.Delta)

	slots   []*slot
	byIndex map[int]*slot

	done             bool
	status           string
	incompleteReason string
	usage            *soma.Usage
	failure          error
}

func newStreamState(model, api string, info config.ModelInfo, onDelta func(soma.Delta)) *streamState {
	if onDelta == nil {
		onDelta = func(soma.Delta) {}
	}
	return &streamState{model: model, api: api, info: info, onDelta: onDelta, byIndex: map[int]*slot{}}
}

type outputItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Phase     string `json:"phase"`
	Content   []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
}

type responseBody struct {
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Usage *struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens     int `json:"cached_tokens"`
			CacheWriteTokens int `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
		OutputTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

type event struct {
	Type        string          `json:"type"`
	OutputIndex int             `json:"output_index"`
	Delta       string          `json:"delta"`
	Arguments   *string         `json:"arguments"`
	Item        json.RawMessage `json:"item"`
	Response    *responseBody   `json:"response"`
	Code        string          `json:"code"`
	Message     string          `json:"message"`
}

// handle applies one event and reports whether the stream is finished.
func (s *streamState) handle(data []byte) bool {
	if string(data) == "[DONE]" {
		return s.done
	}
	var ev event
	if json.Unmarshal(data, &ev) != nil {
		return false
	}
	switch ev.Type {
	case "response.output_item.added":
		var item outputItem
		if json.Unmarshal(ev.Item, &item) == nil {
			s.start(ev.OutputIndex, item)
		}
	case "response.output_text.delta", "response.refusal.delta":
		if sl := s.slotOf(ev.OutputIndex, soma.BlockText); sl != nil {
			sl.block.Text += ev.Delta
			s.onDelta(soma.Delta{Kind: soma.DeltaText, Text: ev.Delta})
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if sl := s.slotOf(ev.OutputIndex, soma.BlockThinking); sl != nil {
			sl.block.Text += ev.Delta
			s.onDelta(soma.Delta{Kind: soma.DeltaThinking, Text: ev.Delta})
		}
	case "response.reasoning_summary_part.done":
		if sl := s.slotOf(ev.OutputIndex, soma.BlockThinking); sl != nil {
			sl.block.Text += "\n\n"
			s.onDelta(soma.Delta{Kind: soma.DeltaThinking, Text: "\n\n"})
		}
	case "response.function_call_arguments.delta":
		if sl := s.slotOf(ev.OutputIndex, soma.BlockToolCall); sl != nil {
			sl.args.WriteString(ev.Delta)
		}
	case "response.function_call_arguments.done":
		if sl := s.slotOf(ev.OutputIndex, soma.BlockToolCall); sl != nil && ev.Arguments != nil {
			sl.args.Reset()
			sl.args.WriteString(*ev.Arguments)
		}
	case "response.output_item.done":
		var item outputItem
		if json.Unmarshal(ev.Item, &item) == nil {
			s.finish(ev.OutputIndex, item, ev.Item)
		}
	case "response.completed", "response.incomplete":
		s.done = true
		if ev.Response != nil {
			s.status = ev.Response.Status
			if ev.Response.IncompleteDetails != nil {
				s.incompleteReason = ev.Response.IncompleteDetails.Reason
			}
			s.readUsage(ev.Response)
		}
		if s.status == "" {
			s.status = strings.TrimPrefix(ev.Type, "response.")
		}
		return true
	case "response.failed":
		s.done = true
		msg := "response failed"
		if r := ev.Response; r != nil && r.Error != nil {
			msg = r.Error.Code + ": " + r.Error.Message
		} else if r != nil && r.IncompleteDetails != nil {
			msg = "incomplete: " + r.IncompleteDetails.Reason
		}
		s.failure = wire.ClassifyMessage(msg)
		return true
	case "error":
		s.done = true
		s.failure = wire.ClassifyMessage(strings.TrimSpace(ev.Code + ": " + ev.Message))
		return true
	}
	return false
}

func (s *streamState) start(index int, item outputItem) *slot {
	var sl *slot
	switch item.Type {
	case "reasoning":
		sl = &slot{block: soma.Block{Type: soma.BlockThinking}}
	case "message":
		sl = &slot{block: soma.Block{Type: soma.BlockText}}
	case "function_call":
		sl = &slot{block: soma.Block{Type: soma.BlockToolCall, CallID: item.CallID, Name: item.Name}}
		sl.args.WriteString(item.Arguments)
		s.onDelta(soma.Delta{Kind: soma.DeltaToolCall, Text: item.Name})
	default:
		return nil
	}
	s.slots = append(s.slots, sl)
	s.byIndex[index] = sl
	return sl
}

func (s *streamState) slotOf(index int, t soma.BlockType) *slot {
	sl := s.byIndex[index]
	if sl == nil || sl.block.Type != t || sl.finished {
		return nil
	}
	return sl
}

func (s *streamState) finish(index int, item outputItem, raw json.RawMessage) {
	sl := s.byIndex[index]
	if sl == nil || sl.finished {
		sl = s.start(index, item)
		if sl == nil {
			return
		}
	}
	switch item.Type {
	case "reasoning":
		var parts []string
		for _, p := range item.Summary {
			parts = append(parts, p.Text)
		}
		if text := strings.Join(parts, "\n\n"); text != "" {
			sl.block.Text = text
		}
		sl.block.Opaque = append(json.RawMessage(nil), raw...)
	case "message":
		var b strings.Builder
		for _, c := range item.Content {
			if c.Type == "output_text" {
				b.WriteString(c.Text)
			} else {
				b.WriteString(c.Refusal)
			}
		}
		sl.block.Text = b.String()
		sl.block.Opaque, _ = json.Marshal(wire.ItemRef{ID: item.ID, Phase: item.Phase})
	case "function_call":
		if item.Arguments != "" {
			sl.args.Reset()
			sl.args.WriteString(item.Arguments)
		}
		if item.CallID != "" {
			sl.block.CallID = item.CallID
		}
		sl.block.Opaque, _ = json.Marshal(wire.ItemRef{ID: item.ID})
	}
	sl.finished = true
}

func (s *streamState) readUsage(r *responseBody) {
	if r.Usage == nil {
		return
	}
	u := r.Usage
	cached := u.InputTokensDetails.CachedTokens
	written := u.InputTokensDetails.CacheWriteTokens
	usage := &soma.Usage{
		Input:      max(0, u.InputTokens-cached-written),
		Output:     u.OutputTokens,
		CacheRead:  cached,
		CacheWrite: written,
		Reasoning:  u.OutputTokensDetails.ReasoningTokens,
	}
	usage.Cost = (float64(usage.Input+usage.CacheWrite)*s.info.InputPrice +
		float64(usage.Output)*s.info.OutputPrice +
		float64(usage.CacheRead)*s.info.CachedPrice) / 1e6
	s.usage = usage
}

func (s *streamState) unfinishedCalls() int {
	n := 0
	for _, sl := range s.slots {
		if sl.block.Type == soma.BlockToolCall && !sl.finished {
			n++
		}
	}
	return n
}

// message assembles the blocks in output order. For an aborted reply, tool
// calls whose arguments did not finish are left out.
func (s *streamState) message(aborted bool) soma.Message {
	m := soma.Message{Role: soma.RoleAssistant, API: s.api, Model: s.model, Usage: s.usage}
	for _, sl := range s.slots {
		b := sl.block
		switch b.Type {
		case soma.BlockText:
			if b.Text == "" {
				continue
			}
		case soma.BlockThinking:
			if b.Text == "" && len(b.Opaque) == 0 {
				continue
			}
		case soma.BlockToolCall:
			if aborted && !sl.finished {
				continue
			}
			args := strings.TrimSpace(sl.args.String())
			if args == "" {
				args = "{}"
			}
			b.Args = json.RawMessage(args)
			if !json.Valid(b.Args) {
				b.Args, _ = json.Marshal(args)
			}
		}
		m.Blocks = append(m.Blocks, b)
	}
	if m.Blocks == nil {
		m.Blocks = []soma.Block{}
	}
	return m
}
