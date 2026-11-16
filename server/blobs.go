package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/cheetahbyte/soma/wire"
)

type blobStore struct {
	dir   string
	limit int64

	mu   sync.Mutex
	pins map[string]int
}

func newBlobStore(root string, limit int64) *blobStore {
	return &blobStore{dir: filepath.Join(root, "blobs"), limit: limit, pins: map[string]int{}}
}

func validHash(h string) bool {
	if len(h) != sha256.Size*2 {
		return false
	}
	for _, c := range h {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// acquire pins the request's blobs against eviction and marks them recently
// used, or reports the hashes the server lacks without pinning any.
func (b *blobStore) acquire(names map[string]string) (missing []string, release func(), err error) {
	var hashes []string
	for _, h := range names {
		if !validHash(h) {
			return nil, nil, fmt.Errorf("soma: invalid blob hash %q", h)
		}
		if !slices.Contains(hashes, h) {
			hashes = append(hashes, h)
		}
	}
	sort.Strings(hashes)
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	for _, h := range hashes {
		if err := os.Chtimes(filepath.Join(b.dir, h), now, now); err != nil {
			missing = append(missing, h)
		}
	}
	if len(missing) > 0 {
		return missing, nil, nil
	}
	for _, h := range hashes {
		b.pins[h]++
	}
	return nil, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for _, h := range hashes {
			if b.pins[h]--; b.pins[h] <= 0 {
				delete(b.pins, h)
			}
		}
	}, nil
}

func (b *blobStore) loader(names map[string]string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		h, ok := names[name]
		if !ok {
			return nil, fmt.Errorf("blob %q was not sent to the soma server", name)
		}
		return os.ReadFile(filepath.Join(b.dir, h))
	}
}

func (b *blobStore) store(hash string, r io.Reader) (int, error) {
	if !validHash(hash) {
		return http.StatusBadRequest, fmt.Errorf("soma: invalid blob hash %q", hash)
	}
	if err := os.MkdirAll(b.dir, 0700); err != nil {
		return http.StatusInternalServerError, err
	}
	tmp, err := os.CreateTemp(b.dir, ".upload-*")
	if err != nil {
		return http.StatusInternalServerError, err
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
			return http.StatusRequestEntityTooLarge, fmt.Errorf("soma: blob exceeds the cache size of %d bytes", b.limit)
		}
		return http.StatusBadRequest, fmt.Errorf("soma: reading blob: %w", err)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != hash {
		return http.StatusBadRequest, fmt.Errorf("soma: blob content hashes to %s, not %s", got, hash)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := os.Rename(tmp.Name(), filepath.Join(b.dir, hash)); err != nil {
		return http.StatusInternalServerError, err
	}
	b.evict(hash)
	return 0, nil
}

func (b *blobStore) evict(keep string) {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return
	}
	type blob struct {
		hash string
		size int64
		mod  time.Time
	}
	var blobs []blob
	var total int64
	for _, e := range entries {
		if !e.Type().IsRegular() || !validHash(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
		blobs = append(blobs, blob{e.Name(), info.Size(), info.ModTime()})
	}
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].mod.Before(blobs[j].mod) })
	for _, bl := range blobs {
		if total <= b.limit {
			break
		}
		if bl.hash == keep || b.pins[bl.hash] > 0 {
			continue
		}
		if err := os.Remove(filepath.Join(b.dir, bl.hash)); err == nil || errors.Is(err, os.ErrNotExist) {
			total -= bl.size
		}
	}
}

func (s *Server) putBlob(w http.ResponseWriter, r *http.Request) {
	status, err := s.blobs.store(r.PathValue("sha256"), http.MaxBytesReader(w, r.Body, s.o.BlobCacheBytes))
	if err != nil {
		writeError(w, status, false, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeMissing(w http.ResponseWriter, missing []string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	json.NewEncoder(w).Encode(wire.MissingBlobs{Missing: missing})
}
