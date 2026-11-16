package claudecode

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cheetahbyte/soma"
)

func user(text string) soma.Message {
	return soma.Message{Role: soma.RoleUser, Blocks: []soma.Block{{Type: soma.BlockText, Text: text}}}
}

func reply(provider, api, session, epoch string) soma.Message {
	b := soma.Block{Type: soma.BlockText, Text: "ok"}
	if session != "" {
		b.Opaque = json.RawMessage(`{"claudeSession":"` + session + `","epoch":"` + epoch + `"}`)
	}
	return soma.Message{Role: soma.RoleAssistant, Provider: provider, API: api, Blocks: []soma.Block{b}}
}

func toolResult(name, text string) soma.Message {
	return soma.Message{Role: soma.RoleTool, ToolName: name, CallID: "c1", Blocks: []soma.Block{{Type: soma.BlockText, Text: text}}}
}

func TestDecide(t *testing.T) {
	cc := func(session, epoch string) soma.Message { return reply("claude", "claude-code", session, epoch) }
	cases := []struct {
		name       string
		msgs       []soma.Message
		epoch      string
		wantResume string
		wantSend   []string
		wantHist   bool
	}{
		{"continue", []soma.Message{user("a"), cc("S1", ""), user("b")}, "", "S1", []string{"b"}, false},
		{"continue sends users after last in order", []soma.Message{user("a"), cc("S1", "e1"), toolResult("read", "x"), user("steer"), user("c")}, "e1", "S1", []string{"steer", "c"}, false},
		{"tool results ignored without a user message", []soma.Message{user("a"), cc("S1", ""), toolResult("read", "x")}, "", "S1", nil, false},
		{"epoch changed", []soma.Message{user("a"), cc("S1", ""), user("b")}, "e2", "", []string{"b"}, true},
		{"other provider reply follows", []soma.Message{user("a"), cc("S1", ""), user("b"), reply("openai", "openai-responses", "", ""), user("c")}, "", "", []string{"c"}, true},
		{"same api, other provider name", []soma.Message{user("a"), reply("other", "claude-code", "S1", ""), user("b")}, "", "", []string{"b"}, true},
		{"no opaque", []soma.Message{user("a"), cc("", ""), user("b")}, "", "", []string{"b"}, true},
		{"later reply without opaque is not another provider", []soma.Message{user("a"), cc("S1", ""), user("b"), cc("", ""), user("c")}, "", "S1", []string{"b", "c"}, false},
		{"first message", []soma.Message{user("a")}, "", "", []string{"a"}, false},
		{"ends in tool result", []soma.Message{user("a"), cc("S1", ""), toolResult("read", "x")}, "e2", "", nil, true},
	}
	for _, c := range cases {
		got := decide("claude", soma.Request{Messages: c.msgs, Epoch: c.epoch})
		var send []string
		for _, m := range got.Send {
			send = append(send, m.Text())
		}
		if got.Resume != c.wantResume || strings.Join(send, "|") != strings.Join(c.wantSend, "|") || (got.History != "") != c.wantHist {
			t.Errorf("%s: got resume %q send %v history %q, want resume %q send %v history %v", c.name, got.Resume, send, got.History, c.wantResume, c.wantSend, c.wantHist)
		}
	}
}

func TestDecideHistoryExcludesFinalUser(t *testing.T) {
	got := decide("claude", soma.Request{Messages: []soma.Message{user("first"), reply("openai", "openai-responses", "", ""), user("second")}})
	if !strings.Contains(got.History, "first") || strings.Contains(got.History, "second") {
		t.Errorf("history = %q", got.History)
	}
}

func TestHistoryText(t *testing.T) {
	if got := historyText(nil); got != "" {
		t.Errorf("empty history = %q", got)
	}
	call := soma.Message{Role: soma.RoleAssistant, Blocks: []soma.Block{
		{Type: soma.BlockThinking, Text: "hmm"},
		{Type: soma.BlockText, Text: "reading"},
		{Type: soma.BlockToolCall, CallID: "c1", Name: "read", Args: json.RawMessage(`{"path":"a.go"}`)},
	}}
	img := soma.Message{Role: soma.RoleUser, Blocks: []soma.Block{{Type: soma.BlockText, Text: "look"}, {Type: soma.BlockImage, Blob: "b1", MIME: "image/png"}}}
	got := historyText([]soma.Message{img, call, toolResult("read", "package a")})
	want := "<conversation_history>\n" +
		"This is the earlier conversation, replayed as text because it continues in a new session.\n" +
		"\nUser:\nlook\n[image]\n" +
		"\nAssistant:\nreading\n[called read {\"path\":\"a.go\"}]\n" +
		"\nTool result (read):\npackage a\n" +
		"</conversation_history>"
	if got != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
}

func TestHistoryTextClipsLongToolResult(t *testing.T) {
	body := strings.Repeat("a", 4000) + strings.Repeat("m", 100) + strings.Repeat("z", 4000)
	got := historyText([]soma.Message{toolResult("read", body)})
	want := "Tool result (read):\n" + strings.Repeat("a", 4000) + "\n[… 100 bytes omitted …]\n" + strings.Repeat("z", 4000) + "\n"
	if !strings.Contains(got, want) {
		t.Errorf("clipped result not found in %d bytes of history", len(got))
	}
	exact := strings.Repeat("a", 8000)
	if got := historyText([]soma.Message{toolResult("read", exact)}); strings.Contains(got, "omitted") {
		t.Error("8000 bytes must not be clipped")
	}
	runes := strings.Repeat("é", 5000)
	got = historyText([]soma.Message{toolResult("read", runes)})
	if !strings.Contains(got, "omitted …]") || strings.ContainsRune(got, '�') {
		t.Error("clip must cut at rune boundaries")
	}
}

func TestUserLine(t *testing.T) {
	load := func(name string) ([]byte, error) {
		if name == "b1" {
			return []byte("png"), nil
		}
		return nil, errors.New("missing")
	}
	got, err := userLine(user("hello"), "", load)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"message":{"content":"hello","role":"user"},"type":"user"}`+"\n" {
		t.Errorf("text line = %s", got)
	}
	got, err = userLine(user("hello"), "HIST", load)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"message":{"content":"HIST\n\nhello","role":"user"},"type":"user"}`+"\n" {
		t.Errorf("prefixed line = %s", got)
	}
	img := soma.Message{Role: soma.RoleUser, Blocks: []soma.Block{{Type: soma.BlockText, Text: "look"}, {Type: soma.BlockImage, Blob: "b1", MIME: "image/png"}}}
	got, err = userLine(img, "HIST", load)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"message":{"content":[{"type":"text","text":"HIST"},{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"cG5n"}}],"role":"user"},"type":"user"}` + "\n"
	if string(got) != want {
		t.Errorf("image line = %s", got)
	}
	img.Blocks[1].Blob = "nope"
	if _, err := userLine(img, "", load); err == nil {
		t.Error("missing blob: want an error")
	}
}
