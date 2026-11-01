package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doctorEnv isolates doctor from the developer's real config, cache and network.
func doctorEnv(t *testing.T, settings string) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	clearTokenEnv(t)
	t.Setenv("LITELLM_PROXY_URL", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("LITELLM_PLUGIN_CLAUDE_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none.json"))
	writeSettings(t, []byte(settings))
	origManaged, origFetch := managedSettingsDir, fetchLatestVersion
	t.Cleanup(func() { managedSettingsDir, fetchLatestVersion = origManaged, origFetch })
	managedSettingsDir = func() string { return t.TempDir() }
	fetchLatestVersion = func() string { return "" }
}

func TestDoctorHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key/info":
			_, _ = w.Write([]byte(`{"info":{"team_id":"t1"}}`))
		case "/team/info":
			_, _ = w.Write([]byte(`{"team_info":{"spend":10,"max_budget":40,"budget_duration":"7d"}}`))
		}
	}))
	defer srv.Close()
	exe, _ := os.Executable()
	doctorEnv(t, `{"env":{"LITELLM_PROXY_URL":"`+srv.URL+`","LITELLM_PROXY_API_KEY":"sk-secret"},
		"statusLine":{"type":"command","command":"`+filepath.ToSlash(exe)+`","refreshInterval":30}}`)

	var out bytes.Buffer
	code := runDoctor(nil, &out, &out)
	report := out.String()
	if strings.Contains(report, "sk-secret") {
		t.Fatal("doctor printed the token")
	}
	for _, want := range []string{"$10.00 of $40.00 (25%)", "LITELLM_PROXY_API_KEY from settings.json env", "settings.json env"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
	// The test binary isn't named claude-code-litellm-plugin, so statusLine is
	// reported as foreign (a warning, not a failure).
	if code != 0 {
		t.Errorf("exit code = %d, report:\n%s", code, report)
	}
}

func TestDoctorReportsProblems(t *testing.T) {
	doctorEnv(t, `{"env":{"LITELLM_PROXY_API_KEY":"sk"}}`)
	managedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(managedDir, "managed-settings.json"), []byte(`{"allowManagedHooksOnly":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	managedSettingsDir = func() string { return managedDir }

	var out bytes.Buffer
	if code := runDoctor(nil, &out, &out); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	report := out.String()
	for _, want := range []string{"statusLine: not configured", "proxy URL: not set", "allowManagedHooksOnly"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
}

func TestDoctorAuthFailureHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	doctorEnv(t, `{"env":{"LITELLM_PROXY_URL":"`+srv.URL+`","LITELLM_PROXY_API_KEY":"sk"}}`)
	var out bytes.Buffer
	runDoctor(nil, &out, &out)
	if !strings.Contains(out.String(), "Auth error") || !strings.Contains(out.String(), "hint:") {
		t.Errorf("report:\n%s", out.String())
	}
}
