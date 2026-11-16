// Package auth stores credentials in <root>/auth.json and runs the ChatGPT
// browser login and token refresh. No token it handles is ever put into an
// error, a log line, or any other output.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

const CodexKey = "codex"

type Codex struct {
	Type      string `json:"type"` // "oauth"
	Access    string `json:"access"`
	Refresh   string `json:"refresh"`
	Expires   int64  `json:"expires"` // Unix milliseconds
	AccountID string `json:"accountId"`
}

// Production endpoints. Only cmd/axon/reg_e2e_codex.go (tag e2e) changes them,
// from AXON_CHATGPT_ISSUER, AXON_LOGIN_PORT (one port; 0 for any free one), and AXON_NO_BROWSER.
var (
	Issuer      = "https://auth.openai.com"
	LoginPorts  = []int{1455, 1457}
	OpenBrowser = true
)

func authPath(root string) string { return filepath.Join(root, "auth.json") }

// readEntries returns every entry of auth.json, undecoded, so that entries
// of other providers are written back exactly as they were.
func readEntries(root string) (map[string]json.RawMessage, error) {
	entries := map[string]json.RawMessage{}
	data, err := os.ReadFile(authPath(root))
	if errors.Is(err, fs.ErrNotExist) {
		return entries, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", authPath(root), err)
	}
	if entries == nil {
		entries = map[string]json.RawMessage{}
	}
	return entries, nil
}

// LoadCodex reads the ChatGPT entry. It takes no lock: auth.json is only
// ever replaced by a rename, so a reader sees one whole version.
func LoadCodex(root string) (Codex, bool, error) {
	entries, err := readEntries(root)
	if err != nil {
		return Codex{}, false, err
	}
	raw, ok := entries[CodexKey]
	if !ok {
		return Codex{}, false, nil
	}
	var c Codex
	if err := json.Unmarshal(raw, &c); err != nil {
		return Codex{}, false, fmt.Errorf("%s: the %q entry is not valid: %w", authPath(root), CodexKey, err)
	}
	if c.Type != "oauth" || c.Refresh == "" {
		return Codex{}, false, nil
	}
	return c, true, nil
}

// SaveCodex replaces the ChatGPT entry under the auth.json lock and keeps
// every other entry.
func SaveCodex(root string, c Codex) error {
	unlock, err := lock(context.Background(), root)
	if err != nil {
		return err
	}
	defer unlock()
	return saveLocked(root, c)
}

func saveLocked(root string, c Codex) error {
	c.Type = "oauth"
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return writeEntryLocked(root, CodexKey, raw)
}

type apiKey struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

// HasAPIKey reports whether auth.json holds an API key under name.
func HasAPIKey(root, name string) bool {
	entries, err := readEntries(root)
	if err != nil {
		return false
	}
	var k apiKey
	return json.Unmarshal(entries[name], &k) == nil && k.Type == "api_key" && k.Key != ""
}

// SaveAPIKey stores key under name in auth.json, the entry that a provider
// of that name reads, under the lock and keeping every other entry.
func SaveAPIKey(root, name, key string) error {
	unlock, err := lock(context.Background(), root)
	if err != nil {
		return err
	}
	defer unlock()
	raw, err := json.Marshal(apiKey{Type: "api_key", Key: key})
	if err != nil {
		return err
	}
	return writeEntryLocked(root, name, raw)
}

func writeEntryLocked(root, name string, raw json.RawMessage) error {
	entries, err := readEntries(root)
	if err != nil {
		return err
	}
	entries[name] = raw
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(root, ".auth-*.json")
	if err != nil {
		return err
	}
	err = f.Chmod(0o600)
	if err == nil {
		_, err = f.Write(append(data, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), authPath(root))
	}
	if err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("saving %s: %w", authPath(root), err)
	}
	return nil
}

// lock takes the exclusive lock on <root>/auth.lock, which every writer of
// auth.json holds, across processes. ctx cancels only the wait.
func lock(ctx context.Context, root string) (unlock func(), err error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(root, "auth.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	got := make(chan error, 1)
	go func() { got <- syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }()
	release := func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
	select {
	case err := <-got:
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("locking %s: %w", f.Name(), err)
		}
		return release, nil
	case <-ctx.Done():
		go func() {
			if <-got == nil {
				release()
			} else {
				f.Close()
			}
		}()
		return nil, ctx.Err()
	}
}
