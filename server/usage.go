package server

import (
	"bufio"
	"bytes"
	"crypto/rand"
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
	"github.com/cheetahbyte/soma/wire"
)

const usagePageBytes = 1 << 20

// usageLog is <root>/usage.jsonl: a first line {"id":...} naming the log,
// then one wire.UsageEntry per line. The id and the next seq are loaded on
// first use and kept in memory.
type usageLog struct {
	path  string
	limit int64

	mu   sync.Mutex
	id   string
	next int64
}

type usageHeader struct {
	ID string `json:"id"`
}

func newUsageLog(root string, limit int64) *usageLog {
	return &usageLog{path: filepath.Join(root, "usage.jsonl"), limit: limit}
}

func hasTokens(u *soma.Usage) bool {
	return u != nil && (u.Input != 0 || u.Output != 0 || u.CacheRead != 0 || u.CacheWrite != 0 || u.Reasoning != 0)
}

func entrySeq(line []byte) (int64, bool) {
	var e struct {
		Seq int64 `json:"seq"`
	}
	if json.Unmarshal(line, &e) != nil || e.Seq <= 0 {
		return 0, false
	}
	return e.Seq, true
}

func (u *usageLog) load() error {
	if u.id != "" {
		return nil
	}
	f, err := os.Open(u.path)
	if errors.Is(err, os.ErrNotExist) {
		return u.create()
	}
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	head, err := r.ReadBytes('\n')
	var h usageHeader
	if json.Unmarshal(head, &h) != nil || h.ID == "" {
		return fmt.Errorf("soma: %s does not start with a usage log id", u.path)
	}
	next := int64(1)
	for err == nil {
		var line []byte
		line, err = r.ReadBytes('\n')
		if seq, ok := entrySeq(line); ok {
			next = seq + 1
		}
	}
	if !errors.Is(err, io.EOF) {
		return err
	}
	u.id, u.next = h.ID, next
	return nil
}

func (u *usageLog) create() error {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	head, err := json.Marshal(usageHeader{ID: hex.EncodeToString(id[:])})
	if err != nil {
		return err
	}
	if err := u.replace(append(head, '\n')); err != nil {
		return err
	}
	u.id, u.next = hex.EncodeToString(id[:]), 1
	return nil
}

func (u *usageLog) replace(data []byte) error {
	dir := filepath.Dir(u.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".usage-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), u.path)
}

// open opens the log, creating a new one, with a new id, when it was deleted.
func (u *usageLog) open(flag int) (*os.File, error) {
	if err := u.load(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(u.path, flag, 0)
	if !errors.Is(err, os.ErrNotExist) {
		return f, err
	}
	u.id = ""
	if err := u.load(); err != nil {
		return nil, err
	}
	return os.OpenFile(u.path, flag, 0)
}

// record appends e under the next seq; a failure drops the entry.
func (u *usageLog) record(e wire.UsageEntry) {
	u.mu.Lock()
	defer u.mu.Unlock()
	f, err := u.open(os.O_WRONLY | os.O_APPEND)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	e.Seq = u.next
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Truncate(info.Size())
		return
	}
	u.next++
	if info.Size()+int64(len(line))+1 > u.limit {
		u.trim()
	}
}

// trim keeps the header and the newest lines that fit in half the limit, at
// least the newest line, so an entry is never dropped before it can be read.
func (u *usageLog) trim() {
	data, err := os.ReadFile(u.path)
	if err != nil {
		return
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1]
	}
	if len(lines) < 2 {
		return
	}
	start := len(lines) - 1
	size := int64(len(lines[0]) + len(lines[start]))
	for start > 1 && size+int64(len(lines[start-1])) <= u.limit/2 {
		start--
		size += int64(len(lines[start]))
	}
	u.replace(append(slices.Clone(lines[0]), bytes.Join(lines[start:], nil)...))
}

func (u *usageLog) read(after int64) (wire.Usage, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	f, err := u.open(os.O_RDONLY)
	if err != nil {
		return wire.Usage{}, err
	}
	defer f.Close()
	res := wire.Usage{ID: u.id, Next: after, Entries: []wire.UsageEntry{}}
	r := bufio.NewReader(f)
	size := 0
	for {
		line, err := r.ReadBytes('\n')
		if seq, ok := entrySeq(line); ok {
			if res.First == 0 {
				res.First = seq
			}
			if seq > after {
				if size > 0 && size+len(line) > usagePageBytes {
					break
				}
				var e wire.UsageEntry
				if json.Unmarshal(line, &e) == nil {
					res.Entries = append(res.Entries, e)
					res.Next = e.Seq
					size += len(line)
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return wire.Usage{}, err
		}
	}
	if newest := u.next - 1; after > newest {
		res.Next = newest
	}
	return res, nil
}

func (s *Server) getUsage(w http.ResponseWriter, r *http.Request) {
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, false, fmt.Errorf("soma: after must be a non-negative integer, not %q", v))
			return
		}
		after = n
	}
	res, err := s.usage.read(after)
	if err != nil {
		writeError(w, http.StatusInternalServerError, false, fmt.Errorf("soma: reading the usage log: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func usageEntry(upstream, api string, req soma.Request, m soma.Message) wire.UsageEntry {
	return wire.UsageEntry{Time: time.Now().UTC(), Upstream: upstream, API: api, Model: req.Model,
		SessionID: req.SessionID, Usage: *m.Usage, StopReason: m.StopReason}
}
