package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Catalog is a provider's cached live model list.
type Catalog struct {
	Fetched time.Time   `json:"fetched"`
	Error   string      `json:"error,omitempty"` // set when the fetch failed; Models is then empty
	Models  []ModelInfo `json:"models"`
}

const (
	catalogMaxAge       = 24 * time.Hour
	failedCatalogMaxAge = 10 * time.Minute
)

// Stale reports whether the catalog should be fetched again.
func (c Catalog) Stale(now time.Time) bool {
	age := now.Sub(c.Fetched)
	if c.Error != "" {
		return age >= failedCatalogMaxAge
	}
	return age >= catalogMaxAge
}

func catalogPath(root, provider string) (string, error) {
	if provider == "" || strings.ContainsAny(provider, `/\`) || provider == "." || provider == ".." {
		return "", fmt.Errorf("provider name %q cannot name a cache file", provider)
	}
	return filepath.Join(root, "cache", "models", provider+".json"), nil
}

// LoadCatalog returns false when no cache file exists. A file that cannot be
// read or decoded is returned as a catalog holding the error, so it counts as
// a failed fetch.
func LoadCatalog(root, provider string) (Catalog, bool) {
	path, err := catalogPath(root, provider)
	if err != nil {
		return Catalog{}, false
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Catalog{}, false
	}
	if err != nil {
		return Catalog{Error: err.Error()}, true
	}
	var c Catalog
	if err := json.Unmarshal(data, &c); err != nil {
		return Catalog{Error: fmt.Sprintf("%s: %v", path, err)}, true
	}
	return c, true
}

// SaveCatalog writes the cache file through a temporary file and a rename,
// so a concurrent reader never sees a partial file.
func SaveCatalog(root, provider string, c Catalog) error {
	path, err := catalogPath(root, provider)
	if err != nil {
		return err
	}
	return writeJSON(path, c)
}

func writeJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".catalog-*.json")
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}
