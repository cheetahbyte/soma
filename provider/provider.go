// Package provider maps a provider's api to the protocol package that
// serves it. Protocol packages register themselves from init().
package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/cheetahbyte/soma"
	"github.com/cheetahbyte/soma/config"
)

type Factory func(cfg *config.Config, p config.ProviderConfig) (soma.Provider, error)
type Lister func(ctx context.Context, cfg *config.Config, p config.ProviderConfig) ([]config.ModelInfo, error)

var ErrNoLister = errors.New("provider has no model list")

var (
	factories = map[string]Factory{}
	listers   = map[string]Lister{}
)

func Register(api string, f Factory) {
	if _, dup := factories[api]; dup {
		panic("provider: api " + api + " registered twice")
	}
	factories[api] = f
}

func RegisterLister(api string, l Lister) {
	if _, dup := listers[api]; dup {
		panic("provider: model list for api " + api + " registered twice")
	}
	listers[api] = l
}

func New(cfg *config.Config, p config.ProviderConfig) (soma.Provider, error) {
	f, ok := factories[p.API]
	if !ok {
		names := make([]string, 0, len(factories))
		for name := range factories {
			names = append(names, name)
		}
		slices.Sort(names)
		return nil, fmt.Errorf("provider %q: unsupported api %q (supported: %s)", p.Name, p.API, strings.Join(names, ", "))
	}
	return f(cfg, p)
}

func FetchModels(ctx context.Context, cfg *config.Config, p config.ProviderConfig) ([]config.ModelInfo, error) {
	l, ok := listers[p.API]
	if !ok {
		return nil, ErrNoLister
	}
	return l(ctx, cfg, p)
}

// ModelInfoFunc resolves a model of p through cfg, so listed models, the
// cached catalog, and overrides all apply.
func ModelInfoFunc(cfg *config.Config, p config.ProviderConfig) func(id string) config.ModelInfo {
	return func(id string) config.ModelInfo {
		_, m, _ := cfg.Model(p.Name + "/" + id)
		return m
	}
}
