package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"claude-go/internal/api"
	"claude-go/internal/auth"
)

// StartupTraffic fires real's captured GET sequence in order and stores
// the bootstrap/account-settings payloads on the client.
func TestStartupTrafficSequence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_GO_RAW_HTTP", "0")
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/claude_cli/bootstrap"):
			json.NewEncoder(w).Encode(map[string]any{"auto_compact": map[string]any{"enabled": true}})
		case r.URL.Path == "/api/oauth/account/settings":
			json.NewEncoder(w).Encode(map[string]any{"some_setting": 1})
		default:
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer srv.Close()
	c := api.NewClient(&auth.Credentials{AccessToken: "tok"}, "sess", api.EntrySDKCLI)
	c.BaseURL = srv.URL
	c.HTTP = srv.Client()

	c.StartupTraffic(context.Background(), "claude-fable-5-1")

	want := []string{
		"/v1/mcp_servers?limit=1000&include_additional_installs=true",
		"/v1/mcp_servers?limit=1000",
		"/api/oauth/account/settings",
		"/mcp-registry/v0/servers?version=latest&limit=100&visibility=commercial%2Cgsuite%2Centerprise%2Chealth",
		"/api/claude_cli/bootstrap?entrypoint=sdk-cli&model=claude-fable-5-1",
		"/api/claude_code_penguin_mode",
		"/api/claude_code_grove",
		// no org uuid in the temp HOME → org-scoped calls skipped
		"/v1/mcp_servers?limit=1000&include_additional_installs=true",
		"/v1/mcp_servers?limit=1000",
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != len(want) {
		t.Fatalf("got %d requests %v, want %d", len(paths), paths, len(want))
	}
	for i, p := range want {
		if paths[i] != p {
			t.Fatalf("request %d = %s, want %s\nall: %v", i, paths[i], p, paths)
		}
	}
	if c.Bootstrap()["auto_compact"] == nil {
		t.Fatal("bootstrap payload not stored")
	}
	if c.AccountSettings()["some_setting"] == nil {
		t.Fatal("account settings payload not stored")
	}
}

// With an oauthAccount.organizationUuid in ~/.claude.json the org-scoped
// calls interleave exactly as captured.
func TestStartupTrafficOrgScoped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_GO_RAW_HTTP", "0")
	os.WriteFile(filepath.Join(home, ".claude.json"),
		[]byte(`{"oauthAccount":{"organizationUuid":"org-1"}}`), 0o600)
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer srv.Close()
	c := api.NewClient(&auth.Credentials{AccessToken: "tok"}, "sess", api.EntrySDKCLI)
	c.BaseURL = srv.URL
	c.HTTP = srv.Client()

	c.StartupTraffic(context.Background(), "claude-fable-5-1")

	skills := "/api/oauth/organizations/org-1/skills/list-skills?include_wiggle_skills=true&entrypoint=sdk-cli"
	plugins := "/api/oauth/organizations/org-1/plugins/list-plugins?enabled_only=true&compact=true&limit=100&offset=0"
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(paths, "\n")
	for _, p := range []string{skills, plugins, "/api/oauth/organizations/org-1/marketplaces"} {
		if !strings.Contains(joined, p) {
			t.Fatalf("missing %s in:\n%s", p, joined)
		}
	}
	// second skills precedes the second mcp_servers pair; second plugins last
	if strings.Count(joined, skills) != 2 || strings.Count(joined, plugins) != 2 {
		t.Fatalf("org calls not doubled as captured:\n%s", joined)
	}
	if paths[len(paths)-1] != plugins {
		t.Fatalf("last request = %s, want %s", paths[len(paths)-1], plugins)
	}
}

// QuotaProbe posts the haiku max_tokens=1 body with the auxiliary class.
func TestQuotaProbe(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_GO_RAW_HTTP", "0")
	var gotClass, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClass = r.Header.Get("x-claude-code-request-class")
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","content":[],"model":"m","stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	c := api.NewClient(&auth.Credentials{AccessToken: "tok"}, "sess", api.EntrySDKCLI)
	c.BaseURL = srv.URL
	c.HTTP = srv.Client()

	c.QuotaProbe(context.Background())
	if gotClass != "auxiliary" {
		t.Fatalf("request-class = %q", gotClass)
	}
	for _, s := range []string{
		`"model":"claude-haiku-4-5-20251001"`, `"max_tokens":1`,
		`"content":"quota"`, `\"session_id\":\"sess\"`,
	} {
		if !strings.Contains(gotBody, s) {
			t.Fatalf("body missing %s: %s", s, gotBody)
		}
	}
}

// LogSessionStart posts a ClaudeCodeInternalEvent batch.
func TestLogSessionStart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_GO_RAW_HTTP", "0")
	var payload struct {
		Events []struct {
			Type      string         `json:"event_type"`
			EventData map[string]any `json:"event_data"`
		} `json:"events"`
	}
	var svc string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		svc = r.Header.Get("x-service-name")
		json.NewDecoder(r.Body).Decode(&payload)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := api.NewClient(&auth.Credentials{AccessToken: "tok", SubscriptionType: "max"},
		"sess", api.EntrySDKCLI)
	c.BaseURL = srv.URL
	c.HTTP = srv.Client()

	c.LogSessionStart(context.Background(), "claude-fable-5-1", false, api.SessionFacts{
		Plugins: []api.PluginFact{{Name: "flopwire", Marketplace: "mkt", Scope: "local"}},
		Skills:  []api.SkillFact{{Name: "deep-research", Source: "userSettings", LoadedFrom: "skills", ContentChars: 500}},
		MCPOk:   []string{"docs"}, MCPFail: []string{"deepwiki"},
		CLIFlags: []string{"continue", "print"},
	})
	if svc != "claude-code" {
		t.Fatalf("x-service-name = %q", svc)
	}
	if len(payload.Events) < 30 {
		t.Fatalf("events = %d — startup burst should emit the lifecycle set", len(payload.Events))
	}
	names := map[string]int{}
	for _, e := range payload.Events {
		if e.Type != "ClaudeCodeInternalEvent" {
			t.Fatalf("event_type = %q", e.Type)
		}
		names[fmt.Sprint(e.EventData["event_name"])]++
	}
	for _, want := range []string{
		"tengu_feature_ok", "tengu_feature_bad", "tengu_feature_sad",
		"tengu_skill_loaded", "tengu_plugin_enabled_for_session",
		"tengu_started", "tengu_cli_flags",
	} {
		if names[want] == 0 {
			t.Fatalf("missing %s in %v", want, names)
		}
	}
}
