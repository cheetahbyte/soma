package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// From the Codex command-line client, as Pi and fx use it.
const (
	clientID   = "app_EMoamEEZ73f0CkXaXp7hrann"
	claimPath  = "https://api.openai.com/auth"
	loginScope = "openid profile email offline_access"
)

// ErrUnreachable marks a token request that failed before the endpoint
// answered, so callers may retry it.
var ErrUnreachable = errors.New("the ChatGPT sign-in server is unavailable")

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// endpointError is a non-200 answer from the token endpoint. It holds only
// the status and the standard error fields, never the body.
type endpointError struct {
	Status      int
	Code        string
	Description string
}

func (e *endpointError) Error() string {
	return "the ChatGPT sign-in server answered " + e.summary()
}

func (e *endpointError) summary() string {
	s := fmt.Sprintf("%d", e.Status)
	if e.Code != "" {
		s += " " + e.Code
	} else if t := http.StatusText(e.Status); t != "" {
		s += " " + t
	}
	if e.Description != "" {
		s += ": " + e.Description
	}
	return s
}

// tokenClient never follows a redirect: a 307 or 308 would resend the form,
// with its refresh token or authorization code, to wherever it points.
var tokenClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}}

// postToken sends a form to {issuer}/oauth/token. secrets are removed from
// any text of the answer that ends up in an error.
func postToken(ctx context.Context, issuer string, form url.Values, secrets ...string) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(issuer, "/")+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := tokenClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return tokenResponse{}, ctx.Err()
		}
		return tokenResponse{}, fmt.Errorf("%w: %s", ErrUnreachable, scrub(err, secrets))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		if ctx.Err() != nil {
			return tokenResponse{}, ctx.Err()
		}
		return tokenResponse{}, fmt.Errorf("%w: reading the answer: %s", ErrUnreachable, scrub(err, secrets))
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return tokenResponse{}, fmt.Errorf("the ChatGPT sign-in server answered %d with a redirect, which token requests do not follow", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		e := &endpointError{Status: resp.StatusCode}
		e.Code, e.Description = errorFields(body)
		e.Code, e.Description = clean(e.Code, secrets), clean(e.Description, secrets)
		return tokenResponse{}, e
	}
	var t tokenResponse
	if err := json.Unmarshal(body, &t); err != nil || t.AccessToken == "" {
		return tokenResponse{}, errors.New("the ChatGPT sign-in server sent an answer without an access token")
	}
	return t, nil
}

// errorFields reads OAuth's {"error": "...", "error_description": "..."}
// and OpenAI's {"error": {"code" or "type", "message"}}.
func errorFields(body []byte) (code, description string) {
	var b struct {
		Error            json.RawMessage `json:"error"`
		ErrorDescription string          `json:"error_description"`
	}
	if json.Unmarshal(body, &b) != nil {
		return "", ""
	}
	description = b.ErrorDescription
	if json.Unmarshal(b.Error, &code) == nil {
		return code, description
	}
	var obj struct {
		Code    string `json:"code"`
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b.Error, &obj) == nil {
		code = obj.Code
		if code == "" {
			code = obj.Type
		}
		if description == "" {
			description = obj.Message
		}
	}
	return code, description
}

// clean keeps a server-provided field printable and free of the secrets
// sent in the request, in case a server echoes them.
func clean(s string, secrets []string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	const max = 200
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return strings.TrimSpace(s)
}

func scrub(err error, secrets []string) string {
	if err == nil {
		return ""
	}
	return clean(err.Error(), secrets)
}

type claims struct {
	Exp  float64 `json:"exp"`
	Auth struct {
		AccountID string `json:"chatgpt_account_id"`
	} `json:"https://api.openai.com/auth"`
}

// tokenClaims decodes the payload of a JWT without verifying it; the token
// came straight from the issuer over TLS.
func tokenClaims(token string) (claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims{}, errors.New("the ChatGPT access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return claims{}, errors.New("the ChatGPT access token has an unreadable payload")
	}
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return claims{}, errors.New("the ChatGPT access token has an unreadable payload")
	}
	return c, nil
}

// credentials turns a token answer into a stored entry. previous supplies
// the refresh token when the answer has none.
func credentials(t tokenResponse, previousRefresh string, now time.Time) (Codex, error) {
	c, err := tokenClaims(t.AccessToken)
	if err != nil {
		return Codex{}, err
	}
	account := c.Auth.AccountID
	if !validAccountID(account) {
		return Codex{}, errors.New("the ChatGPT access token has no usable account id")
	}
	var expires int64
	switch {
	case t.ExpiresIn > 0:
		expires = now.Add(time.Duration(t.ExpiresIn) * time.Second).UnixMilli()
	case c.Exp > 0:
		expires = int64(c.Exp * 1000)
	default:
		return Codex{}, errors.New("the ChatGPT sign-in server sent no expiry for the access token")
	}
	refresh := t.RefreshToken
	if refresh == "" {
		refresh = previousRefresh
	}
	if refresh == "" {
		return Codex{}, errors.New("the ChatGPT sign-in server sent no refresh token")
	}
	return Codex{Type: "oauth", Access: t.AccessToken, Refresh: refresh, Expires: expires, AccountID: account}, nil
}

// validAccountID accepts ids that are safe to send as a header value.
func validAccountID(id string) bool {
	if id == "" || len(id) > 200 {
		return false
	}
	for _, r := range id {
		if r <= ' ' || r >= 0x7f {
			return false
		}
	}
	return true
}
