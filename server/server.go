package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/cheetahbyte/soma"
	"github.com/cheetahbyte/soma/auth"
	"github.com/cheetahbyte/soma/config"
	"github.com/cheetahbyte/soma/provider"
	_ "github.com/cheetahbyte/soma/provider/chat"
	_ "github.com/cheetahbyte/soma/provider/claudecode"
	_ "github.com/cheetahbyte/soma/provider/openai"
	"github.com/cheetahbyte/soma/wire"
)

type Options struct {
	Root           string
	Token          string
	KeepAlive      time.Duration
	Grace          time.Duration
	BlobCacheBytes int64
}

type Server struct {
	o      Options
	cfg    *config.Config
	mux    *http.ServeMux
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	blobs  *blobStore

	mu        sync.Mutex
	closed    bool
	providers map[string]soma.Provider
	sessions  map[string]map[string]bool
	streams   map[string]context.CancelFunc
	wg        sync.WaitGroup
}

func New(o Options) (*Server, error) {
	if o.Token == "" {
		return nil, errors.New("soma: token is not set")
	}
	if o.Root == "" {
		return nil, errors.New("soma: root is not set")
	}
	if o.KeepAlive <= 0 {
		o.KeepAlive = 30 * time.Second
	}
	if o.Grace <= 0 {
		o.Grace = 10 * time.Minute
	}
	if o.BlobCacheBytes <= 0 {
		o.BlobCacheBytes = 512 << 20
	}
	cfg, err := config.Load(o.Root)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{o: o, cfg: cfg, mux: http.NewServeMux(), ctx: ctx, cancel: cancel, done: make(chan struct{}),
		blobs: newBlobStore(o.Root, o.BlobCacheBytes), providers: map[string]soma.Provider{}, sessions: map[string]map[string]bool{},
		streams: map[string]context.CancelFunc{}}
	s.mux.HandleFunc("POST /v1/stream", s.stream)
	s.mux.HandleFunc("POST /v1/abort", s.abort)
	s.mux.HandleFunc("POST /v1/endrun", s.endRun)
	s.mux.HandleFunc("GET /v1/info", s.info)
	s.mux.HandleFunc("PUT /v1/blobs/{sha256}", s.putBlob)
	return s, nil
}

func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.handle) }

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(wire.HeaderProtocol, strconv.Itoa(wire.Version))
	w.Header().Set("Cache-Control", "no-store")
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.o.Token)) != 1 {
		writeError(w, http.StatusUnauthorized, false, errors.New("soma: invalid server token"))
		return
	}
	v, err := strconv.Atoi(r.Header.Get(wire.HeaderProtocol))
	if err != nil || v != wire.Version {
		older, other := "client", "server"
		if err == nil && v > wire.Version {
			older, other = "server", "client"
		}
		writeError(w, http.StatusUpgradeRequired, false, fmt.Errorf("soma: %s is older than %s (client protocol %q, server protocol %d); update the %s", older, other, r.Header.Get(wire.HeaderProtocol), wire.Version, older))
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, false, errors.New("soma: server is shutting down"))
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	s.mux.ServeHTTP(w, r.WithContext(ctx))
}

func (s *Server) upstream(name string) (soma.Provider, config.ProviderConfig, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pc, ok := s.cfg.Providers[name]
	if !ok {
		return nil, pc, http.StatusNotFound, fmt.Errorf("soma: upstream %q is not defined", name)
	}
	if pc.API == "soma" {
		return nil, pc, http.StatusBadRequest, fmt.Errorf("soma: upstream %q may not be another soma entry", name)
	}
	p := s.providers[name]
	if p == nil {
		var err error
		p, err = provider.New(s.cfg, pc)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, auth.ErrNoLogin) {
				status = http.StatusUnauthorized
			}
			return nil, pc, status, err
		}
		s.providers[name] = p
	}
	return p, pc, 0, nil
}

func decode[T wire.StreamRequest | wire.EndRun | wire.Abort](r *http.Request, v *T) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("expected one JSON object")
	}
	return nil
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	var body wire.StreamRequest
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, false, fmt.Errorf("soma: invalid stream request: %w", err))
		return
	}
	if body.Upstream == "" || body.Request.Model == "" {
		writeError(w, http.StatusBadRequest, false, errors.New("soma: upstream and model are required"))
		return
	}
	p, pc, status, err := s.upstream(body.Upstream)
	if err != nil {
		writeError(w, status, status != http.StatusNotFound && pc.API != "soma", err)
		return
	}
	missing, release, err := s.blobs.acquire(body.Blobs)
	if err != nil {
		writeError(w, http.StatusBadRequest, false, err)
		return
	}
	if len(missing) > 0 {
		writeMissing(w, missing)
		return
	}
	defer release()
	if h, ok := p.(soma.ConversationHolder); ok && h.HoldsConversation() && len(body.Request.Tools) > 0 {
		if body.Request.SessionID == "" {
			writeError(w, http.StatusBadRequest, false, errors.New("soma: sessionId is required for a conversation-holding upstream"))
			return
		}
		s.mu.Lock()
		if s.sessions[body.Upstream] == nil {
			s.sessions[body.Upstream] = map[string]bool{}
		}
		s.sessions[body.Upstream][body.Request.SessionID] = true
		s.mu.Unlock()
	}
	req := soma.Request{Model: body.Request.Model, System: body.Request.System, Messages: body.Request.Messages,
		Thinking: body.Request.Thinking, SessionID: body.Request.SessionID, Epoch: body.Request.Epoch,
		LoadBlob: s.blobs.loader(body.Blobs)}
	for _, tool := range body.Request.Tools {
		req.Tools = append(req.Tools, soma.ToolSpec{Name: tool.Name, Description: tool.Description, Schema: tool.Schema})
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		writeError(w, http.StatusInternalServerError, false, err)
		return
	}
	stream := hex.EncodeToString(id[:])
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	s.mu.Lock()
	s.streams[stream] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.streams, stream)
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	write := func(line wire.Line) error {
		if err := enc.Encode(line); err != nil {
			return err
		}
		return http.NewResponseController(w).Flush()
	}
	if err := write(wire.Line{Stream: stream}); err != nil {
		return
	}
	lines := make(chan wire.Line, 64)
	finished := make(chan struct{})
	quit := make(chan struct{})
	go func() {
		defer close(finished)
		send := func(line wire.Line) {
			select {
			case lines <- line:
			case <-quit:
			}
		}
		m, err := p.Stream(ctx, req, func(d soma.Delta) { send(wire.Line{Delta: &wire.Delta{Kind: string(d.Kind), Text: d.Text}}) })
		send(wire.Line{Done: true, Message: &m, Error: encodeError(err, true)})
	}()
	defer func() { cancel(); close(quit); <-finished }()
	timer := time.NewTimer(s.o.KeepAlive)
	defer timer.Stop()
	for {
		select {
		case line := <-lines:
			if err := write(line); err != nil || line.Done {
				return
			}
			timer.Reset(s.o.KeepAlive)
		case <-timer.C:
			if err := write(wire.Line{KeepAlive: true}); err != nil {
				return
			}
			timer.Reset(s.o.KeepAlive)
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) abort(w http.ResponseWriter, r *http.Request) {
	var body wire.Abort
	if err := decode(r, &body); err != nil || body.Stream == "" {
		writeError(w, http.StatusBadRequest, false, errors.New("soma: abort requires stream in one JSON object"))
		return
	}
	s.mu.Lock()
	cancel := s.streams[body.Stream]
	s.mu.Unlock()
	if cancel == nil {
		writeError(w, http.StatusNotFound, false, fmt.Errorf("soma: stream %q is not in progress", body.Stream))
		return
	}
	cancel()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("upstream")
	p, pc, status, err := s.upstream(name)
	if err != nil {
		writeError(w, status, status != http.StatusNotFound && pc.API != "soma", err)
		return
	}
	models := pc.Models
	if pc.NeedsCatalog() {
		cat, ok := config.LoadCatalog(s.cfg.Root, name)
		if !ok || cat.Stale(time.Now()) {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			models, err = provider.FetchModels(ctx, s.cfg, pc)
			cancel()
			cat = config.Catalog{Fetched: time.Now(), Models: models}
			if err != nil {
				cat.Error = err.Error()
			}
			if serr := config.SaveCatalog(s.cfg.Root, name, cat); err == nil {
				err = serr
			}
		} else {
			models = cat.Models
			if cat.Error != "" {
				err = errors.New(cat.Error)
			}
		}
		if err != nil {
			writeError(w, http.StatusBadGateway, true, err)
			return
		}
	}
	info := wire.Info{Models: make([]config.ModelInfo, 0, len(models))}
	if h, ok := p.(soma.ConversationHolder); ok {
		info.HoldsConversation = h.HoldsConversation()
	}
	for _, m := range models {
		_, resolved, err := s.cfg.Model(name + "/" + m.ID)
		if err != nil {
			writeError(w, http.StatusBadRequest, true, err)
			return
		}
		info.Models = append(info.Models, resolved)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

func (s *Server) endRun(w http.ResponseWriter, r *http.Request) {
	var body wire.EndRun
	if err := decode(r, &body); err != nil || body.Upstream == "" || body.SessionID == "" {
		writeError(w, http.StatusBadRequest, false, errors.New("soma: endrun requires upstream and sessionId in one JSON object"))
		return
	}
	p, pc, status, err := s.upstream(body.Upstream)
	if err != nil {
		writeError(w, status, status != http.StatusNotFound && pc.API != "soma", err)
		return
	}
	if ender, ok := p.(soma.RunEnder); ok {
		ender.EndRun(body.SessionID)
	}
	s.mu.Lock()
	delete(s.sessions[body.Upstream], body.SessionID)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func encodeError(err error, upstream bool) *wire.Error {
	if err == nil {
		return nil
	}
	e := &wire.Error{Provider: upstream, Kind: string(soma.ErrOther), Message: err.Error()}
	var pe *soma.ProviderError
	if errors.As(err, &pe) {
		e.Kind, e.Status, e.RetryAfterMS, e.Message = string(pe.Kind), pe.Status, pe.RetryAfter.Milliseconds(), pe.Message
	} else if errors.Is(err, auth.ErrNoLogin) {
		e.Kind = string(soma.ErrAuth)
	}
	return e
}

func writeError(w http.ResponseWriter, status int, upstream bool, err error) {
	e := encodeError(err, upstream)
	if !upstream {
		e.Status = status
		switch {
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			e.Kind = string(soma.ErrAuth)
		case status >= 500:
			e.Kind = string(soma.ErrServer)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(e)
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.cancel()
		go func() {
			s.wg.Wait()
			for name, sessions := range s.sessions {
				if ender, ok := s.providers[name].(soma.RunEnder); ok {
					for id := range sessions {
						ender.EndRun(id)
					}
				}
			}
			close(s.done)
		}()
	}
	s.mu.Unlock()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
