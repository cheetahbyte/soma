package claudecode

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cheetahbyte/soma"
)

const apiName = "claude-code"

const toolResultLimit = 8000

// opaque is kept on each reply. Total and Tokens are Claude Code's running
// cost and token count when the reply ended, which a resumed process may
// start from again.
type opaque struct {
	ClaudeSession string  `json:"claudeSession"`
	Epoch         string  `json:"epoch"`
	Total         float64 `json:"total,omitempty"`
	Tokens        int     `json:"tokens,omitempty"`
}

type plan struct {
	Resume  string
	Base    opaque
	Send    []soma.Message
	History string
}

func sessionOf(m soma.Message) (opaque, bool) {
	if len(m.Blocks) == 0 || len(m.Blocks[0].Opaque) == 0 {
		return opaque{}, false
	}
	var o opaque
	if json.Unmarshal(m.Blocks[0].Opaque, &o) != nil || o.ClaudeSession == "" {
		return opaque{}, false
	}
	return o, true
}

// decide applies the continue rule: resume the conversation of the last reply
// of this provider when nothing since then breaks the match, otherwise start
// a new one. When the messages end in a tool result or a reply, Send is empty
// and History holds everything.
func decide(providerName string, req soma.Request) plan {
	msgs := req.Messages
	last := -1
	var session opaque
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != soma.RoleAssistant || m.API != apiName || m.Provider != providerName {
			continue
		}
		if o, ok := sessionOf(m); ok {
			last, session = i, o
			break
		}
	}
	if last >= 0 && session.Epoch == req.Epoch && !otherProviderAfter(providerName, msgs[last+1:]) {
		p := plan{Resume: session.ClaudeSession, Base: session}
		for _, m := range msgs[last+1:] {
			if m.Role == soma.RoleUser {
				p.Send = append(p.Send, m)
			}
		}
		return p
	}
	return newConversation(msgs)
}

// newConversation is the plan for a conversation that starts from msgs.
func newConversation(msgs []soma.Message) plan {
	if n := len(msgs); n > 0 && msgs[n-1].Role == soma.RoleUser {
		return plan{Send: msgs[n-1:], History: historyText(msgs[:n-1])}
	}
	return plan{History: historyText(msgs)}
}

// lateResults is the text of the tool results that follow the last reply,
// or "". Claude Code gets results only through the held calls of the
// process that made them, so a new process resuming the conversation
// receives them as text before its first message.
func lateResults(msgs []soma.Message) string {
	start := len(msgs)
	for start > 0 && msgs[start-1].Role != soma.RoleAssistant {
		start--
	}
	var b strings.Builder
	for _, m := range msgs[start:] {
		if m.Role == soma.RoleTool {
			fmt.Fprintf(&b, "\nTool result (%s):\n%s\n", m.ToolName, clip(blocksText(m.Blocks)))
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "<tool_results>\nThe results of the tool calls of your last reply, which ended before they reached you.\n" + b.String() + "</tool_results>"
}

func otherProviderAfter(providerName string, msgs []soma.Message) bool {
	for _, m := range msgs {
		if m.Role == soma.RoleAssistant && (m.API != apiName || m.Provider != providerName) {
			return true
		}
	}
	return false
}

func historyText(msgs []soma.Message) string {
	if len(msgs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<conversation_history>\n")
	b.WriteString("This is the earlier conversation, replayed as text because it continues in a new session.\n")
	for _, m := range msgs {
		b.WriteString("\n")
		switch m.Role {
		case soma.RoleUser:
			b.WriteString("User:\n")
			b.WriteString(blocksText(m.Blocks))
		case soma.RoleAssistant:
			b.WriteString("Assistant:\n")
			b.WriteString(blocksText(m.Blocks))
		case soma.RoleTool:
			fmt.Fprintf(&b, "Tool result (%s):\n", m.ToolName)
			b.WriteString(clip(blocksText(m.Blocks)))
		}
		b.WriteString("\n")
	}
	b.WriteString("</conversation_history>")
	return b.String()
}

func blocksText(blocks []soma.Block) string {
	parts := make([]string, 0, len(blocks))
	for _, bl := range blocks {
		switch bl.Type {
		case soma.BlockText:
			parts = append(parts, bl.Text)
		case soma.BlockImage:
			parts = append(parts, "[image]")
		case soma.BlockToolCall:
			parts = append(parts, fmt.Sprintf("[called %s %s]", bl.Name, bl.Args))
		}
	}
	return strings.Join(parts, "\n")
}

func clip(s string) string {
	if len(s) <= toolResultLimit {
		return s
	}
	half := toolResultLimit / 2
	end := half
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	head := s[:end]
	start := len(s) - half
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	tail := s[start:]
	return fmt.Sprintf("%s\n[… %d bytes omitted …]\n%s", head, len(s)-len(head)-len(tail), tail)
}

type contentBlock struct {
	Type   string       `json:"type"`
	Text   string       `json:"text,omitempty"`
	Source *imageSource `json:"source,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// userLine renders a user message as one stdin line, newline included. prefix
// is put before the first text, as its own text block when there is none.
func userLine(m soma.Message, prefix string, load func(string) ([]byte, error)) ([]byte, error) {
	var content any
	hasImage := false
	for _, b := range m.Blocks {
		if b.Type == soma.BlockImage {
			hasImage = true
		}
	}
	if !hasImage {
		text := m.Text()
		if prefix != "" {
			text = prefix + "\n\n" + text
		}
		content = text
	} else {
		var blocks []contentBlock
		if prefix != "" {
			blocks = append(blocks, contentBlock{Type: "text", Text: prefix})
		}
		for _, b := range m.Blocks {
			switch b.Type {
			case soma.BlockText:
				blocks = append(blocks, contentBlock{Type: "text", Text: b.Text})
			case soma.BlockImage:
				data, err := load(b.Blob)
				if err != nil {
					return nil, fmt.Errorf("load image %s: %w", b.Blob, err)
				}
				blocks = append(blocks, contentBlock{Type: "image", Source: &imageSource{
					Type:      "base64",
					MediaType: b.MIME,
					Data:      base64.StdEncoding.EncodeToString(data),
				}})
			}
		}
		content = blocks
	}
	out, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": content},
	})
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}
