package main

import (
	"fmt"
	"io"
	"net"
	"path/filepath"

	"tailscale.com/tsnet"
)

func listenTailnet(root, hostname, authKey string) (net.Listener, io.Closer, error) {
	ts := &tsnet.Server{
		Dir:      filepath.Join(root, "tailscale"),
		Hostname: hostname,
		AuthKey:  authKey,
	}
	ln, err := ts.Listen("tcp", ":80")
	if err != nil {
		ts.Close()
		return nil, nil, fmt.Errorf("joining the tailnet: %w", err)
	}
	return ln, ts, nil
}
