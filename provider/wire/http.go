// Package wire holds the HTTP, error, and replay helpers that every model
// protocol shares.
package wire

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/cheetahbyte/soma"
)

// UserAgent names the program in requests, in the form Pi uses:
// program/version (os; runtime; arch).
func UserAgent() string {
	return soma.Program + "/" + soma.Version + " (" + runtime.GOOS + "; " + runtime.Version() + "; " + runtime.GOARCH + ")"
}

// Pi waits five minutes for response headers and between stream reads
// (undici's headersTimeout and bodyTimeout); spec scenario 13 sets the same
// idle limit.
const (
	defaultHeaderTimeout = 5 * time.Minute
	defaultIdleTimeout   = 5 * time.Minute
)

var (
	ErrIncompleteStream = errors.New("stream ended before the reply was complete")
	ErrStreamIdle       = errors.New("stream idle")
)

// NewHTTPClient returns a client whose transport limits the wait for
// response headers, and the limit between stream reads that callers apply
// with IdleReader.
func NewHTTPClient() (client *http.Client, idle time.Duration) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = envDuration("SOMA_HEADER_TIMEOUT_MS", defaultHeaderTimeout)
	return &http.Client{Transport: transport}, envDuration("SOMA_IDLE_TIMEOUT_MS", defaultIdleTimeout)
}

func envDuration(name string, def time.Duration) time.Duration {
	if ms, err := strconv.Atoi(os.Getenv(name)); err == nil && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return def
}

// IdleReader pushes the idle deadline back whenever a read returns data.
type IdleReader struct {
	R     io.Reader
	Timer *time.Timer
	Idle  time.Duration
}

func (r *IdleReader) Read(p []byte) (int, error) {
	n, err := r.R.Read(p)
	if n > 0 {
		r.Timer.Reset(r.Idle)
	}
	return n, err
}

// ReadSSE calls handle with the data of each event until handle reports done.
// End of input before that is NetworkError(ErrIncompleteStream).
func ReadSSE(body io.Reader, handle func(data []byte) (done bool)) error {
	r := bufio.NewReaderSize(body, 64*1024)
	var data bytes.Buffer
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			switch {
			case len(line) == 0:
				if data.Len() > 0 {
					if handle(data.Bytes()) {
						return nil
					}
					data.Reset()
				}
			case bytes.HasPrefix(line, []byte("data:")):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.Write(bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && data.Len() > 0 && handle(data.Bytes()) {
				return nil
			}
			if errors.Is(err, io.EOF) {
				return NetworkError(ErrIncompleteStream)
			}
			return NetworkError(err)
		}
	}
}
