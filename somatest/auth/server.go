// Package auth serves a scripted ChatGPT token endpoint on localhost,
// for tests of the login and the token refresh.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const clientID = "app_EMoamEEZ73f0CkXaXp7hrann"

// JWT returns an unsigned token in the issuer's shape. Each call returns a
// different token, even for the same account and expiry.
func JWT(accountID string, expires time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	var claims struct {
		Exp  int64  `json:"exp"`
		JTI  string `json:"jti"`
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	claims.Exp, claims.JTI, claims.Auth.AccountID = expires.Unix(), random(), accountID
	payload, _ := json.Marshal(claims)
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func random() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type Server struct {
	URL string

	srv     *httptest.Server
	account string
	code    string

	mu        sync.Mutex
	valid     map[string]bool // refresh tokens that are currently valid
	issued    int             // highest n of an issued rt-<n>
	refreshes int
	delay     time.Duration
	status    int
	exchanges []url.Values
	hold      chan struct{}
	holding   bool
	redirect  string
}

func Start(accountID string) *Server {
	s := &Server{account: accountID, code: "code-" + random(), valid: map[string]bool{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL
	return s
}

// Code returns the authorization code the server accepts.
func (s *Server) Code() string { return s.code }

// Seed makes refresh a valid refresh token.
func (s *Server) Seed(refresh string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.valid[refresh] = true
	if n, ok := tokenNumber(refresh); ok && n > s.issued {
		s.issued = n
	}
}

func (s *Server) SetRefreshDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = d
}

// SetRefreshStatus answers refresh grants with status and {"error":"invalid_grant"}.
func (s *Server) SetRefreshStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

// SetRedirect answers every token request with 307 to target, which keeps
// the method and body when followed.
func (s *Server) SetRedirect(target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.redirect = target
}

// Refreshes counts refresh grants answered with 200.
func (s *Server) Refreshes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshes
}

// Exchanges returns the authorization_code grants received.
func (s *Server) Exchanges() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.Values(nil), s.exchanges...)
}

// HoldExchange makes the next authorization_code grant wait until release is called,
// after Exchanging() reports that it has arrived.
func (s *Server) HoldExchange() (release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hold := make(chan struct{})
	s.hold = hold
	var once sync.Once
	return func() { once.Do(func() { close(hold) }) }
}

// Exchanging reports whether a held grant is waiting.
func (s *Server) Exchanging() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.holding
}

func (s *Server) Close() {
	s.srv.CloseClientConnections()
	s.srv.Close()
}

func tokenNumber(refresh string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(refresh, "rt-"))
	return n, err == nil && strings.HasPrefix(refresh, "rt-")
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	redirect := s.redirect
	s.mu.Unlock()
	if redirect != "" {
		http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
		return
	}
	body, _ := io.ReadAll(r.Body)
	form, err := url.ParseQuery(string(body))
	if err != nil || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if form.Get("client_id") != clientID {
		fail(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	switch form.Get("grant_type") {
	case "authorization_code":
		s.exchange(w, r, form)
	case "refresh_token":
		s.refresh(w, r, form)
	default:
		fail(w, http.StatusBadRequest, "unsupported_grant_type")
	}
}

func (s *Server) exchange(w http.ResponseWriter, r *http.Request, form url.Values) {
	s.mu.Lock()
	s.exchanges = append(s.exchanges, form)
	hold := s.hold
	s.hold = nil
	s.holding = hold != nil
	s.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
		}
		s.mu.Lock()
		s.holding = false
		s.mu.Unlock()
	}
	if form.Get("code") != s.code || form.Get("code_verifier") == "" || form.Get("redirect_uri") == "" {
		fail(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	s.mu.Lock()
	s.issued = max(s.issued, 1)
	refresh := "rt-" + strconv.Itoa(s.issued)
	s.valid[refresh] = true
	s.mu.Unlock()
	s.tokens(w, refresh)
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request, form url.Values) {
	s.mu.Lock()
	delay, status := s.delay, s.status
	s.mu.Unlock()
	if delay > 0 {
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-r.Context().Done():
			t.Stop()
			return
		}
	}
	if status != 0 {
		fail(w, status, "invalid_grant")
		return
	}
	s.mu.Lock()
	old := form.Get("refresh_token")
	if !s.valid[old] {
		s.mu.Unlock()
		fail(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	delete(s.valid, old)
	n, ok := tokenNumber(old)
	if !ok {
		n = s.issued
	}
	s.issued = max(s.issued, n+1)
	refresh := "rt-" + strconv.Itoa(n+1)
	s.valid[refresh] = true
	s.refreshes++
	s.mu.Unlock()
	s.tokens(w, refresh)
}

func (s *Server) tokens(w http.ResponseWriter, refresh string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}{JWT(s.account, time.Now().Add(time.Hour)), refresh, JWT(s.account, time.Now().Add(time.Hour)), "Bearer", 3600})
}

// fail never echoes anything from the request.
func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%q}`, code)
}
