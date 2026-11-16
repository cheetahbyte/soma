package chat

import (
	"bufio"
	"encoding/json"
	"strings"

	"github.com/cheetahbyte/soma"
	"github.com/cheetahbyte/soma/provider/wire"
)

// Reasoning arrives in one of these delta fields, and is replayed in one.
var reasoningFields = []string{"reasoning_content", "reasoning", "reasoning_text"}

// thinkingRef is what a thinking block keeps in Opaque: the delta field its
// text arrived in.
type thinkingRef struct {
	Field string `json:"field"`
}

// replay says how reasoning of earlier replies is sent back.
type replay struct {
	provider string // only replies of this provider and model carry their reasoning
	model    string
	as       string // the provider's replayReasoningAs
	deepseek bool   // always replay as reasoning_content, empty when there was none
}

type (
	textMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	partsMessage struct {
		Role    string `json:"role"`
		Content []part `json:"content"`
	}
	toolMessage struct {
		Role       string `json:"role"`
		ToolCallID string `json:"tool_call_id"`
		Content    string `json:"content"`
	}
	assistantMessage struct {
		Role             string     `json:"role"`
		Content          *string    `json:"content"`
		ToolCalls        []toolCall `json:"tool_calls,omitempty"`
		ReasoningContent *string    `json:"reasoning_content,omitempty"`
		Reasoning        *string    `json:"reasoning,omitempty"`
		ReasoningText    *string    `json:"reasoning_text,omitempty"`
	}
)

type part struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function callFunction `json:"function"`
}

type callFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type messageShape interface {
	textMessage | partsMessage | toolMessage | assistantMessage
}

type messageWriter struct {
	buf *bufio.Writer
	enc *json.Encoder
	n   int
}

func writeMessage[T messageShape](w *messageWriter, m T) error {
	if w.n > 0 {
		w.buf.WriteByte(',')
	}
	w.n++
	return w.enc.Encode(m)
}

// writeMessages writes the messages for req to buf, separated by commas,
// without the surrounding brackets.
func writeMessages(buf *bufio.Writer, req soma.Request, rp replay) error {
	w := &messageWriter{buf: buf, enc: json.NewEncoder(buf)}
	if req.System != "" {
		if err := writeMessage(w, textMessage{Role: "system", Content: req.System}); err != nil {
			return err
		}
	}
	// Call ids of other providers are normalized; their results must follow.
	ids := map[string]string{}
	msgs := req.Messages
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		var err error
		switch m.Role {
		case soma.RoleUser:
			err = writeUser(w, m.Blocks, req.LoadBlob)
		case soma.RoleAssistant:
			a := assistant(m, rp, ids)
			if a.Content == nil && len(a.ToolCalls) == 0 {
				continue
			}
			err = writeMessage(w, a)
		case soma.RoleTool:
			// Images cannot go in a tool message; those of a run of tool
			// results follow it in one user message.
			var images []part
			for ; i < len(msgs) && msgs[i].Role == soma.RoleTool; i++ {
				t := msgs[i]
				var own []part
				for _, b := range t.Blocks {
					if b.Type == soma.BlockImage {
						own = append(own, imagePart(b, req.LoadBlob))
					}
				}
				content := wire.ToolOutputText(t.Blocks)
				if len(own) > 0 && !hasText(t.Blocks) {
					content = "(see attached image)"
				}
				id := t.CallID
				if n, ok := ids[t.CallID]; ok {
					id = n
				}
				if err = writeMessage(w, toolMessage{Role: "tool", ToolCallID: id, Content: content}); err != nil {
					return err
				}
				images = append(images, own...)
			}
			i--
			if len(images) > 0 {
				err = writeMessage(w, partsMessage{Role: "user", Content: append([]part{{Type: "text", Text: "Attached image(s) from tool result:"}}, images...)})
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func hasText(blocks []soma.Block) bool {
	for _, b := range blocks {
		if b.Type == soma.BlockText && strings.TrimSpace(b.Text) != "" {
			return true
		}
	}
	return false
}

func writeUser(w *messageWriter, blocks []soma.Block, load func(string) ([]byte, error)) error {
	var texts []string
	var parts []part
	hasImage := false
	for _, b := range blocks {
		switch b.Type {
		case soma.BlockText:
			if b.Text != "" {
				texts = append(texts, b.Text)
				parts = append(parts, part{Type: "text", Text: b.Text})
			}
		case soma.BlockImage:
			hasImage = true
			parts = append(parts, imagePart(b, load))
		}
	}
	switch {
	case hasImage:
		return writeMessage(w, partsMessage{Role: "user", Content: parts})
	case len(texts) > 0:
		return writeMessage(w, textMessage{Role: "user", Content: strings.Join(texts, "\n")})
	}
	return nil
}

func imagePart(b soma.Block, load func(string) ([]byte, error)) part {
	url, err := wire.DataURL(b, load)
	if err != nil {
		return part{Type: "text", Text: "(image unavailable: " + err.Error() + ")"}
	}
	return part{Type: "image_url", ImageURL: &imageURL{URL: url}}
}

func assistant(m soma.Message, rp replay, ids map[string]string) assistantMessage {
	var text strings.Builder
	var reasoning []string
	field := ""
	out := assistantMessage{Role: "assistant"}
	replayable := wire.Replayable(m, rp.provider, api, rp.model)
	for _, b := range m.Blocks {
		switch b.Type {
		case soma.BlockText:
			text.WriteString(b.Text)
		case soma.BlockThinking:
			if !replayable || b.Text == "" {
				continue
			}
			reasoning = append(reasoning, b.Text)
			var ref thinkingRef
			if field == "" && json.Unmarshal(b.Opaque, &ref) == nil {
				field = ref.Field
			}
		case soma.BlockToolCall:
			id := b.CallID
			// Only a Responses API call keeps its item in Opaque.
			var ref wire.ItemRef
			if !replayable && len(b.Opaque) > 0 && json.Unmarshal(b.Opaque, &ref) == nil {
				id = wire.ChatCallID(b.CallID, ref.ID)
				ids[b.CallID] = id
			}
			out.ToolCalls = append(out.ToolCalls, toolCall{
				ID:       id,
				Type:     "function",
				Function: callFunction{Name: b.Name, Arguments: wire.CallArguments(b.Args)},
			})
		}
	}
	if s := text.String(); s != "" {
		out.Content = &s
	}
	switch {
	case rp.deepseek:
		field = "reasoning_content"
	case len(reasoning) == 0:
		return out
	case rp.as != "":
		field = rp.as
	case field == "":
		field = reasoningFields[0]
	}
	r := strings.Join(reasoning, "\n")
	switch field {
	case "reasoning":
		out.Reasoning = &r
	case "reasoning_text":
		out.ReasoningText = &r
	default:
		out.ReasoningContent = &r
	}
	return out
}
