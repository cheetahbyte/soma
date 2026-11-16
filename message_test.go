package soma

import "testing"

func TestCacheHitPercent(t *testing.T) {
	tests := []struct {
		name string
		u    Usage
		pct  int
		ok   bool
	}{
		{"no usage", Usage{Output: 5}, 0, false},
		{"no cache", Usage{Input: 100}, 0, true},
		{"mostly cached", Usage{Input: 100, CacheRead: 900}, 90, true},
		{"writes count as input", Usage{Input: 100, CacheRead: 100, CacheWrite: 200}, 25, true},
		{"rounds down", Usage{Input: 2, CacheRead: 1}, 33, true},
	}
	for _, tt := range tests {
		pct, ok := tt.u.CacheHitPercent()
		if pct != tt.pct || ok != tt.ok {
			t.Errorf("%s: got %d, %v; want %d, %v", tt.name, pct, ok, tt.pct, tt.ok)
		}
	}
}
