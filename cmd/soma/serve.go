package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cheetahbyte/soma/server"
)

const httpShutdownLimit = 10 * time.Second

func serveCommand(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:7662", "address to serve on")
	hostname := fs.String("hostname", "soma", "host name on the tailnet")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: soma serve [--listen <addr>] [--hostname <name>]")
		return 2
	}
	token := os.Getenv("SOMA_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "soma: SOMA_TOKEN is not set")
		return 2
	}
	dir, err := root()
	if err != nil {
		fmt.Fprintln(os.Stderr, "soma:", err)
		return 1
	}
	s, err := server.New(server.Options{Root: dir, Token: token})
	if err != nil {
		fmt.Fprintln(os.Stderr, "soma:", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "soma:", err)
		return 1
	}
	hs := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 30 * time.Second}
	served := make(chan error, 2)
	go func() { served <- hs.Serve(ln) }()
	fmt.Fprintln(os.Stderr, "soma: listening on", ln.Addr())

	var tailnet io.Closer
	if key := os.Getenv("TS_AUTHKEY"); key != "" {
		tln, closer, err := listenTailnet(dir, *hostname, key)
		if err != nil {
			fmt.Fprintln(os.Stderr, "soma:", err)
			hs.Close()
			return 1
		}
		tailnet = closer
		go func() { served <- hs.Serve(tln) }()
		fmt.Fprintf(os.Stderr, "soma: listening on the tailnet as %s, port 80\n", *hostname)
	}

	code := 0
	select {
	case <-ctx.Done():
	case err := <-served:
		fmt.Fprintln(os.Stderr, "soma:", err)
		code = 1
	}
	stop()

	httpCtx, cancel := context.WithTimeout(context.Background(), httpShutdownLimit)
	defer cancel()
	httpDone := make(chan error, 1)
	go func() { httpDone <- hs.Shutdown(httpCtx) }()
	if err := s.Shutdown(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "soma:", err)
		code = 1
	}
	if err := <-httpDone; err != nil {
		hs.Close()
	}
	if tailnet != nil {
		tailnet.Close()
	}
	return code
}
