package toolserver

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func post(t *testing.T, url, secret, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func newServer(t *testing.T, h CallHandler) *ToolServer {
	t.Helper()
	tools := []ServedTool{
		{Name: "read", Description: "Read.", Schema: json.RawMessage(`{"type":"object"}`), ReadOnly: true},
		{Name: "bash", Description: "Run.", Schema: json.RawMessage(`{"type":"object"}`)},
	}
	s, err := Serve(tools, "s3", h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestServeRejectsWrongSecret(t *testing.T) {
	s := newServer(t, nil)
	for _, secret := range []string{"", "wrong"} {
		resp := post(t, s.URL(), secret, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("secret %q: status %d", secret, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, s.URL(), nil)
	req.Header.Set("Authorization", "Bearer s3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status %d", resp.StatusCode)
	}
	resp = post(t, s.URL(), "s3", `{"jsonrpc":"2.0","id":2,"method":"server/discover"}`)
	defer resp.Body.Close()
	var m RPCMsg
	json.NewDecoder(resp.Body).Decode(&m)
	if m.Error == nil || m.Error.Code != -32601 {
		t.Fatalf("discover: %+v", m)
	}
}

func TestServeListsReadOnlyHint(t *testing.T) {
	s := newServer(t, nil)
	resp := post(t, s.URL(), "s3", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	defer resp.Body.Close()
	var m struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Annotations *struct {
					ReadOnlyHint bool `json:"readOnlyHint"`
				} `json:"annotations"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	tools := m.Result.Tools
	if len(tools) != 2 || tools[0].Name != "read" || tools[0].Annotations == nil || !tools[0].Annotations.ReadOnlyHint {
		t.Fatalf("tools: %+v", tools)
	}
	if tools[1].Name != "bash" || tools[1].Annotations != nil {
		t.Fatalf("bash has annotations: %+v", tools[1])
	}
}

func TestServeCallPlain(t *testing.T) {
	type got struct{ id, name, args string }
	ch := make(chan got, 1)
	s := newServer(t, func(_ context.Context, id, name string, args json.RawMessage) CallResult {
		ch <- got{id, name, string(args)}
		return CallResult{Content: []Content{{Type: "text", Text: "ok"}}}
	})
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"read","arguments":{"path":"a"},"_meta":{"claudecode/toolUseId":"toolu_1"}}}`
	resp := post(t, s.URL(), "s3", body)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %q", ct)
	}
	var m RPCMsg
	json.NewDecoder(resp.Body).Decode(&m)
	var res CallResult
	json.Unmarshal(m.Result, &res)
	if string(m.ID) != "7" || len(res.Content) != 1 || res.Content[0].Text != "ok" {
		t.Fatalf("response: %+v %s", m, m.Result)
	}
	if g := <-ch; g != (got{"toolu_1", "read", `{"path":"a"}`}) {
		t.Fatalf("handler got %+v", g)
	}
}

func TestServeCallStreamsProgress(t *testing.T) {
	old := progressInterval
	progressInterval = 10 * time.Millisecond
	t.Cleanup(func() { progressInterval = old })
	release := make(chan struct{})
	s := newServer(t, func(context.Context, string, string, json.RawMessage) CallResult {
		<-release
		return CallResult{Content: []Content{{Type: "text", Text: "late"}}}
	})
	body := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"bash","arguments":{},"_meta":{"progressToken":"tok"}}}`
	resp := post(t, s.URL(), "s3", body)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	var events []RPCMsg
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var m RPCMsg
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
			t.Fatal(err)
		}
		events = append(events, m)
		if len(events) == 2 {
			close(release)
		}
		if m.Method == "" {
			break
		}
	}
	if len(events) < 3 {
		t.Fatalf("events: %+v", events)
	}
	var p struct {
		ProgressToken string `json:"progressToken"`
	}
	json.Unmarshal(events[0].Params, &p)
	if events[0].Method != "notifications/progress" || p.ProgressToken != "tok" {
		t.Fatalf("first event: %+v", events[0])
	}
	last := events[len(events)-1]
	if string(last.ID) != "3" || !strings.Contains(string(last.Result), "late") {
		t.Fatalf("last event: %+v", last)
	}
}

func TestServeCloseCancelsHeldCall(t *testing.T) {
	started := make(chan struct{})
	ended := make(chan error, 1)
	s, err := Serve(nil, "s3", func(ctx context.Context, _, _ string, _ json.RawMessage) CallResult {
		close(started)
		<-ctx.Done()
		ended <- ctx.Err()
		return CallResult{}
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"bash"}}`
		req, _ := http.NewRequest(http.MethodPost, s.URL(), strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer s3")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	if err := <-ended; err == nil {
		t.Fatal("handler context was not cancelled")
	}
	if _, err := http.Get(s.URL()); err == nil {
		t.Fatal("listener still open")
	}
}

func TestWireResultOmitsEmptyFields(t *testing.T) {
	b, err := json.Marshal(wireResult(CallResult{Content: []Content{{Type: "text", Text: "hi"}, {Type: "image", Data: "QQ==", MIMEType: "image/png"}}}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"content":[{"text":"hi","type":"text"},{"data":"QQ==","mimeType":"image/png","type":"image"}]}`
	if string(b) != want {
		t.Fatalf("got %s\nwant %s", b, want)
	}
}
