package claudecode

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cheetahbyte/soma"
	"github.com/cheetahbyte/soma/config"
	"github.com/cheetahbyte/soma/toolserver"
)

func result(text string) toolserver.CallResult {
	return toolserver.CallResult{Content: []toolserver.Content{{Type: "text", Text: text}}}
}

func TestMailboxResultBeforeAndAfterCall(t *testing.T) {
	m := newMailbox()
	m.announce("a", "read", json.RawMessage(`{"path":"x"}`))
	m.announce("b", "read", json.RawMessage(`{"path":"y"}`))
	m.deliver("a", result("A"))
	r, ok := m.wait(context.Background(), "a", "read", nil)
	if !ok || r.Content[0].Text != "A" {
		t.Fatalf("result before the call = %+v, %v", r, ok)
	}
	got := make(chan toolserver.CallResult, 1)
	go func() {
		r, _ := m.wait(context.Background(), "", "read", json.RawMessage(`{ "path": "y" }`))
		got <- r
	}()
	select {
	case <-got:
		t.Fatal("the call was answered before its result existed")
	case <-time.After(20 * time.Millisecond):
	}
	m.deliver("b", result("B"))
	if r := <-got; r.Content[0].Text != "B" {
		t.Errorf("call matched by name and arguments got %+v", r)
	}
	if n := m.outstanding(); n != 0 {
		t.Errorf("%d calls outstanding, want 0", n)
	}
}

func TestMailboxLostCall(t *testing.T) {
	m := newMailbox()
	m.announce("a", "bash", json.RawMessage(`{}`))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		_, ok := m.wait(ctx, "a", "bash", nil)
		done <- ok
	}()
	cancel()
	if ok := <-done; ok {
		t.Fatal("a cancelled wait reported a result")
	}
	if n := m.takeLost(); n != 1 {
		t.Errorf("lost = %d, want 1", n)
	}
	if n := m.takeLost(); n != 0 {
		t.Errorf("lost after taking = %d, want 0", n)
	}
	if n := m.outstanding(); n != 0 {
		t.Errorf("%d calls outstanding after the call was dropped, want 0", n)
	}
}

func TestEstimate(t *testing.T) {
	m := config.ModelInfo{InputPrice: 2, OutputPrice: 10, CachedPrice: 0.2}
	u := soma.Usage{Input: 1_000_000, CacheWrite: 2_000_000, CacheRead: 1_000_000, Output: 1_000_000}
	if got, want := estimate(u, 0, m), 2+5+0.2+10.0; got != want {
		t.Errorf("five-minute writes: estimate = %v, want %v", got, want)
	}
	if got, want := estimate(u, 1_000_000, m), 2+2.5+4+0.2+10.0; got != want {
		t.Errorf("half kept for an hour: estimate = %v, want %v", got, want)
	}
}
