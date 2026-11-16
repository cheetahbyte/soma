package wire

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cheetahbyte/soma"
)

// Patterns from Pi's utils/retry.js and utils/overflow.js, limited to the
// wording OpenAI and OpenAI-compatible gateways use.
var (
	quotaPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)insufficient_quota|quota exceeded|exceeded your current quota|out of budget|billing`)
	})
	overflowPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)context_length_exceeded|exceeds the context window|maximum context length|context window exceeded|too many tokens|token limit exceeded`)
	})
	transientPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)overloaded|server_busy|server.?error|internal.?error|service.?(is.?)?(temporarily.?)?unavailable|rate.?limit|too many requests|try (your request )?again|retry your request|timed? out|upstream`)
	})
)

type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// StatusError maps a non-200 response and its body to a *soma.ProviderError.
func StatusError(resp *http.Response, body []byte) error {
	msg := strings.TrimSpace(string(body))
	var eb errorBody
	if json.Unmarshal(body, &eb) == nil && eb.Error.Message != "" {
		msg = eb.Error.Message
		if eb.Error.Type != "" {
			msg = eb.Error.Type + ": " + msg
		}
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	raw := string(body)
	e := &soma.ProviderError{Kind: soma.ErrOther, Status: resp.StatusCode, Message: msg}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		if quotaPattern().MatchString(raw) {
			e.Kind = soma.ErrQuota
		} else {
			e.Kind = soma.ErrRateLimit
			e.RetryAfter = RetryAfter(resp.Header)
		}
	case resp.StatusCode == http.StatusRequestEntityTooLarge || overflowPattern().MatchString(raw):
		e.Kind = soma.ErrContextOverflow
	case resp.StatusCode >= 500:
		e.Kind = soma.ErrServer
		e.RetryAfter = RetryAfter(resp.Header)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		e.Kind = soma.ErrAuth
	}
	return e
}

// ClassifyMessage maps an error reported inside the event stream.
func ClassifyMessage(msg string) error {
	kind := soma.ErrOther
	switch {
	case quotaPattern().MatchString(msg):
		kind = soma.ErrQuota
	case overflowPattern().MatchString(msg):
		kind = soma.ErrContextOverflow
	case transientPattern().MatchString(msg):
		kind = soma.ErrServer
	}
	return &soma.ProviderError{Kind: kind, Message: msg}
}

func RetryAfter(h http.Header) time.Duration {
	if v := h.Get("Retry-After-Ms"); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms > 0 {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil && s > 0 {
		return time.Duration(s * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// NetworkError marks err as a dropped or failed connection, which is retried.
func NetworkError(err error) error {
	return &soma.ProviderError{Kind: soma.ErrNetwork, Message: err.Error()}
}

// Redact removes each secret from err's text, because endpoints echo keys
// and tokens in their error messages.
func Redact(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	var pe *soma.ProviderError
	if errors.As(err, &pe) {
		msg := redact(pe.Message, secrets)
		if msg == pe.Message {
			return err
		}
		out := *pe
		out.Message = msg
		return &out
	}
	if msg := err.Error(); redact(msg, secrets) != msg {
		return errors.New(redact(msg, secrets))
	}
	return err
}

func redact(s string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}
