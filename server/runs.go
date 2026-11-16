package server

import (
	"context"
	"time"

	"github.com/cheetahbyte/soma"
)

type runKey struct{ upstream, session string }

type live struct {
	cancel     context.CancelFunc
	aborted    bool
	superseded bool
}

// run is a session of a conversation-holding upstream that may hold a
// process: at most one stream in flight, or else the grace timer.
type run struct {
	busy  chan struct{}
	cur   *live
	timer *time.Timer
	gen   int
}

type abandoner interface{ Abandon(sessionID string) }

func abandon(p soma.Provider, session string) {
	if a, ok := p.(abandoner); ok {
		a.Abandon(session)
	} else if e, ok := p.(soma.RunEnder); ok {
		e.EndRun(session)
	}
}

// begin makes l the session's stream in flight. An earlier stream of the
// session still in flight is one the asking side gave up on, so it is ended
// as abandoned before l starts.
func (s *Server) begin(key runKey, l *live) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		r := s.runs[key]
		if r == nil {
			r = &run{}
			s.runs[key] = r
		}
		if r.busy == nil {
			if r.timer != nil {
				r.timer.Stop()
				r.timer = nil
			}
			r.busy, r.cur = make(chan struct{}), l
			return
		}
		busy := r.busy
		r.cur.superseded = true
		r.cur.cancel()
		s.mu.Unlock()
		<-busy
		s.mu.Lock()
	}
}

// end finishes the session's stream l. A stream whose final line did not
// reach the asking side without an abort is abandoned; otherwise the
// process it leaves is ended by the grace timer unless another stream comes.
func (s *Server) end(key runKey, l *live, p soma.Provider, delivered bool, err error) {
	s.mu.Lock()
	abandoned := l.superseded || (!delivered && !l.aborted)
	s.mu.Unlock()
	if abandoned {
		abandon(p, key.session)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[key]
	close(r.busy)
	r.busy, r.cur = nil, nil
	switch {
	case abandoned || (err != nil && !l.aborted):
		delete(s.runs, key)
	case !s.closed:
		r.gen++
		gen := r.gen
		r.timer = time.AfterFunc(s.o.Grace, func() { s.expire(key, gen) })
	}
}

func (s *Server) expire(key runKey, gen int) {
	s.mu.Lock()
	r := s.runs[key]
	if s.closed || r == nil || r.busy != nil || r.gen != gen {
		s.mu.Unlock()
		return
	}
	delete(s.runs, key)
	p := s.providers[key.upstream]
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	if e, ok := p.(soma.RunEnder); ok {
		e.EndRun(key.session)
	}
}

func (s *Server) forget(key runKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.runs[key]; r != nil && r.busy == nil {
		if r.timer != nil {
			r.timer.Stop()
		}
		delete(s.runs, key)
	}
}

func (s *Server) endAll() {
	s.mu.Lock()
	runs := s.runs
	s.runs = map[runKey]*run{}
	enders := map[runKey]soma.RunEnder{}
	for key, r := range runs {
		if r.timer != nil {
			r.timer.Stop()
		}
		if e, ok := s.providers[key.upstream].(soma.RunEnder); ok {
			enders[key] = e
		}
	}
	s.mu.Unlock()
	for key, e := range enders {
		e.EndRun(key.session)
	}
}
