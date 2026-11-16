package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cheetahbyte/soma"
)

var version = "dev"

const usage = `usage: soma <command>
commands:
  serve [--listen <addr>] [--hostname <name>]
  login codex | melious --stdin
  version`

func main() {
	soma.Program = "soma"
	soma.Version = version
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return serveCommand(args[1:])
	case "login":
		return loginCommand(args[1:])
	case "version":
		fmt.Println("soma", version)
		return 0
	}
	fmt.Fprintf(os.Stderr, "soma: unknown command %q\n%s\n", args[0], usage)
	return 2
}

func root() (string, error) {
	if dir := os.Getenv("SOMA_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("SOMA_HOME is not set and the home directory is unknown")
	}
	return filepath.Join(home, ".soma"), nil
}
