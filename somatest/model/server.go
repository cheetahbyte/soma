// Package model serves scripted replies over the OpenAI Responses API
// event stream, for tests that run the real client against localhost.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

type Step struct {
	Text      string     // assistant text, streamed in fragments
	Thinking  string     // reasoning summary, streamed before the text
	ToolCalls []ToolCall // function calls in the reply
	Status    int        // when non-zero, respond with this HTTP status instead
	Body      string     // response body for a Status step; a generic error when empty
	Truncate  bool       // end the reply as incomplete because of the output limit
	Drop      bool       // close the connection after the first text fragment
	Hang      bool       // send the first text fragment, then keep the stream open until the client goes away
	Silent    bool       // send no response headers; keep the request open until the client goes away
	// InputTokens is usage.input_tokens of the reply; default 100.
	InputTokens int
	// MeasureInput sets InputTokens to a quarter of the request body's bytes,
	// as a provider counts what it was sent.
	MeasureInput bool
}

type ToolCall struct {
	Name   string
	Args   string // JSON
	CallID string // default call_<step>_<index>
	ItemID string // default fc_<step>_<index>
}

// Lane is a script for the requests it matches: those whose instructions
// contain Instructions, or whose first user message contains FirstUser (an
// empty field matches nothing). Lanes are tried in order; a request that
// matches none gets the main steps.
type Lane struct {
	Instructions string
	FirstUser    string
	Steps        []Step
}

type Server struct {
	URL string

	srv      *httptest.Server
	mu       sync.Mutex
	main     script
	lanes    []Lane
	laneRuns []script
	count    int
	requests []json.RawMessage
	paths    []string
	headers  []http.Header
	expand   func(args string) string

	models       string
	modelsStatus int
	modelReqs    []*http.Request
}

// DefaultModels is the body of GET /codex/models until SetModels or
// SetModelsStatus replaces it: the ChatGPT model list's shape, with the
// fields soma reads.
const DefaultModels = `{"models":[` +
	`{"slug":"gpt-6.1-sol","context_window":500000,"max_context_window":872000,"visibility":"list","priority":1,"default_reasoning_level":"low",` +
	`"supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}]},` +
	`{"slug":"gpt-6-luna","context_window":272000,"max_context_window":872000,"visibility":"list","priority":2,"default_reasoning_level":"medium",` +
	`"supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"}]}]}`

// script is the steps of the main script or of one lane and what it served.
type script struct {
	steps    []Step
	next     int
	requests []json.RawMessage
}

func Start(steps []Step) *Server { return StartLanes(steps) }

func StartLanes(main []Step, lanes ...Lane) *Server {
	s := &Server{main: script{steps: main}, lanes: lanes, laneRuns: make([]script, len(lanes)),
		models: DefaultModels, modelsStatus: http.StatusOK}
	for i, l := range lanes {
		s.laneRuns[i].steps = l.Steps
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL
	return s
}

// SetExpand sets a function applied to every ToolCall.Args when its step is
// served, so a test can name ids that exist only at run time. It is called
// without the server's lock held.
func (s *Server) SetExpand(f func(args string) string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expand = f
}

// MainRequests returns the bodies served from the main steps, in order.
func (s *Server) MainRequests() []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]json.RawMessage(nil), s.main.requests...)
}

// LaneRequests returns the bodies served from lane i, in order.
func (s *Server) LaneRequests(i int) []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.laneRuns) {
		return nil
	}
	return append([]json.RawMessage(nil), s.laneRuns[i].requests...)
}

// Requests returns every body in arrival order, whichever script served it.
func (s *Server) Requests() []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]json.RawMessage(nil), s.requests...)
}

// Paths returns the request paths, in order.
func (s *Server) Paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

// Headers returns the request headers, in order.
func (s *Server) Headers() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.headers...)
}

// SetModels sets the body of GET /codex/models, answered with status 200.
func (s *Server) SetModels(body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models, s.modelsStatus = body, http.StatusOK
}

// SetModelsStatus makes GET /codex/models answer with status and a generic
// error body.
func (s *Server) SetModelsStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models, s.modelsStatus = fmt.Sprintf(`{"error":{"message":"scripted status %d"}}`, status), status
}

// ModelRequests returns the GET /codex/models requests, without bodies, in
// order. They are not among Requests, Paths, or Headers and use no step.
func (s *Server) ModelRequests() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.modelReqs...)
}

func (s *Server) Close() {
	s.srv.CloseClientConnections()
	s.srv.Close()
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/codex/models") {
		s.serveModels(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	lane := s.route(body)
	s.mu.Lock()
	s.requests = append(s.requests, json.RawMessage(body))
	s.paths = append(s.paths, r.URL.Path)
	s.headers = append(s.headers, r.Header.Clone())
	n := s.count
	s.count++
	sc := &s.main
	if lane >= 0 {
		sc = &s.laneRuns[lane]
	}
	sc.requests = append(sc.requests, json.RawMessage(body))
	var step Step
	ok := sc.next < len(sc.steps)
	if ok {
		step = sc.steps[sc.next]
	}
	sc.next++
	expand := s.expand
	s.mu.Unlock()

	if !ok {
		msg := "no scripted step left"
		if lane >= 0 {
			msg = fmt.Sprintf("no scripted step left in lane %d", lane)
		}
		step = Step{Status: http.StatusInternalServerError, Body: `{"error":{"message":"` + msg + `","type":"server_error"}}`}
	}
	if expand != nil && len(step.ToolCalls) > 0 {
		calls := make([]ToolCall, len(step.ToolCalls))
		for i, c := range step.ToolCalls {
			c.Args = expand(c.Args)
			calls[i] = c
		}
		step.ToolCalls = calls
	}
	if !strings.HasSuffix(r.URL.Path, "/responses") || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if step.Silent {
		<-r.Context().Done()
		return
	}
	if step.Status != 0 {
		if step.Body == "" {
			step.Body = fmt.Sprintf(`{"error":{"message":"scripted status %d","type":"server_error"}}`, step.Status)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(step.Status)
		io.WriteString(w, step.Body)
		return
	}
	if step.MeasureInput {
		step.InputTokens = (len(body) + 3) / 4
	}
	(&stream{w: w, r: r, step: step, n: n}).run()
}

func (s *Server) serveModels(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.modelReqs = append(s.modelReqs, r.Clone(context.Background()))
	body, status := s.models, s.modelsStatus
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

// route returns the index of the first lane that matches body, or -1.
func (s *Server) route(body []byte) int {
	if len(s.lanes) == 0 {
		return -1
	}
	var req struct {
		Instructions string `json:"instructions"`
		Input        []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"input"`
	}
	json.Unmarshal(body, &req)
	first := ""
	for _, it := range req.Input {
		if it.Role == "user" {
			first = contentText(it.Content)
			break
		}
	}
	for i, l := range s.lanes {
		if l.Instructions != "" && strings.Contains(req.Instructions, l.Instructions) ||
			l.FirstUser != "" && strings.Contains(first, l.FirstUser) {
			return i
		}
	}
	return -1
}

// contentText is a message's content given as a string, or the
// concatenated text of its parts.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

type stream struct {
	w     http.ResponseWriter
	r     *http.Request
	step  Step
	n     int
	index int
}

type event struct {
	Type         string    `json:"type"`
	OutputIndex  int       `json:"output_index"`
	SummaryIndex int       `json:"summary_index"`
	ContentIndex int       `json:"content_index"`
	ItemID       string    `json:"item_id,omitempty"`
	Delta        string    `json:"delta,omitempty"`
	Arguments    string    `json:"arguments,omitempty"`
	Item         *item     `json:"item,omitempty"`
	Response     *response `json:"response,omitempty"`
}

type item struct {
	Type             string     `json:"type"`
	ID               string     `json:"id"`
	CallID           string     `json:"call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
	Arguments        string     `json:"arguments,omitempty"`
	Role             string     `json:"role,omitempty"`
	Status           string     `json:"status,omitempty"`
	Content          []content  `json:"content,omitempty"`
	Summary          *[]summary `json:"summary,omitempty"`
	EncryptedContent string     `json:"encrypted_content,omitempty"`
}

type content struct {
	Type        string     `json:"type"`
	Text        string     `json:"text"`
	Annotations []struct{} `json:"annotations"`
}

type summary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type response struct {
	ID                string      `json:"id"`
	Status            string      `json:"status"`
	Output            []item      `json:"output"`
	Usage             *usage      `json:"usage,omitempty"`
	IncompleteDetails *incomplete `json:"incomplete_details,omitempty"`
}

type incomplete struct {
	Reason string `json:"reason"`
}

type usage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (st *stream) send(e event) {
	data, _ := json.Marshal(e)
	fmt.Fprintf(st.w, "event: %s\ndata: %s\n\n", e.Type, data)
	st.w.(http.Flusher).Flush()
}

// interrupt applies Drop or Hang after the first text fragment and reports
// whether the reply stops there.
func (st *stream) interrupt() bool {
	switch {
	case st.step.Drop:
		panic(http.ErrAbortHandler)
	case st.step.Hang:
		<-st.r.Context().Done()
		return true
	}
	return false
}

func (st *stream) run() {
	st.w.Header().Set("Content-Type", "text/event-stream")
	st.w.WriteHeader(http.StatusOK)
	id := fmt.Sprintf("resp_%d", st.n)
	st.send(event{Type: "response.created", Response: &response{ID: id, Status: "in_progress", Output: []item{}}})

	output := []item{}
	if st.step.Thinking != "" {
		output = append(output, st.reasoning())
	}
	if st.step.Text != "" {
		it, stopped := st.message()
		if stopped {
			return
		}
		output = append(output, it)
	} else if (st.step.Drop || st.step.Hang) && st.interrupt() {
		return
	}
	for i, c := range st.step.ToolCalls {
		output = append(output, st.call(i, c))
	}

	input := st.step.InputTokens
	if input == 0 {
		input = 100
	}
	resp := &response{ID: id, Status: "completed", Output: output, Usage: &usage{InputTokens: input, OutputTokens: 10, TotalTokens: input + 10}}
	kind := "response.completed"
	if st.step.Truncate {
		kind = "response.incomplete"
		resp.Status = "incomplete"
		resp.IncompleteDetails = &incomplete{Reason: "max_output_tokens"}
	}
	st.send(event{Type: kind, Response: resp})
}

func (st *stream) reasoning() item {
	idx := st.index
	st.index++
	itemID := fmt.Sprintf("rs_%d", st.n)
	st.send(event{Type: "response.output_item.added", OutputIndex: idx, Item: &item{Type: "reasoning", ID: itemID, Summary: &[]summary{}}})
	st.send(event{Type: "response.reasoning_summary_part.added", OutputIndex: idx})
	for _, f := range Fragments(st.step.Thinking) {
		st.send(event{Type: "response.reasoning_summary_text.delta", OutputIndex: idx, ItemID: itemID, Delta: f})
	}
	st.send(event{Type: "response.reasoning_summary_part.done", OutputIndex: idx, ItemID: itemID})
	it := item{
		Type:             "reasoning",
		ID:               itemID,
		Summary:          &[]summary{{Type: "summary_text", Text: st.step.Thinking}},
		EncryptedContent: fmt.Sprintf("enc_%d", st.n),
	}
	st.send(event{Type: "response.output_item.done", OutputIndex: idx, Item: &it})
	return it
}

func (st *stream) message() (item, bool) {
	idx := st.index
	st.index++
	itemID := fmt.Sprintf("msg_%d", st.n)
	st.send(event{Type: "response.output_item.added", OutputIndex: idx, Item: &item{Type: "message", ID: itemID, Role: "assistant", Status: "in_progress"}})
	for i, f := range Fragments(st.step.Text) {
		st.send(event{Type: "response.output_text.delta", OutputIndex: idx, ItemID: itemID, Delta: f})
		if i == 0 && st.interrupt() {
			return item{}, true
		}
	}
	it := item{
		Type:    "message",
		ID:      itemID,
		Role:    "assistant",
		Status:  "completed",
		Content: []content{{Type: "output_text", Text: st.step.Text, Annotations: []struct{}{}}},
	}
	st.send(event{Type: "response.output_item.done", OutputIndex: idx, Item: &it})
	return it, false
}

func (st *stream) call(i int, c ToolCall) item {
	idx := st.index
	st.index++
	itemID := fmt.Sprintf("fc_%d_%d", st.n, i)
	if c.ItemID != "" {
		itemID = c.ItemID
	}
	callID := fmt.Sprintf("call_%d_%d", st.n, i)
	if c.CallID != "" {
		callID = c.CallID
	}
	st.send(event{Type: "response.output_item.added", OutputIndex: idx, Item: &item{Type: "function_call", ID: itemID, CallID: callID, Name: c.Name, Status: "in_progress"}})
	half := len(c.Args) / 2
	for _, part := range []string{c.Args[:half], c.Args[half:]} {
		if part != "" {
			st.send(event{Type: "response.function_call_arguments.delta", OutputIndex: idx, ItemID: itemID, Delta: part})
		}
	}
	st.send(event{Type: "response.function_call_arguments.done", OutputIndex: idx, ItemID: itemID, Arguments: c.Args})
	it := item{Type: "function_call", ID: itemID, CallID: callID, Name: c.Name, Arguments: c.Args, Status: "completed"}
	st.send(event{Type: "response.output_item.done", OutputIndex: idx, Item: &it})
	return it
}

// Fragments splits text the way the server streams it: one fragment per
// word, each keeping the whitespace that follows it.
func Fragments(text string) []string {
	var out []string
	start := 0
	for i := 1; i < len(text); i++ {
		if text[i-1] == ' ' && text[i] != ' ' {
			out = append(out, text[start:i])
			start = i
		}
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}
