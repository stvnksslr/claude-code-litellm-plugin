package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantText string
		wantCode string
	}{
		{"auth", fmt.Errorf("status=401: %w", ErrAuth), "Auth error", "auth error"},
		{"no key", fmt.Errorf("%w", ErrNoAPIKey), "No API key", "no api key"},
		{"no base url", ErrNoBaseURL, "No proxy URL", "no proxy url"},
		{"budget", &BudgetExceededError{}, "Budget exceeded", "budget exceeded"},
		{"dial", errors.New("connection error: dial tcp: refused"), "Connection error", "connection error"},
		{"deadline", fmt.Errorf("x: %w", context.DeadlineExceeded), "Connection error", "connection error"},
		{"http 500", &httpStatusError{Status: 500}, "Error", "error"},
		{"cached transport", &cachedError{msg: "connection error: timeout"}, "Connection error", "connection error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, code := classifyError(tc.err)
			if text != tc.wantText || code != tc.wantCode {
				t.Errorf("classifyError() = (%q, %q), want (%q, %q)", text, code, tc.wantText, tc.wantCode)
			}
		})
	}
}

func TestBudgetInfoNoBaseURL(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	_, err := budgetInfo(credential{token: "sk-x"})
	if !errors.Is(err, ErrNoBaseURL) {
		t.Fatalf("budgetInfo() error = %v, want ErrNoBaseURL", err)
	}
	t.Setenv("LITELLM_PLUGIN_PREFIX", "")
	if got := stripANSI(renderLine(nil, "", StatusInput{}, err)); !strings.Contains(got, "No proxy URL") {
		t.Errorf("renderLine() = %q, want No proxy URL", got)
	}
}

// keyTeamServer serves /key/info with a team id and /team/info with teamStatus.
func keyTeamServer(t *testing.T, teamStatus int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key/info":
			_, _ = w.Write([]byte(`{"info":{"team_id":"team-1","user_id":"u1"}}`))
		case "/team/info":
			w.WriteHeader(teamStatus)
			if teamStatus == http.StatusOK {
				_, _ = w.Write([]byte(`{"team_info":{"spend":5,"max_budget":50}}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTeamInfoFailureHandling(t *testing.T) {
	t.Run("5xx propagates as an error (not cached as no-budget)", func(t *testing.T) {
		t.Setenv("XDG_CACHE_HOME", t.TempDir())
		srv := keyTeamServer(t, http.StatusBadGateway)
		c := credential{token: "sk", baseURL: srv.URL}
		_, err := budgetInfo(c)
		// 5xx is connection-classified (a proxy-side outage, eligible for the
		// stale fallback) rather than an authoritative httpStatusError — but it
		// must still propagate, never be cached as "no budget".
		if !isTransportErr(err) || !strings.Contains(err.Error(), "502") {
			t.Fatalf("budgetInfo() error = %v, want transport-classified 502", err)
		}
		if _, ok := readBudgetCache(c); ok {
			t.Error("a failed /team/info must not be cached as success")
		}
	})
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(fmt.Sprintf("%d tolerated as no team budget", status), func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			srv := keyTeamServer(t, status)
			info, err := budgetInfo(credential{token: "sk", baseURL: srv.URL})
			if err != nil {
				t.Fatalf("budgetInfo() error = %v, want nil", err)
			}
			if info.TeamMaxBudget != nil {
				t.Errorf("TeamMaxBudget = %v, want nil", *info.TeamMaxBudget)
			}
		})
	}
	t.Run("user agent identifies the plugin", func(t *testing.T) {
		t.Setenv("XDG_CACHE_HOME", t.TempDir())
		var ua string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ua = r.UserAgent()
			_, _ = w.Write([]byte(`{"info":{}}`))
		}))
		defer srv.Close()
		if _, err := budgetInfo(credential{token: "sk", baseURL: srv.URL}); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(ua, "claude-code-litellm-plugin/") {
			t.Errorf("User-Agent = %q", ua)
		}
	})
}

func TestUpdateCheckClaimsBeforeFetching(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("LITELLM_PLUGIN_UPDATE_CHECK", "")
	origVersion, origFetch := Version, fetchLatestVersion
	t.Cleanup(func() { Version, fetchLatestVersion = origVersion, origFetch })
	Version = "1.0.0"

	claimed := false
	fetchLatestVersion = func() string {
		// Simulates a process killed mid-request: the backoff must already exist.
		_, claimed = readUpdateCache()
		return "1.1.0"
	}
	if got := getLatestVersion(); got != "1.1.0" {
		t.Fatalf("getLatestVersion() = %q, want 1.1.0", got)
	}
	if !claimed {
		t.Error("update cache was not written before the network call")
	}
	if v, ok := readUpdateCache(); !ok || v != "1.1.0" {
		t.Errorf("readUpdateCache() = (%q, %v), want 1.1.0", v, ok)
	}
}

func TestUpdateCheckDisabled(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	origVersion, origFetch := Version, fetchLatestVersion
	t.Cleanup(func() { Version, fetchLatestVersion = origVersion, origFetch })
	called := false
	fetchLatestVersion = func() string { called = true; return "9.9.9" }

	Version = "dev"
	if getLatestVersion() != "" || called {
		t.Error("dev builds must not check for updates")
	}
	Version = "1.0.0"
	t.Setenv("LITELLM_PLUGIN_UPDATE_CHECK", "0")
	if getLatestVersion() != "" || called {
		t.Error("LITELLM_PLUGIN_UPDATE_CHECK=0 must disable the check")
	}
}

func writeSettings(t *testing.T, content []byte) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestApplyClaudeSettingsEnv(t *testing.T) {
	bom := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"env":{"LITELLM_PROXY_URL":"https://settings.example","NUM":5,"LITELLM_PLUGIN_PREFIX":"from-settings"}}`)...)
	writeSettings(t, bom)
	t.Setenv("LITELLM_PROXY_URL", "https://shell.example")
	t.Setenv("NUM", "")
	t.Setenv("LITELLM_PLUGIN_PREFIX", "")

	keys := applyClaudeSettingsEnv()
	if strings.Join(keys, ",") != "LITELLM_PLUGIN_PREFIX,LITELLM_PROXY_URL" {
		t.Errorf("applied keys = %v (non-string values must be skipped)", keys)
	}
	if got := os.Getenv("LITELLM_PROXY_URL"); got != "https://settings.example" {
		t.Errorf("settings env must override the process env (BOM tolerated), got %q", got)
	}
}

func TestRunModes(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	clearTokenEnv(t)
	t.Setenv("LITELLM_PLUGIN_CLAUDE_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("LITELLM_PLUGIN_PREFIX", "")

	t.Run("--version", func(t *testing.T) {
		var out bytes.Buffer
		if code := run([]string{"--version"}, strings.NewReader(""), &out, &out); code != 0 || strings.TrimSpace(out.String()) != Version {
			t.Errorf("run(--version) = %d %q", code, out.String())
		}
	})
	t.Run("unknown argument", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if code := run([]string{"bogus"}, strings.NewReader(""), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "Usage") {
			t.Errorf("run(bogus) = %d, stderr %q", code, errOut.String())
		}
	})
	t.Run("--json loads settings env and reports version", func(t *testing.T) {
		writeSettings(t, []byte(`{"env":{"LITELLM_PROXY_URL":"https://p.example"}}`))
		t.Setenv("LITELLM_PROXY_URL", "")
		var out bytes.Buffer
		run([]string{"--json"}, strings.NewReader("{}"), &out, &out)
		var s StatusJSON
		if err := json.Unmarshal(out.Bytes(), &s); err != nil {
			t.Fatalf("invalid JSON %q: %v", out.String(), err)
		}
		if s.Version != Version || s.Error != "no api key" {
			t.Errorf("status = %+v", s)
		}
		if os.Getenv("LITELLM_PROXY_URL") != "https://p.example" {
			t.Error("--json did not apply the settings env block")
		}
	})
	t.Run("plain mode never loads settings env", func(t *testing.T) {
		writeSettings(t, []byte(`{"env":{"LITELLM_PROXY_URL":"https://p.example"}}`))
		t.Setenv("LITELLM_PROXY_URL", "")
		var out bytes.Buffer
		run(nil, strings.NewReader("{}"), &out, &out)
		if os.Getenv("LITELLM_PROXY_URL") != "" {
			t.Error("plain statusline mode must not apply settings env")
		}
	})
}

func TestClaudeConfigDirSkipsDefaultKeychain(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LITELLM_PLUGIN_CLAUDE_CREDENTIALS_FILE", "")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // no .credentials.json inside
	creds := []byte(gatewayCredsJSON("gw-jwt", "https://gw.example/claude_code_gateway", 1<<62))
	withAcct, noAcct := stubKeychain(t, creds, creds)

	c, notes := resolveCredentialWithNotes()
	if c.token != "" {
		t.Errorf("token resolved from the default Keychain item despite CLAUDE_CONFIG_DIR")
	}
	if *withAcct+*noAcct != 0 {
		t.Errorf("Keychain read %d times, want 0", *withAcct+*noAcct)
	}
	if !strings.Contains(credentialsFilePath(), os.Getenv("CLAUDE_CONFIG_DIR")) {
		t.Errorf("credentials file %q not under CLAUDE_CONFIG_DIR", credentialsFilePath())
	}
	if len(notes) == 0 && keychainSkipReason() != "" {
		t.Error("expected a note explaining the Keychain skip")
	}
}

func TestResolveCredentialSource(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok")
	if c := resolveCredential(); c.source != "ANTHROPIC_AUTH_TOKEN" {
		t.Errorf("source = %q", c.source)
	}
}
