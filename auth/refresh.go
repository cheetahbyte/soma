package auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/cheetahbyte/soma"
)

const (
	refreshMargin = 5 * time.Minute
	refreshLimit  = 30 * time.Second
)

// ErrNoLogin means auth.json holds no ChatGPT login.
var ErrNoLogin error = noLoginError{}

type noLoginError struct{}

func (noLoginError) Error() string {
	return fmt.Sprintf(`no ChatGPT login: run "%s login codex"`, soma.Program)
}

type CodexSource struct {
	root   string
	issuer string
}

func NewCodexSource(root, issuer string) *CodexSource {
	return &CodexSource{root: root, issuer: issuer}
}

func needsRefresh(c Codex, rejected string, now time.Time) bool {
	return now.Add(refreshMargin).UnixMilli() >= c.Expires || (rejected != "" && c.Access == rejected)
}

// Token returns a usable access token and account id. It refreshes when fewer
// than five minutes are left, or when the stored token equals rejected.
//
// The refresh runs under the auth.json lock and is checked again once the
// lock is held, so concurrent requests and processes refresh once. Once it
// has started it is not cancelled by ctx: the issuer may already have rotated
// the refresh token, and the new one must reach the file.
func (s *CodexSource) Token(ctx context.Context, rejected string) (access, accountID string, err error) {
	c, ok, err := LoadCodex(s.root)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", ErrNoLogin
	}
	if !needsRefresh(c, rejected, time.Now()) {
		return c.Access, c.AccountID, nil
	}
	unlock, err := lock(ctx, s.root)
	if err != nil {
		return "", "", err
	}
	defer unlock()
	c, ok, err = LoadCodex(s.root)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", ErrNoLogin
	}
	if !needsRefresh(c, rejected, time.Now()) {
		return c.Access, c.AccountID, nil
	}
	next, err := s.refresh(ctx, c)
	if err != nil {
		return "", "", err
	}
	if err := saveLocked(s.root, next); err != nil {
		return "", "", err
	}
	return next.Access, next.AccountID, nil
}

func (s *CodexSource) refresh(ctx context.Context, c Codex) (Codex, error) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshLimit)
	defer cancel()
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.Refresh},
		"client_id":     {clientID},
	}
	t, err := postToken(rctx, s.issuer, form, c.Refresh, c.Access)
	var ee *endpointError
	switch {
	case errors.As(err, &ee) && ee.Status >= 400 && ee.Status < 500:
		return Codex{}, fmt.Errorf(`ChatGPT login expired or was revoked (%s): run "%s login codex"`, ee.summary(), soma.Program)
	case errors.As(err, &ee):
		return Codex{}, fmt.Errorf("%w: refreshing the ChatGPT login failed (%s)", ErrUnreachable, ee.summary())
	case errors.Is(err, context.DeadlineExceeded):
		return Codex{}, fmt.Errorf("%w: refreshing the ChatGPT login took longer than %s", ErrUnreachable, refreshLimit)
	case err != nil:
		return Codex{}, fmt.Errorf("refreshing the ChatGPT login: %w", err)
	}
	next, err := credentials(t, c.Refresh, time.Now())
	if err != nil {
		return Codex{}, fmt.Errorf("refreshing the ChatGPT login: %w", err)
	}
	if c.AccountID != "" && next.AccountID != c.AccountID {
		return Codex{}, fmt.Errorf(`the refreshed ChatGPT token belongs to a different account than the stored login; run "%s login codex"`, soma.Program)
	}
	return next, nil
}
