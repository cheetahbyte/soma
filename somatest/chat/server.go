// Package chat serves scripted replies over the Chat Completions event
// stream, and a model list, for tests that run the real client against
// localhost.
package chat

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

type Step struct {
	Text           string
	Reasoning      string
	ReasoningField string     // delta field for Reasoning; default "reasoning_content"
	ToolCalls      []ToolCall // each call's arguments are sent in Fragments pieces; id and name only in the first
	Fragments      int        // default 3
	Interleave     bool       // send the fragments of all calls round-robin
	OmitUsage      bool
	UsageInChoice  bool   // usage inside choices[0] instead of a final usage chunk
	OmitFinish     bool   // end the stream without finish_reason, after [DONE]
	Finish         string // finish_reason; default "tool_calls" with calls, else "stop"
	Status         int
	Body           string
}

type ToolCall struct {
	Name string
	Args string // JSON
	ID   string // default call_<step>_<index>
}

type Server struct {
	URL string

	srv          *httptest.Server
	mu           sync.Mutex
	steps        []Step
	next         int
	requests     []json.RawMessage
	headers      []http.Header
	models       string
	modelsStatus int
	modelReqs    []http.Header
}

func Start(steps []Step) *Server {
	s := &Server{steps: steps}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL
	return s
}

// SetModels sets the body of GET /models, answered with status 200.
func (s *Server) SetModels(body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models, s.modelsStatus = body, http.StatusOK
}

// SetModelsStatus makes GET /models answer with status and a generic error body.
func (s *Server) SetModelsStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models, s.modelsStatus = fmt.Sprintf(`{"error":{"message":"scripted status %d"}}`, status), status
}

func (s *Server) ModelRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.modelReqs)
}

// ModelHeaders returns the headers of the GET /models requests.
func (s *Server) ModelHeaders() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.modelReqs...)
}

// Requests returns the bodies of POST /chat/completions.
func (s *Server) Requests() []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]json.RawMessage(nil), s.requests...)
}

// Headers returns the headers of the POST /chat/completions requests.
func (s *Server) Headers() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.headers...)
}

func (s *Server) Close() {
	s.srv.CloseClientConnections()
	s.srv.Close()
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models"):
		s.serveModels(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions"):
		s.serveChat(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) serveModels(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.modelReqs = append(s.modelReqs, r.Header.Clone())
	body, status := s.models, s.modelsStatus
	s.mu.Unlock()
	if status == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func (s *Server) serveChat(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, json.RawMessage(body))
	s.headers = append(s.headers, r.Header.Clone())
	n := s.next
	s.next++
	var step Step
	ok := n < len(s.steps)
	if ok {
		step = s.steps[n]
	}
	s.mu.Unlock()

	if !ok {
		step = Step{Status: http.StatusInternalServerError, Body: `{"error":{"message":"no scripted step left","type":"server_error"}}`}
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
	(&stream{w: w, step: step, n: n}).run()
}

type stream struct {
	w    http.ResponseWriter
	step Step
	n    int
}

type chunk struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
	Usage   *usage   `json:"usage,omitempty"`
}

type choice struct {
	Index        int     `json:"index"`
	Delta        delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
	Usage        *usage  `json:"usage,omitempty"`
}

type delta struct {
	Role             string          `json:"role,omitempty"`
	Content          *string         `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	Reasoning        string          `json:"reasoning,omitempty"`
	ReasoningText    string          `json:"reasoning_text,omitempty"`
	ToolCalls        []toolCallDelta `json:"tool_calls,omitempty"`
}

type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func reasoningDelta(field, text string) delta {
	switch field {
	case "reasoning":
		return delta{Reasoning: text}
	case "reasoning_text":
		return delta{ReasoningText: text}
	default:
		return delta{ReasoningContent: text}
	}
}

type usage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (st *stream) send(c chunk) {
	c.ID = fmt.Sprintf("chatcmpl-%d", st.n)
	c.Object = "chat.completion.chunk"
	c.Model = "m"
	if c.Choices == nil {
		c.Choices = []choice{}
	}
	data, _ := json.Marshal(c)
	fmt.Fprintf(st.w, "data: %s\n\n", data)
	st.w.(http.Flusher).Flush()
}

func (st *stream) delta(d delta) {
	st.send(chunk{Choices: []choice{{Delta: d}}})
}

func (st *stream) run() {
	st.w.Header().Set("Content-Type", "text/event-stream")
	st.w.WriteHeader(http.StatusOK)
	empty := ""
	st.delta(delta{Role: "assistant", Content: &empty})
	for _, f := range fragments(st.step.Reasoning) {
		st.delta(reasoningDelta(st.step.ReasoningField, f))
	}
	for _, f := range fragments(st.step.Text) {
		st.delta(delta{Content: &f})
	}
	st.calls()

	u := &usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}
	u.PromptTokensDetails.CachedTokens = 20
	if !st.step.OmitFinish {
		finish := st.step.Finish
		if finish == "" {
			finish = "stop"
			if len(st.step.ToolCalls) > 0 {
				finish = "tool_calls"
			}
		}
		c := choice{Delta: delta{}, FinishReason: &finish}
		if st.step.UsageInChoice && !st.step.OmitUsage {
			c.Usage = u
		}
		st.send(chunk{Choices: []choice{c}})
	}
	if !st.step.OmitUsage && !st.step.UsageInChoice {
		st.send(chunk{Usage: u})
	}
	fmt.Fprint(st.w, "data: [DONE]\n\n")
	st.w.(http.Flusher).Flush()
}

func (st *stream) calls() {
	n := st.step.Fragments
	if n <= 0 {
		n = 3
	}
	pieces := make([][]string, len(st.step.ToolCalls))
	for i, c := range st.step.ToolCalls {
		pieces[i] = split(c.Args, n)
	}
	piece := func(i, j int) delta {
		d := toolCallDelta{Index: i}
		d.Function.Arguments = pieces[i][j]
		if j == 0 {
			d.ID = st.step.ToolCalls[i].ID
			if d.ID == "" {
				d.ID = fmt.Sprintf("call_%d_%d", st.n, i)
			}
			d.Type = "function"
			d.Function.Name = st.step.ToolCalls[i].Name
		}
		return delta{ToolCalls: []toolCallDelta{d}}
	}
	if st.step.Interleave {
		for j := range n {
			for i := range pieces {
				st.delta(piece(i, j))
			}
		}
		return
	}
	for i := range pieces {
		for j := range n {
			st.delta(piece(i, j))
		}
	}
}

// split cuts s into n pieces of nearly equal length, by runes.
func split(s string, n int) []string {
	r := []rune(s)
	out := make([]string, n)
	for j := range n {
		out[j] = string(r[j*len(r)/n : (j+1)*len(r)/n])
	}
	return out
}

// fragments splits text into words, each keeping the whitespace after it.
func fragments(text string) []string {
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
