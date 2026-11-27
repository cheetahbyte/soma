package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/cheetahbyte/soma/wire"
)

func TestArtifactsPutGetDeleteAndEvict(t *testing.T) {
	srv, err := New(Options{
		Root:               t.TempDir(),
		Token:              "secret",
		ArtifactCacheBytes: 300,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	do := func(method, path string, body []byte) *http.Response {
		t.Helper()
		var r io.Reader
		if body != nil {
			r = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, ts.URL+path, r)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set(wire.HeaderProtocol, strconv.Itoa(wire.Version))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	payload := []byte("hello bundle bytes")
	sum := sha256.Sum256(payload)
	wantHash := hex.EncodeToString(sum[:])

	resp := do(http.MethodPut, "/v1/artifacts/task-1/bundle", payload)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get(headerArtifactSHA256); got != wantHash {
		t.Fatalf("PUT sha256 = %q, want %q", got, wantHash)
	}

	resp = do(http.MethodGet, "/v1/artifacts/task-1/bundle", nil)
	gotBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(gotBody, payload) {
		t.Fatalf("GET = %d %q, want 200 %q", resp.StatusCode, gotBody, payload)
	}
	if got := resp.Header.Get(headerArtifactSHA256); got != wantHash {
		t.Fatalf("GET sha256 = %q, want %q", got, wantHash)
	}

	// Reject invalid path segments
	resp = do(http.MethodPut, "/v1/artifacts/task-1/bad.sha256", payload)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT .sha256 status = %d, want 400", resp.StatusCode)
	}

	// Delete task artifacts
	resp = do(http.MethodDelete, "/v1/artifacts/task-1", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", resp.StatusCode)
	}
	resp = do(http.MethodGet, "/v1/artifacts/task-1/bundle", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after DELETE status = %d, want 404", resp.StatusCode)
	}
}
