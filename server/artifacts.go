package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultArtifactCacheBytes = 2 << 30
	maxArtifactFileBytes      = 256 << 20
	headerArtifactSHA256      = "Soma-Artifact-SHA256"
)

type artifactStore struct {
	dir   string
	limit int64
	mu    sync.Mutex
}

func newArtifactStore(root string, limit int64) *artifactStore {
	if limit <= 0 {
		limit = defaultArtifactCacheBytes
	}
	return &artifactStore{dir: filepath.Join(root, "artifacts"), limit: limit}
}

func validArtifactSegment(s string) bool {
	if len(s) == 0 || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

func (a *artifactStore) store(task, name string, r io.Reader) (string, int, error) {
	if !validArtifactSegment(task) || !validArtifactSegment(name) || strings.HasSuffix(name, ".sha256") {
		return "", http.StatusBadRequest, fmt.Errorf("soma: invalid artifact path %q/%q", task, name)
	}
	taskDir := filepath.Join(a.dir, task)
	if err := os.MkdirAll(taskDir, 0o700); err != nil {
		return "", http.StatusInternalServerError, err
	}
	tmp, err := os.CreateTemp(taskDir, ".upload-*")
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	defer os.Remove(tmp.Name())
	sum := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, sum), r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return "", http.StatusRequestEntityTooLarge, fmt.Errorf("soma: artifact exceeds %d bytes", maxArtifactFileBytes)
		}
		return "", http.StatusBadRequest, fmt.Errorf("soma: reading artifact: %w", err)
	}
	digest := hex.EncodeToString(sum.Sum(nil))
	a.mu.Lock()
	defer a.mu.Unlock()
	dst := filepath.Join(taskDir, name)
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", http.StatusInternalServerError, err
	}
	_ = os.WriteFile(dst+".sha256", []byte(digest), 0o600)
	now := time.Now()
	_ = os.Chtimes(taskDir, now, now)
	a.evict(task)
	return digest, 0, nil
}

func (a *artifactStore) open(task, name string) (*os.File, string, int, error) {
	if !validArtifactSegment(task) || !validArtifactSegment(name) || strings.HasSuffix(name, ".sha256") {
		return nil, "", http.StatusBadRequest, fmt.Errorf("soma: invalid artifact path %q/%q", task, name)
	}
	path := filepath.Join(a.dir, task, name)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", http.StatusNotFound, fmt.Errorf("soma: artifact %s/%s not found", task, name)
		}
		return nil, "", http.StatusInternalServerError, err
	}
	digest := ""
	if b, err := os.ReadFile(path + ".sha256"); err == nil {
		digest = strings.TrimSpace(string(b))
	}
	return f, digest, 0, nil
}

func (a *artifactStore) remove(task string) (int, error) {
	if !validArtifactSegment(task) {
		return http.StatusBadRequest, fmt.Errorf("soma: invalid task id %q", task)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(a.dir, task)); err != nil {
		return http.StatusInternalServerError, err
	}
	return 0, nil
}

func (a *artifactStore) evict(keep string) {
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return
	}
	type item struct {
		name string
		size int64
		mod  time.Time
	}
	var items []item
	var total int64
	for _, e := range entries {
		if !e.IsDir() || !validArtifactSegment(e.Name()) {
			continue
		}
		dir := filepath.Join(a.dir, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		var size int64
		var mod time.Time
		for _, f := range files {
			if info, err := f.Info(); err == nil {
				size += info.Size()
				if info.ModTime().After(mod) {
					mod = info.ModTime()
				}
			}
		}
		total += size
		items = append(items, item{e.Name(), size, mod})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.Before(items[j].mod) })
	for _, it := range items {
		if total <= a.limit {
			break
		}
		if it.name == keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(a.dir, it.name)); err == nil {
			total -= it.size
		}
	}
}

func (s *Server) putArtifact(w http.ResponseWriter, r *http.Request) {
	maxBytes := min(int64(maxArtifactFileBytes), s.artifacts.limit)
	digest, status, err := s.artifacts.store(r.PathValue("task"), r.PathValue("name"), http.MaxBytesReader(w, r.Body, maxBytes))
	if err != nil {
		writeError(w, status, false, err)
		return
	}
	w.Header().Set(headerArtifactSHA256, digest)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getArtifact(w http.ResponseWriter, r *http.Request) {
	f, digest, status, err := s.artifacts.open(r.PathValue("task"), r.PathValue("name"))
	if err != nil {
		writeError(w, status, false, err)
		return
	}
	defer f.Close()
	if digest != "" {
		w.Header().Set(headerArtifactSHA256, digest)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, f)
}

func (s *Server) deleteArtifact(w http.ResponseWriter, r *http.Request) {
	if status, err := s.artifacts.remove(r.PathValue("task")); err != nil {
		writeError(w, status, false, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
