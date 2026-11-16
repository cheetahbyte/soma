package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cheetahbyte/soma"
)

const (
	callbackPath = "/auth/callback"
	loginLimit   = 5 * time.Minute
)

type LoginOptions struct {
	Issuer      string    // the caller passes auth.Issuer
	Ports       []int     // tried in order; the caller passes auth.LoginPorts
	OpenBrowser bool      // the caller passes auth.OpenBrowser
	Out         io.Writer // where the URL and progress are printed
}

type loginResult struct {
	creds Codex
	err   error
}

// LoginCodex runs the browser login with PKCE and returns the new
// credentials; the caller saves them. It waits for the callback, ctx, or a
// five-minute deadline that exists only while it runs.
func LoginCodex(ctx context.Context, o LoginOptions) (Codex, error) {
	verifier, err := RandomSecret()
	if err != nil {
		return Codex{}, err
	}
	state, err := RandomSecret()
	if err != nil {
		return Codex{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	ln, err := listen(o.Ports)
	if err != nil {
		return Codex{}, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirect := fmt.Sprintf("http://localhost:%d%s", port, callbackPath)
	authorize := strings.TrimRight(o.Issuer, "/") + "/oauth/authorize?" + url.Values{
		"response_type":              {"code"},
		"client_id":                  {clientID},
		"redirect_uri":               {redirect},
		"scope":                      {loginScope},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"state":                      {state},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"originator":                 {"harness"},
	}.Encode()

	ctx, cancel := context.WithTimeout(ctx, loginLimit)
	defer cancel()
	results := make(chan loginResult, 1)
	cb := &callback{
		state: state,
		exchange: func(code string) (Codex, error) {
			return exchangeCode(ctx, o.Issuer, code, verifier, redirect)
		},
		done: results,
	}
	srv := &http.Server{Handler: cb, ReadHeaderTimeout: 30 * time.Second}
	go srv.Serve(ln)
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer scancel()
		if srv.Shutdown(sctx) != nil {
			srv.Close()
		}
	}()

	fmt.Fprintf(o.Out, "Open this URL to sign in to ChatGPT:\n%s\n", authorize)
	if o.OpenBrowser {
		if err := OpenURL(authorize); err != nil {
			fmt.Fprintln(o.Out, "Could not open a browser; open the URL above yourself.")
		}
	}
	fmt.Fprintln(o.Out, "Waiting for the sign-in to finish…")

	select {
	case r := <-results:
		return r.creds, r.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Codex{}, fmt.Errorf("the ChatGPT sign-in did not finish within %s", loginLimit)
		}
		return Codex{}, ctx.Err()
	}
}

// RandomSecret returns 32 random bytes, base64url encoded, for PKCE verifiers and state.
func RandomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// listen binds the loopback address on the first free port of ports.
func listen(ports []int) (net.Listener, error) {
	if len(ports) == 0 {
		return nil, errors.New("no port configured for the ChatGPT sign-in")
	}
	for _, p := range ports {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err == nil {
			return ln, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, fmt.Errorf("listening for the ChatGPT sign-in: %w", err)
		}
	}
	names := make([]string, len(ports))
	for i, p := range ports {
		names[i] = strconv.Itoa(p)
	}
	return nil, fmt.Errorf("the ChatGPT sign-in needs port %s on 127.0.0.1, and it is in use; close the program holding it (often another sign-in) and try again",
		strings.Join(names, " or "))
}

// OpenURL starts the system browser on target without waiting for it.
func OpenURL(target string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, target)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func exchangeCode(ctx context.Context, issuer, code, verifier, redirect string) (Codex, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirect},
	}
	t, err := postToken(ctx, issuer, form, code, verifier)
	var ee *endpointError
	if errors.As(err, &ee) {
		return Codex{}, fmt.Errorf("the ChatGPT sign-in server refused the sign-in (%s)", ee.summary())
	}
	if err != nil {
		return Codex{}, err
	}
	if t.RefreshToken == "" {
		return Codex{}, errors.New("the ChatGPT sign-in server sent no refresh token")
	}
	return credentials(t, "", time.Now())
}

// callback serves the redirect. A callback with the wrong state is refused
// and changes nothing; the first one with the right state claims the login,
// and every later one gets 409.
type callback struct {
	state    string
	exchange func(code string) (Codex, error)
	done     chan<- loginResult

	mu      sync.Mutex
	claimed bool
}

func (cb *callback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != callbackPath {
		Page(w, http.StatusNotFound, "Not found.")
		return
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(cb.state)) != 1 {
		Page(w, http.StatusBadRequest, "This sign-in link does not belong to the running login.")
		return
	}
	cb.mu.Lock()
	if cb.claimed {
		cb.mu.Unlock()
		Page(w, http.StatusConflict, "This sign-in has already been handled.")
		return
	}
	failure, code := q.Get("error"), q.Get("code")
	if failure == "" && code == "" {
		cb.mu.Unlock()
		Page(w, http.StatusBadRequest, "The sign-in link has no authorization code.")
		return
	}
	cb.claimed = true
	cb.mu.Unlock()

	if failure != "" {
		msg := clean(failure, nil)
		if d := clean(q.Get("error_description"), nil); d != "" {
			msg += ": " + d
		}
		Page(w, http.StatusBadRequest, "ChatGPT sign-in failed. Return to the terminal for details.")
		cb.done <- loginResult{err: fmt.Errorf("ChatGPT sign-in failed: %s", msg)}
		return
	}
	creds, err := cb.exchange(code)
	if err != nil {
		Page(w, http.StatusBadGateway, "ChatGPT sign-in failed. Return to the terminal for details.")
		cb.done <- loginResult{err: err}
		return
	}
	Page(w, http.StatusOK, "Signed in to ChatGPT. You can close this page.")
	cb.done <- loginResult{creds: creds}
}

// Page answers a browser with a one-line page.
func Page(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<!doctype html><meta charset=\"utf-8\"><title>%s</title><p>%s</p>\n", html.EscapeString(soma.Program), text)
}
