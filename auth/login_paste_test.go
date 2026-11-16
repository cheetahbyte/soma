package auth

import (
	"bufio"
	"context"
	"io"
	"net/url"
	"strings"
	"testing"

	authfake "github.com/cheetahbyte/soma/somatest/auth"
)

const pasteRedirect = "http://localhost:1455/auth/callback"

func pasteOptions(fake *authfake.Server, out io.Writer) LoginOptions {
	return LoginOptions{Issuer: fake.URL, Ports: []int{1455, 1456}, Out: out}
}

func TestPasteLogin(t *testing.T) {
	fake := authfake.Start("acct-1")
	defer fake.Close()
	outR, outW := io.Pipe()
	inR, inW := io.Pipe()
	type result struct {
		creds Codex
		err   error
	}
	done := make(chan result, 1)
	go func() {
		c, err := LoginCodexPaste(context.Background(), pasteOptions(fake, outW), inR, outW)
		outW.Close()
		done <- result{c, err}
	}()

	var addr *url.URL
	sc := bufio.NewScanner(outR)
	for addr == nil && sc.Scan() {
		if strings.HasPrefix(sc.Text(), fake.URL) {
			u, err := url.Parse(sc.Text())
			if err != nil {
				t.Fatal(err)
			}
			addr = u
		}
	}
	if addr == nil {
		t.Fatal("no sign-in address printed")
	}
	go io.Copy(io.Discard, outR)
	if got := addr.Query().Get("redirect_uri"); got != pasteRedirect {
		t.Fatalf("redirect_uri = %q", got)
	}
	state := addr.Query().Get("state")
	io.WriteString(inW, pasteRedirect+"?code="+fake.Code()+"&state="+state+"\n")

	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.creds.Refresh != "rt-1" || r.creds.AccountID != "acct-1" || r.creds.Access == "" {
		t.Fatalf("credentials = %+v", r.creds)
	}
	ex := fake.Exchanges()
	if len(ex) != 1 || ex[0].Get("redirect_uri") != pasteRedirect || ex[0].Get("code_verifier") == "" {
		t.Fatalf("exchanges = %v", ex)
	}
}

func TestPasteWrongState(t *testing.T) {
	fake := authfake.Start("acct-1")
	defer fake.Close()
	in := strings.NewReader(pasteRedirect + "?code=" + fake.Code() + "&state=wrong\n")
	_, err := LoginCodexPaste(context.Background(), pasteOptions(fake, io.Discard), in, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("err = %v", err)
	}
	if n := len(fake.Exchanges()); n != 0 {
		t.Fatalf("%d exchanges, want 0", n)
	}
}

func TestPasteNotAnAddress(t *testing.T) {
	fake := authfake.Start("acct-1")
	defer fake.Close()
	_, err := LoginCodexPaste(context.Background(), pasteOptions(fake, io.Discard), strings.NewReader("not an address\n"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "address the browser was sent to") {
		t.Fatalf("err = %v", err)
	}
	if n := len(fake.Exchanges()); n != 0 {
		t.Fatalf("%d exchanges, want 0", n)
	}
}
