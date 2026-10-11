package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"claude-go/internal/api"
	"claude-go/internal/auth"
)

// TestBorrowedCredsReBorrowOn401: borrowed creds must never refresh —
// but when the API 401s the borrowed token, the client re-reads the
// shared store (real may have rotated it) and retries with the new one.
func TestBorrowedCredsReBorrowOn401(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_GO_RAW_HTTP", "0")
	// own creds file absent → Load() falls through to the shared store
	t.Setenv("CLAUDE_GO_CREDS_FILE", filepath.Join(home, "own.json"))
	claudeDir := filepath.Join(home, ".claude")
	os.MkdirAll(claudeDir, 0o755)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), []byte(
		`{"claudeAiOauth":{"accessToken":"tok-FRESH","refreshToken":"r2",`+
			`"expiresAt":9999999999999,"scopes":["user:profile"],"subscriptionType":"pro"}}`), 0o600)

	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer tok-FRESH" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":{"type":"authentication_error","message":"bad token"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	c := api.NewClient(&auth.Credentials{
		AccessToken: "tok-DEAD", RefreshToken: "r1",
		ExpiresAt: 9999999999999, Borrowed: true,
	}, "sess", api.EntrySDKCLI)
	c.BaseURL = srv.URL
	c.HTTP = srv.Client()

	resp, err := c.Do(context.Background(), "POST", "/v1/messages", map[string]any{"x": 1})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (auths: %v)", resp.StatusCode, auths)
	}
	if len(auths) < 2 || auths[len(auths)-1] != "Bearer tok-FRESH" {
		t.Fatalf("did not re-borrow rotated token: %v", auths)
	}
	for _, a := range auths[:len(auths)-1] {
		if a != "Bearer tok-DEAD" {
			t.Fatalf("unexpected early auth: %v", auths)
		}
	}
}

// TestBorrowedCredsNoFreshFallback: when the store still holds the same
// dead token, bounded retries exhaust and the error surfaces — the
// borrowed refresh token is never used to rotate.
func TestBorrowedCredsNoFreshFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_GO_RAW_HTTP", "0")
	t.Setenv("CLAUDE_GO_CREDS_FILE", filepath.Join(home, "own.json"))
	claudeDir := filepath.Join(home, ".claude")
	os.MkdirAll(claudeDir, 0o755)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), []byte(
		`{"claudeAiOauth":{"accessToken":"tok-DEAD","refreshToken":"r1",`+
			`"expiresAt":9999999999999,"scopes":["user:profile"],"subscriptionType":"pro"}}`), 0o600)

	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"type":"authentication_error","message":"dead"}}`)
	}))
	defer srv.Close()

	c := api.NewClient(&auth.Credentials{
		AccessToken: "tok-DEAD", RefreshToken: "r1",
		ExpiresAt: 9999999999999, Borrowed: true,
	}, "sess", api.EntrySDKCLI)
	c.BaseURL = srv.URL
	c.HTTP = srv.Client()

	resp, err := c.Do(context.Background(), "POST", "/v1/messages", map[string]any{"x": 1})
	if err == nil {
		resp.Body.Close()
	}
	if n < 2 || n > 4 {
		t.Fatalf("expected bounded 401 retries, got %d attempts", n)
	}
}
