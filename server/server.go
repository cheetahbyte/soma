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
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/cheetahbyte/soma"
	"github.com/cheetahbyte/soma/auth"
	"github.com/cheetahbyte/soma/config"
	"github.com/cheetahbyte/soma/provider"
	_ "github.com/cheetahbyte/soma/provider/chat"
	"github.com/cheetahbyte/soma/provider/claudecode"
	_ "github.com/cheetahbyte/soma/provider/openai"
	"github.com/cheetahbyte/soma/wire"
)

type Options struct {
	Root               string
	Token              string
	KeepAlive          time.Duration
	Grace              time.Duration
	BlobCacheBytes     int64
	ArtifactCacheBytes int64
	UsageLogBytes      int64
}

type Server struct {
	o         Options
	mux       *http.ServeMux
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	blobs     *blobStore
	artifacts *artifactStore
	usage     *usageLog

	loadMu sync.Mutex
	cfg    *config.Config
	stamp  stamp

	mu        sync.Mutex
	closed    bool
	providers map[string]soma.Provider
	runs      map[runKey]*run
	pending   map[runKey]bool
	streams   map[string]*live
	wg        sync.WaitGroup
}

// stamp identifies the versions of models.json and auth.json a configuration
// was loaded from; a missing file has the zero value.
type stamp [2]struct {
	exists bool
	mod    int64
	size   int64
}

func readStamp(root string) stamp {
	var st stamp
	for i, name := range []string{"models.json", "auth.json"} {
		if fi, err := os.Stat(filepath.Join(root, name)); err == nil {
			st[i].exists, st[i].mod, st[i].size = true, fi.ModTime().UnixNano(), fi.Size()
		}
	}
	return st
}

func load(root string) (*config.Config, error) {
	cfg, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	cfg.ClaudeDir = filepath.Join(root, "claude-work")
	return cfg, nil
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
	if o.UsageLogBytes <= 0 {
		o.UsageLogBytes = 16 << 20
	}
	st := readStamp(o.Root)
	cfg, err := load(o.Root)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"SOMA_TOKEN", "TS_AUTHKEY"} {
		if !slices.Contains(claudecode.StripEnv, name) {
			claudecode.StripEnv = append(claudecode.StripEnv, name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{o: o, cfg: cfg, stamp: st, mux: http.NewServeMux(), ctx: ctx, cancel: cancel, done: make(chan struct{}),
		blobs: newBlobStore(o.Root, o.BlobCacheBytes), artifacts: newArtifactStore(o.Root, o.ArtifactCacheBytes), usage: newUsageLog(o.Root, o.UsageLogBytes), providers: map[string]soma.Provider{}, runs: map[runKey]*run{},
		pending: map[runKey]bool{}, streams: map[string]*live{}}
	s.mux.HandleFunc("POST /v1/stream", s.stream)
	s.mux.HandleFunc("POST /v1/abort", s.abort)
	s.mux.HandleFunc("POST /v1/endrun", s.endRun)
	s.mux.HandleFunc("GET /v1/info", s.info)
	s.mux.HandleFunc("PUT /v1/blobs/{sha256}", s.putBlob)
	s.mux.HandleFunc("PUT /v1/artifacts/{task}/{name}", s.putArtifact)
	s.mux.HandleFunc("GET /v1/artifacts/{task}/{name}", s.getArtifact)
	s.mux.HandleFunc("DELETE /v1/artifacts/{task}", s.deleteArtifact)
	s.mux.HandleFunc("GET /v1/usage", s.getUsage)
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

// config reloads changed configuration and refreshes opted-in model data when
// due. Both are serialized so concurrent requests share one catalog fetch.
// A reload drops cached providers; a run keeps its provider until it ends.
func (s *Server) config() (*config.Config, error) {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	st := readStamp(s.o.Root)
	if st != s.stamp {
		cfg, err := load(s.o.Root)
		if err != nil {
			return nil, err
		}
		s.cfg, s.stamp = cfg, st
		s.mu.Lock()
		s.providers = map[string]soma.Provider{}
		s.mu.Unlock()
	}
	config.RefreshModelsDev(s.cfg)
	return s.cfg, nil
}

func (s *Server) upstream(name string) (soma.Provider, *config.Config, config.ProviderConfig, int, error) {
	cfg, err := s.config()
	if err != nil {
		return nil, nil, config.ProviderConfig{}, http.StatusInternalServerError, fmt.Errorf("soma: reading the configuration: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pc, ok := cfg.Providers[name]
	if !ok {
		return nil, cfg, pc, http.StatusNotFound, fmt.Errorf("soma: upstream %q is not defined", name)
	}
	if pc.API == "soma" {
		return nil, cfg, pc, http.StatusBadRequest, fmt.Errorf("soma: upstream %q may not be another soma entry", name)
	}
	p := s.providers[name]
	if p == nil {
		var err error
		p, err = provider.New(cfg, pc)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, auth.ErrNoLogin) {
				status = http.StatusUnauthorized
			}
			return nil, cfg, pc, status, err
		}
		s.providers[name] = p
	}
	return p, cfg, pc, 0, nil
}

func fromUpstream(status int, pc config.ProviderConfig) bool {
	return status != http.StatusNotFound && status != http.StatusInternalServerError && pc.API != "soma"
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
	p, _, pc, status, err := s.upstream(body.Upstream)
	if err != nil {
		writeError(w, status, fromUpstream(status, pc), err)
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
	tracked := false
	if h, ok := p.(soma.ConversationHolder); ok && h.HoldsConversation() && len(body.Request.Tools) > 0 {
		if body.Request.SessionID == "" {
			writeError(w, http.StatusBadRequest, false, errors.New("soma: sessionId is required for a conversation-holding upstream"))
			return
		}
		tracked = true
	}
	key := runKey{body.Upstream, body.Request.SessionID}
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
	l := &live{cancel: cancel}
	s.mu.Lock()
	s.streams[stream] = l
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
	if tracked {
		var restart bool
		p, restart = s.begin(key, l, p)
		if body.AfterFailedStream || restart {
			abandon(p, key.session)
		}
	}
	lines := make(chan wire.Line, 64)
	finished := make(chan struct{})
	quit := make(chan struct{})
	var streamErr error
	go func() {
		defer close(finished)
		send := func(line wire.Line) {
			select {
			case lines <- line:
			case <-quit:
			}
		}
		m, err := p.Stream(ctx, req, func(d soma.Delta) { send(wire.Line{Delta: &wire.Delta{Kind: string(d.Kind), Text: d.Text}}) })
		streamErr = err
		if hasTokens(m.Usage) {
			s.usage.record(usageEntry(body.Upstream, pc.API, req, m))
		}
		send(wire.Line{Done: true, Message: &m, Error: encodeError(err, true)})
	}()
	delivered := false
	defer func() {
		cancel()
		close(quit)
		<-finished
		if tracked {
			s.end(key, l, p, delivered, streamErr)
		}
	}()
	timer := time.NewTimer(s.o.KeepAlive)
	defer timer.Stop()
	for {
		select {
		case line := <-lines:
			if err := write(line); err != nil {
				return
			}
			if line.Done {
				delivered = true
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
	l := s.streams[body.Stream]
	if l != nil {
		l.aborted = true
	}
	s.mu.Unlock()
	if l == nil {
		writeError(w, http.StatusNotFound, false, fmt.Errorf("soma: stream %q is not in progress", body.Stream))
		return
	}
	l.cancel()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("upstream")
	p, cfg, pc, status, err := s.upstream(name)
	if err != nil {
		writeError(w, status, fromUpstream(status, pc), err)
		return
	}
	models := pc.Models
	if pc.NeedsCatalog() {
		cat, ok := config.LoadCatalog(cfg.Root, name)
		if !ok || cat.Stale(time.Now()) {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			models, err = provider.FetchModels(ctx, cfg, pc)
			cancel()
			cat = config.Catalog{Fetched: time.Now(), Models: models}
			if err != nil {
				cat.Error = err.Error()
			}
			if serr := config.SaveCatalog(cfg.Root, name, cat); err == nil {
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
		_, resolved, err := cfg.Model(name + "/" + m.ID)
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
	key := runKey{body.Upstream, body.SessionID}
	p := s.runProvider(key)
	if p == nil {
		var pc config.ProviderConfig
		var status int
		var err error
		if p, _, pc, status, err = s.upstream(body.Upstream); err != nil {
			writeError(w, status, fromUpstream(status, pc), err)
			return
		}
	}
	if ender, ok := p.(soma.RunEnder); ok {
		ender.EndRun(body.SessionID)
	}
	s.forget(key)
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
			s.endAll()
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
