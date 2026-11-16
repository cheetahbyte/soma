// Package claudecode runs the installed claude program in headless mode as
// the model of an axon session. Its tools are served by axon itself, so every
// tool call runs in axon while Claude Code holds the call open.
package claudecode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cheetahbyte/soma"
	"github.com/cheetahbyte/soma/config"
	"github.com/cheetahbyte/soma/provider"
	"github.com/cheetahbyte/soma/toolserver"
)

const (
	defaultCommand = "claude"
	lostNotice     = "A tool call was lost while the computer slept; it is not retried."
	settleWait     = 50 * time.Millisecond
)

func init() {
	provider.Register(apiName, func(cfg *config.Config, p config.ProviderConfig) (soma.Provider, error) {
		command := p.Command
		if command == "" {
			command = defaultCommand
		}
		if _, err := exec.LookPath(command); err != nil {
			return nil, fmt.Errorf("provider %q: the program %q is not on PATH; install Claude Code (https://code.claude.com/docs) and run claude auth login", p.Name, command)
		}
		pr := New(p.Name, command, p.AllowExtraUsage)
		pr.info = provider.ModelInfoFunc(cfg, p)
		return pr, nil
	})
}

type Provider struct {
	name       string
	command    string
	allowExtra bool
	info       func(id string) config.ModelInfo // nil: no prices, so no estimate before a turn ends

	mu    sync.Mutex
	procs map[string]*proc
}

func New(name, command string, allowExtraUsage bool) *Provider {
	return &Provider{name: name, command: command, allowExtra: allowExtraUsage, procs: map[string]*proc{}}
}

func (p *Provider) HoldsConversation() bool { return true }

// EndRun ends the process of the session and closes its tool server.
func (p *Provider) EndRun(sessionID string) {
	p.mu.Lock()
	pr := p.procs[sessionID]
	delete(p.procs, sessionID)
	p.mu.Unlock()
	if pr != nil {
		pr.stop()
	}
}

func (p *Provider) get(id string) *proc {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.procs[id]
}

func (p *Provider) set(id string, pr *proc) {
	p.mu.Lock()
	p.procs[id] = pr
	p.mu.Unlock()
}

func (p *Provider) drop(id string, pr *proc) {
	p.mu.Lock()
	if p.procs[id] == pr {
		delete(p.procs, id)
	}
	p.mu.Unlock()
	pr.stop()
}

var errResumeRejected = errors.New("claude rejected the conversation id")

func (p *Provider) Stream(ctx context.Context, req soma.Request, onDelta func(soma.Delta)) (soma.Message, error) {
	if len(req.Tools) == 0 {
		return p.oneShot(ctx, req, onDelta)
	}
	pr := p.get(req.SessionID)
	if pr != nil {
		pr.drain()
		if pr.overage && !p.allowExtra {
			p.drop(req.SessionID, pr)
			return soma.Message{}, &soma.ProviderError{Kind: soma.ErrQuota, Message: "Claude Code reports that requests are billed as extra usage beyond the plan; stopped before the next request. Set allowExtraUsage on the provider to continue."}
		}
	}
	pl := decide(p.name, req)
	if pr != nil && !(pr.alive() && pl.Resume != "" && pl.Resume == pr.sessionID && pr.sameSetup(req)) {
		p.drop(req.SessionID, pr)
		pr = nil
	}
	retried := false
	for {
		started := pr == nil
		if started {
			if pl.Resume != "" && len(pl.Send) == 0 {
				pl = newConversation(req.Messages)
			}
			if pl.Resume != "" {
				pl.History = lateResults(req.Messages)
			}
			var err error
			if pr, err = spawn(p.command, req, pl.Resume, false, p.info); err != nil {
				return soma.Message{}, &soma.ProviderError{Kind: soma.ErrOther, Message: err.Error()}
			}
			if pl.Resume != "" {
				pr.base = pl.Base
				pr.total, pr.allTokens = pl.Base.Total, pl.Base.Tokens
			}
			p.set(req.SessionID, pr)
		}
		err := pr.sendPlan(req, pl)
		if err == nil && !started {
			pr.deliverResults(req)
			for n := pr.box.takeLost(); n > 0; n-- {
				onDelta(soma.Delta{Kind: soma.DeltaNotice, Text: lostNotice})
			}
		}
		var reply soma.Message
		if err == nil {
			reply, err = readReply(ctx, pr, req, onDelta)
		}
		if errors.Is(err, errResumeRejected) && started && !retried {
			retried = true
			p.drop(req.SessionID, pr)
			pr, pl = nil, newConversation(req.Messages)
			continue
		}
		if err != nil && ctx.Err() == nil {
			p.drop(req.SessionID, pr)
			return soma.Message{}, asProviderError(err)
		}
		return reply, err
	}
}

func asProviderError(err error) error {
	var pe *soma.ProviderError
	if errors.As(err, &pe) {
		if pe.Kind == soma.ErrAuth && !strings.Contains(pe.Message, "claude auth login") {
			pe.Message += " (run claude auth login)"
		}
		return err
	}
	return &soma.ProviderError{Kind: soma.ErrOther, Message: err.Error()}
}

func (p *Provider) oneShot(ctx context.Context, req soma.Request, onDelta func(soma.Delta)) (soma.Message, error) {
	pr, err := spawn(p.command, req, "", true, p.info)
	if err != nil {
		return soma.Message{}, &soma.ProviderError{Kind: soma.ErrOther, Message: err.Error()}
	}
	defer pr.stop()
	if err := pr.sendPlan(req, newConversation(req.Messages)); err != nil {
		return soma.Message{}, asProviderError(err)
	}
	reply, err := readReply(ctx, pr, req, onDelta)
	if err != nil && ctx.Err() == nil {
		return soma.Message{}, asProviderError(err)
	}
	return reply, err
}

func (pr *proc) sameSetup(req soma.Request) bool {
	a, b := slices.Clone(pr.tools), toolNames(req.Tools)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b) && pr.model == req.Model && pr.effort == effort(req.Thinking)
}

// sendPlan writes the messages of pl to stdin. A history without a message
// to send is itself the first message.
func (pr *proc) sendPlan(req soma.Request, pl plan) error {
	msgs, prefix := pl.Send, pl.History
	if len(msgs) == 0 && prefix != "" {
		msgs = []soma.Message{{Role: soma.RoleUser, Blocks: []soma.Block{{Type: soma.BlockText, Text: prefix}}}}
		prefix = ""
	}
	for _, m := range msgs {
		line, err := userLine(m, prefix, req.LoadBlob)
		if err != nil {
			return err
		}
		prefix = ""
		if err := pr.write(line); err != nil {
			return pr.died()
		}
	}
	return nil
}

// deliverResults puts the results of the tool calls of the last reply into
// the mailbox of the held calls.
func (pr *proc) deliverResults(req soma.Request) {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role == soma.RoleAssistant {
			return
		}
		if m.Role == soma.RoleTool {
			pr.box.deliver(m.CallID, callResult(m, req.LoadBlob))
		}
	}
}

func callResult(m soma.Message, load func(string) ([]byte, error)) toolserver.CallResult {
	r := toolserver.CallResult{IsError: m.IsError, Content: []toolserver.Content{}}
	for _, b := range m.Blocks {
		switch b.Type {
		case soma.BlockText:
			r.Content = append(r.Content, toolserver.Content{Type: "text", Text: b.Text})
		case soma.BlockImage:
			data, err := load(b.Blob)
			if err != nil {
				r.Content = append(r.Content, toolserver.Content{Type: "text", Text: "[image unavailable]"})
				continue
			}
			r.Content = append(r.Content, toolserver.Content{Type: "image", Data: base64.StdEncoding.EncodeToString(data), MIMEType: b.MIME})
		}
	}
	return r
}

// handle answers a tools/call from the tool server with the result axon
// produced for it.
func (pr *proc) handle(ctx context.Context, toolUseID, name string, args json.RawMessage) toolserver.CallResult {
	r, ok := pr.box.wait(ctx, toolUseID, name, args)
	if !ok {
		return toolserver.CallResult{IsError: true, Content: []toolserver.Content{{Type: "text", Text: "cancelled"}}}
	}
	return r
}

// drain reads what the process wrote since the last reply ended, for its
// rate limit events.
func (pr *proc) drain() {
	for {
		select {
		case l, ok := <-pr.lines:
			if !ok {
				return
			}
			if ev, _ := parseLine(l); ev.kind == evRateLimit {
				pr.sawRate = true
				pr.overage = pr.overage || ev.overage
			}
		default:
			return
		}
	}
}

// settle waits briefly after a reply that ends in tool calls for the rate
// limit event that follows it, which Claude Code writes once per process; it
// is the only line that can come before the calls are answered.
func (pr *proc) settle() {
	if pr.sawRate {
		return
	}
	t := time.NewTimer(settleWait)
	defer t.Stop()
	for {
		select {
		case l, ok := <-pr.lines:
			if !ok {
				return
			}
			if ev, _ := parseLine(l); ev.kind == evRateLimit {
				pr.sawRate = true
				pr.overage = pr.overage || ev.overage
				return
			}
		case <-t.C:
			return
		}
	}
}

func (pr *proc) died() error {
	waitFor(pr.waited, time.Second)
	tail := strings.TrimSpace(pr.stderrTail())
	if pr.resumed && !pr.produced && strings.Contains(strings.ToLower(tail), "no conversation found") {
		return errResumeRejected
	}
	msg := "the claude process exited"
	if tail != "" {
		msg += "; its stderr ended with: " + tail
	}
	return &soma.ProviderError{Kind: soma.ErrOther, Message: msg}
}

type replyState struct {
	text    strings.Builder
	calls   []soma.Block
	usage   soma.Usage
	write1h int
	stop    string

	toolStop bool
}

func (s *replyState) message(pr *proc, req soma.Request, reason soma.StopReason) soma.Message {
	var blocks []soma.Block
	if s.text.Len() > 0 {
		blocks = append(blocks, soma.Block{Type: soma.BlockText, Text: s.text.String()})
	}
	blocks = append(blocks, s.calls...)
	if len(blocks) == 0 {
		blocks = []soma.Block{{Type: soma.BlockText}}
	}
	if !pr.oneShot && pr.sessionID != "" {
		blocks[0].Opaque, _ = json.Marshal(opaque{ClaudeSession: pr.sessionID, Epoch: req.Epoch, Total: pr.total, Tokens: pr.allTokens})
	}
	u := s.usage
	if reason != soma.StopEnd && reason != soma.StopLength {
		u.Cost = pr.estimate(u, s.write1h, req.Model)
		pr.estimated += u.Cost
	}
	return soma.Message{Role: soma.RoleAssistant, API: apiName, Blocks: blocks, Usage: &u, StopReason: reason}
}

// readReply reads lines until a model message ends: at once when it ends in
// tool calls, after the result line when it ends the turn.
func readReply(ctx context.Context, pr *proc, req soma.Request, onDelta func(soma.Delta)) (soma.Message, error) {
	var s replyState
	for {
		select {
		case <-ctx.Done():
			pr.interrupt()
			return s.message(pr, req, soma.StopAborted), ctx.Err()
		case line, ok := <-pr.lines:
			if !ok {
				return soma.Message{}, pr.died()
			}
			ev, err := parseLine(line)
			if err != nil {
				continue
			}
			switch ev.kind {
			case evInit:
				if ev.sessionID != "" {
					pr.sessionID = ev.sessionID
				}
				if src := ev.apiKeySource; src != "" && src != "none" {
					return soma.Message{}, &soma.ProviderError{Kind: soma.ErrAuth, Message: "Claude Code would use an API key (" + src + ") instead of the subscription login"}
				}
				if !pr.oneShot && ev.mcpStatus[serverName] != "connected" {
					status := ev.mcpStatus[serverName]
					if status == "" {
						status = "missing"
					}
					return soma.Message{}, &soma.ProviderError{Kind: soma.ErrOther, Message: "Claude Code could not connect to " + soma.Program + "'s tool server (status: " + status + ")"}
				}
			case evTextDelta:
				pr.produced = true
				s.text.WriteString(ev.text)
				onDelta(soma.Delta{Kind: soma.DeltaText, Text: ev.text})
			case evThinkingStart:
				onDelta(soma.Delta{Kind: soma.DeltaThinking})
			case evToolUse:
				pr.produced = true
				name := toolName(ev.name)
				pr.box.announce(ev.id, name, ev.args)
				s.calls = append(s.calls, soma.Block{Type: soma.BlockToolCall, CallID: ev.id, Name: name, Args: ev.args})
				onDelta(soma.Delta{Kind: soma.DeltaToolCall, Text: name})
			case evMessageEnd:
				pr.produced = true
				s.usage, s.write1h, s.stop = ev.usage, ev.write1h, ev.stopReason
				pr.tokens += ev.usage.Input + ev.usage.Output + ev.usage.CacheRead + ev.usage.CacheWrite
				s.toolStop = ev.stopReason == "tool_use"
			case evMessageStop:
				if s.toolStop {
					pr.settle()
					return s.message(pr, req, soma.StopToolCalls), nil
				}
			case evRetry:
				onDelta(soma.Delta{Kind: soma.DeltaNotice, Text: fmt.Sprintf("Claude Code is retrying (attempt %d of %d, in %s): %s", ev.attempt, ev.maxRetries, ev.delay, ev.errorKind)})
			case evRateLimit:
				pr.sawRate = true
				if ev.overage {
					pr.overage = true
				}
			case evResult:
				if ev.isError {
					if pr.resumed && !pr.produced && strings.Contains(strings.ToLower(ev.text), "no conversation found") {
						return soma.Message{}, errResumeRejected
					}
					text := ev.text
					if text == "" {
						text = "Claude Code reported an error"
					}
					return soma.Message{}, mapError(text)
				}
				s.usage.Cost = pr.turnCost(ev, s.usage, s.write1h, req.Model)
				reason := soma.StopEnd
				if s.stop == "max_tokens" {
					reason = soma.StopLength
				}
				return s.message(pr, req, reason), nil
			}
		}
	}
}

type held struct {
	id, name string
	args     json.RawMessage
	claimed  bool
}

// mailbox pairs the results axon produced with the calls Claude Code holds
// open, in either order of arrival.
type mailbox struct {
	mu      sync.Mutex
	changed chan struct{}
	calls   []*held
	results map[string]toolserver.CallResult
	done    map[string]bool
	lost    int
}

func newMailbox() *mailbox {
	return &mailbox{changed: make(chan struct{}), results: map[string]toolserver.CallResult{}, done: map[string]bool{}}
}

func (m *mailbox) notify() {
	close(m.changed)
	m.changed = make(chan struct{})
}

func (m *mailbox) announce(id, name string, args json.RawMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.done[id] {
		m.calls = append(m.calls, &held{id: id, name: name, args: args})
	}
	m.notify()
}

func (m *mailbox) deliver(id string, r toolserver.CallResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results[id] = r
	m.notify()
}

// outstanding counts calls announced in a reply whose result Claude Code has
// not received.
func (m *mailbox) outstanding() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// takeLost returns how many held calls Claude Code gave up on since the last
// call, and resets the count.
func (m *mailbox) takeLost() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.lost
	m.lost = 0
	return n
}

func sameArgs(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(x, y)
}

// claim returns the id of the first announced call with this name and
// arguments that no request has taken yet, or "". m.mu is held.
func (m *mailbox) claim(name string, args json.RawMessage) string {
	for _, c := range m.calls {
		if !c.claimed && c.name == name && sameArgs(c.args, args) {
			c.claimed = true
			return c.id
		}
	}
	return ""
}

func (m *mailbox) finish(id string) {
	m.done[id] = true
	m.calls = slices.DeleteFunc(m.calls, func(c *held) bool { return c.id == id })
}

// wait returns the result for the call with this tool-use id; without an id
// the call is matched to an announced one by name and arguments. ok is false
// when ctx ended first.
func (m *mailbox) wait(ctx context.Context, id, name string, args json.RawMessage) (toolserver.CallResult, bool) {
	for {
		m.mu.Lock()
		if id == "" {
			id = m.claim(name, args)
		}
		if id != "" {
			if r, ok := m.results[id]; ok {
				delete(m.results, id)
				m.finish(id)
				m.mu.Unlock()
				return r, true
			}
		}
		ch := m.changed
		m.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			if id != "" {
				m.mu.Lock()
				m.finish(id)
				m.lost++
				m.mu.Unlock()
			}
			return toolserver.CallResult{}, false
		}
	}
}

// turnCost returns the cost of the reply that ends a turn: the growth of
// Claude Code's running cost less the estimates already handed out.
func (pr *proc) turnCost(ev event, u soma.Usage, write1h int, model string) float64 {
	start, known := pr.cost, true
	if !pr.settled {
		pr.settled = true
		start, known = pr.startCost(ev)
	}
	cost := ev.totalCost - start
	switch {
	case !known:
		cost = pr.estimated + pr.estimate(u, write1h, model)
	case cost < 0:
		cost = ev.totalCost
	}
	pr.cost, pr.total, pr.allTokens = ev.totalCost, ev.totalCost, ev.allTokens
	cost = max(0, cost-pr.estimated)
	pr.estimated = 0
	return cost
}

// startCost is the part of the first result's running cost that this process
// did not spend. Claude Code may carry a resumed conversation's totals over;
// the token count tells whether it did. known is false when it did but the
// reply resumed from has no record of them, as in older session files.
func (pr *proc) startCost(ev event) (start float64, known bool) {
	carried := ev.allTokens - pr.tokens
	switch {
	case !pr.resumed || carried <= 0:
		return 0, true
	case pr.base.Tokens == 0:
		return 0, false
	case 2*carried < pr.base.Tokens:
		return 0, true
	}
	return pr.base.Total, true
}

func (pr *proc) estimate(u soma.Usage, write1h int, model string) float64 {
	if pr.price == nil {
		return 0
	}
	return estimate(u, write1h, pr.price(model))
}

// estimate prices u at list prices per million tokens. A cache write kept
// for five minutes costs a quarter more than plain input, one kept for an
// hour twice as much.
func estimate(u soma.Usage, write1h int, m config.ModelInfo) float64 {
	write1h = min(write1h, u.CacheWrite)
	write := float64(u.CacheWrite-write1h)*1.25 + float64(write1h)*2
	return (float64(u.Input)*m.InputPrice + write*m.InputPrice +
		float64(u.CacheRead)*m.CachedPrice + float64(u.Output)*m.OutputPrice) / 1e6
}
