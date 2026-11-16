package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/cheetahbyte/soma/auth"
	"github.com/cheetahbyte/soma/config"
	"github.com/cheetahbyte/soma/provider"
	"github.com/cheetahbyte/soma/provider/chat"
)

const (
	loginUsage   = "usage: soma login codex | melious --stdin"
	meliousCheck = 20 * time.Second
	codexList    = 20 * time.Second
	maxKeyBytes  = 4096
)

func loginCommand(args []string) int {
	switch {
	case len(args) == 1 && args[0] == "codex":
		return codexLogin()
	case len(args) == 2 && args[0] == config.MeliousProvider && args[1] == "--stdin":
		return meliousLogin()
	}
	fmt.Fprintln(os.Stderr, loginUsage)
	return 2
}

func codexLogin() int {
	dir, err := root()
	if err != nil {
		fmt.Fprintln(os.Stderr, "soma:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	creds, err := auth.LoginCodexPaste(ctx, auth.LoginOptions{
		Issuer: auth.Issuer,
		Ports:  auth.LoginPorts,
		Out:    os.Stderr,
	}, os.Stdin, os.Stderr)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "soma: login cancelled")
			return 130
		}
		fmt.Fprintln(os.Stderr, "soma:", err)
		return 1
	}
	if err := auth.SaveCodex(dir, creds); err != nil {
		fmt.Fprintln(os.Stderr, "soma:", err)
		return 1
	}
	cacheCodexModels(ctx, dir)
	fmt.Println("Logged in to ChatGPT.")
	return 0
}

// cacheCodexModels caches the model list of each ChatGPT provider that takes
// it from ChatGPT. A failure is only reported: the login stands.
func cacheCodexModels(ctx context.Context, dir string) {
	cfg, err := config.Load(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "soma: warning: could not read the ChatGPT model list:", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, codexList)
	defer cancel()
	for _, name := range slices.Sorted(maps.Keys(cfg.Providers)) {
		p := cfg.Providers[name]
		if p.API != "codex" || !p.NeedsCatalog() {
			continue
		}
		models, err := provider.FetchModels(ctx, cfg, p)
		if err == nil {
			err = config.SaveCatalog(dir, name, config.Catalog{Fetched: time.Now(), Models: models})
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "soma: warning: could not cache the model list of provider %q: %v\n", name, err)
		}
	}
}

func meliousLogin() int {
	dir, err := root()
	if err != nil {
		fmt.Fprintln(os.Stderr, "soma:", err)
		return 1
	}
	line, err := bufio.NewReader(io.LimitReader(os.Stdin, maxKeyBytes)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, "soma:", err)
		return 1
	}
	key := strings.TrimSpace(line)
	if key == "" {
		fmt.Fprintln(os.Stderr, "soma: no API key given")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), meliousCheck)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	models, err := chat.FetchModels(ctx, config.MeliousConfig(), key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "soma: Melious did not accept the key:", err)
		return 1
	}
	if err := auth.SaveAPIKey(dir, config.MeliousProvider, key); err != nil {
		fmt.Fprintln(os.Stderr, "soma:", err)
		return 1
	}
	if err := config.SaveCatalog(dir, config.MeliousProvider, config.Catalog{Fetched: time.Now(), Models: models}); err != nil {
		fmt.Fprintln(os.Stderr, "soma: could not cache the model list:", err)
	}
	fmt.Printf("Logged in to Melious. %d chat models are available as melious/<model>.\n", len(models))
	return 0
}
