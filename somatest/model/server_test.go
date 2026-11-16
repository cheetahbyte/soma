package model

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// post sends body to the server's responses route and returns the status,
// the raw body, and the decoded data of every server-sent event.
func post(t *testing.T, s *Server, body string) (int, string, []map[string]any) {
	t.Helper()
	resp, err := http.Post(s.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		payload, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			t.Fatalf("event %q: %v", payload, err)
		}
		events = append(events, e)
	}
	return resp.StatusCode, string(data), events
}

// completed returns the response object of the stream's final event.
func completed(t *testing.T, events []map[string]any) map[string]any {
	t.Helper()
	for _, e := range events {
		if e["type"] == "response.completed" || e["type"] == "response.incomplete" {
			r, _ := e["response"].(map[string]any)
			return r
		}
	}
	t.Fatalf("no final event in %v", events)
	return nil
}

// replyText returns the text of the reply's message items.
func replyText(t *testing.T, events []map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, it := range completed(t, events)["output"].([]any) {
		m := it.(map[string]any)
		if m["type"] != "message" {
			continue
		}
		for _, c := range m["content"].([]any) {
			b.WriteString(c.(map[string]any)["text"].(string))
		}
	}
	return b.String()
}

// replyCalls returns the function_call items of the reply.
func replyCalls(t *testing.T, events []map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, it := range completed(t, events)["output"].([]any) {
		if m := it.(map[string]any); m["type"] == "function_call" {
			out = append(out, m)
		}
	}
	return out
}

func body(instructions string, input ...string) string {
	data, _ := json.Marshal(instructions)
	return `{"model":"m","instructions":` + string(data) + `,"input":[` + strings.Join(input, ",") + `],"stream":true}`
}

func userString(text string) string {
	data, _ := json.Marshal(text)
	return `{"role":"user","content":` + string(data) + `}`
}

func userParts(texts ...string) string {
	var parts []string
	for _, t := range texts {
		data, _ := json.Marshal(t)
		parts = append(parts, `{"type":"input_text","text":`+string(data)+`}`)
	}
	return `{"role":"user","content":[` + strings.Join(parts, ",") + `]}`
}

func TestLanesRouteByInstructionsAndFirstUser(t *testing.T) {
	s := StartLanes(
		[]Step{{Text: "main one"}, {Text: "main two"}},
		Lane{Instructions: "SUM", Steps: []Step{{Text: "sum one"}}},
		Lane{FirstUser: "TASK-A", Steps: []Step{{Text: "a one"}, {Text: "a two"}}},
	)
	defer s.Close()

	if _, _, ev := post(t, s, body("You are a SUM assistant", userString("TASK-A too"))); replyText(t, ev) != "sum one" {
		t.Errorf("instructions lane: got %q, want sum one (lanes are tried in order)", replyText(t, ev))
	}
	if _, _, ev := post(t, s, body("parent", userString("please do TASK-A now"))); replyText(t, ev) != "a one" {
		t.Errorf("first user lane: got %q, want a one", replyText(t, ev))
	}
	if _, _, ev := post(t, s, body("parent", userString("other"))); replyText(t, ev) != "main one" {
		t.Errorf("main: got %q, want main one", replyText(t, ev))
	}
	// Only the first user message routes: TASK-A in a later one does not.
	later := body("parent", userString("hello"), `{"role":"assistant","content":[{"type":"output_text","text":"x"}]}`, userString("TASK-A"))
	if _, _, ev := post(t, s, later); replyText(t, ev) != "main two" {
		t.Errorf("later user message: got %q, want main two", replyText(t, ev))
	}
	// A developer or system item before the first user message is skipped.
	dev := body("parent", `{"role":"developer","content":"TASK-B"}`, userString("TASK-A again"))
	if _, _, ev := post(t, s, dev); replyText(t, ev) != "a two" {
		t.Errorf("second lane request: got %q, want a two", replyText(t, ev))
	}

	if n := len(s.LaneRequests(0)); n != 1 {
		t.Errorf("lane 0 served %d requests, want 1", n)
	}
	if n := len(s.LaneRequests(1)); n != 2 {
		t.Errorf("lane 1 served %d requests, want 2", n)
	}
	if n := len(s.MainRequests()); n != 2 {
		t.Errorf("main served %d requests, want 2", n)
	}
	if n := len(s.Requests()); n != 5 {
		t.Errorf("Requests has %d bodies, want 5", n)
	}
	if got := string(s.LaneRequests(1)[1]); got != dev {
		t.Errorf("lane 1 request 1 = %s, want %s", got, dev)
	}
	if got := string(s.Requests()[4]); got != dev {
		t.Errorf("Requests()[4] = %s, want the last body in arrival order", got)
	}
	if got := s.LaneRequests(7); got != nil {
		t.Errorf("LaneRequests of a missing lane = %v, want nil", got)
	}

	status, raw, _ := post(t, s, body("parent", userString("TASK-A third")))
	if status != http.StatusInternalServerError || !strings.Contains(raw, "no scripted step left in lane 1") {
		t.Errorf("exhausted lane: status %d, body %s", status, raw)
	}
	status, raw, _ = post(t, s, body("parent", userString("main third")))
	if status != http.StatusInternalServerError || !strings.Contains(raw, `"no scripted step left"`) {
		t.Errorf("exhausted main: status %d, body %s", status, raw)
	}
	if n := len(s.LaneRequests(1)); n != 3 {
		t.Errorf("an exhausted lane still records its request: %d, want 3", n)
	}
}

func TestEmptyLaneFieldsMatchNothing(t *testing.T) {
	s := StartLanes([]Step{{Text: "main"}}, Lane{Steps: []Step{{Text: "lane"}}})
	defer s.Close()
	if _, _, ev := post(t, s, body("anything", userString("anything"))); replyText(t, ev) != "main" {
		t.Errorf("got %q, want main: a lane without markers matches nothing", replyText(t, ev))
	}
	if n := len(s.LaneRequests(0)); n != 0 {
		t.Errorf("lane served %d requests, want 0", n)
	}
}

func TestStartIsStartLanesWithoutLanes(t *testing.T) {
	s := Start([]Step{{Text: "one"}})
	defer s.Close()
	if _, _, ev := post(t, s, body("SUM", userString("TASK-A"))); replyText(t, ev) != "one" {
		t.Errorf("got %q, want one", replyText(t, ev))
	}
	if len(s.MainRequests()) != 1 || len(s.Requests()) != 1 {
		t.Errorf("main %d, all %d; want 1 and 1", len(s.MainRequests()), len(s.Requests()))
	}
}

func TestFirstUserReadsStringsAndParts(t *testing.T) {
	s := StartLanes([]Step{{Text: "main"}}, Lane{FirstUser: "TASK-A", Steps: []Step{{Text: "string"}, {Text: "parts"}, {Text: "split"}}})
	defer s.Close()
	if _, _, ev := post(t, s, body("", userString("TASK-A"))); replyText(t, ev) != "string" {
		t.Errorf("string content: got %q", replyText(t, ev))
	}
	if _, _, ev := post(t, s, body("", userParts("TASK-A"))); replyText(t, ev) != "parts" {
		t.Errorf("parts content: got %q", replyText(t, ev))
	}
	if _, _, ev := post(t, s, body("", userParts("TAS", "K-A"))); replyText(t, ev) != "split" {
		t.Errorf("parts are concatenated: got %q", replyText(t, ev))
	}
	if _, _, ev := post(t, s, body("", userParts("TASK-B"))); replyText(t, ev) != "main" {
		t.Errorf("no match: got %q", replyText(t, ev))
	}
}

func TestSetExpandAppliesAtServeTime(t *testing.T) {
	steps := []Step{
		{ToolCalls: []ToolCall{{Name: "echo", Args: `{"id":"$X"}`}, {Name: "echo", Args: `{"id":"$X$X"}`}}},
		{ToolCalls: []ToolCall{{Name: "echo", Args: `{"id":"$X"}`}}},
	}
	s := StartLanes(steps)
	defer s.Close()
	var mu sync.Mutex
	value := "before"
	s.SetExpand(func(args string) string {
		mu.Lock()
		defer mu.Unlock()
		return strings.ReplaceAll(args, "$X", value)
	})
	mu.Lock()
	value = "first"
	mu.Unlock()

	_, raw, ev := post(t, s, body("", userString("go")))
	calls := replyCalls(t, ev)
	if len(calls) != 2 || calls[0]["arguments"] != `{"id":"first"}` || calls[1]["arguments"] != `{"id":"firstfirst"}` {
		t.Fatalf("calls = %v", calls)
	}
	if strings.Contains(raw, "$X") || !strings.Contains(raw, `"arguments":"{\"id\":\"first\"}"`) {
		t.Errorf("the streamed argument events still carry the unexpanded text: %s", raw)
	}

	mu.Lock()
	value = "second"
	mu.Unlock()
	_, _, ev = post(t, s, body("", userString("go")))
	if calls := replyCalls(t, ev); len(calls) != 1 || calls[0]["arguments"] != `{"id":"second"}` {
		t.Errorf("second request calls = %v, want the value at serve time", calls)
	}
	if steps[0].ToolCalls[0].Args != `{"id":"$X"}` {
		t.Errorf("the script was changed: %q", steps[0].ToolCalls[0].Args)
	}
}

func TestExpandMayReadTheServer(t *testing.T) {
	s := StartLanes([]Step{{ToolCalls: []ToolCall{{Name: "echo", Args: `{"n":"$N"}`}}}})
	defer s.Close()
	s.SetExpand(func(args string) string {
		return strings.ReplaceAll(args, "$N", string(rune('0'+len(s.Requests()))))
	})
	_, _, ev := post(t, s, body("", userString("go")))
	if calls := replyCalls(t, ev); len(calls) != 1 || calls[0]["arguments"] != `{"n":"1"}` {
		t.Errorf("calls = %v", calls)
	}
}

func TestCallIDsAreGlobalAcrossLanes(t *testing.T) {
	call := []ToolCall{{Name: "echo", Args: `{}`}, {Name: "echo", Args: `{}`}}
	s := StartLanes(
		[]Step{{ToolCalls: call}, {ToolCalls: call}},
		Lane{FirstUser: "TASK-A", Steps: []Step{{ToolCalls: call}, {ToolCalls: []ToolCall{{Name: "echo", Args: `{}`, CallID: "mine", ItemID: "fc_mine"}}}}},
	)
	defer s.Close()
	seen := map[string]bool{}
	var ids []string
	for _, b := range []string{
		body("", userString("main")),
		body("", userString("TASK-A")),
		body("", userString("main")),
		body("", userString("TASK-A")),
	} {
		_, _, ev := post(t, s, b)
		for _, c := range replyCalls(t, ev) {
			id := c["call_id"].(string)
			if seen[id] {
				t.Errorf("call id %s repeats", id)
			}
			seen[id] = true
			ids = append(ids, id+"/"+c["id"].(string))
		}
	}
	want := []string{"call_0_0/fc_0_0", "call_0_1/fc_0_1", "call_1_0/fc_1_0", "call_1_1/fc_1_1", "call_2_0/fc_2_0", "call_2_1/fc_2_1", "mine/fc_mine"}
	if strings.Join(ids, " ") != strings.Join(want, " ") {
		t.Errorf("ids = %v, want %v", ids, want)
	}
}

func TestInputTokensReported(t *testing.T) {
	s := StartLanes([]Step{{Text: "a", InputTokens: 4600}, {Text: "b"}})
	defer s.Close()
	for i, want := range []float64{4600, 100} {
		_, _, ev := post(t, s, body("", userString("go")))
		u := completed(t, ev)["usage"].(map[string]any)
		if u["input_tokens"] != want {
			t.Errorf("request %d: input_tokens = %v, want %v", i, u["input_tokens"], want)
		}
		if u["output_tokens"] != float64(10) || u["total_tokens"] != want+10 {
			t.Errorf("request %d: usage = %v", i, u)
		}
	}
}
