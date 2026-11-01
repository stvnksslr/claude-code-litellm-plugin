package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// doctor prints a human-readable diagnosis of everything the status line
// depends on. It never prints a token — only where it came from.
type doctor struct {
	w        io.Writer
	failures int
}

func (d *doctor) ok(format string, a ...any)   { d.line("✓", format, a...) }
func (d *doctor) warn(format string, a ...any) { d.line("!", format, a...) }
func (d *doctor) info(format string, a ...any) { d.line(" ", format, a...) }
func (d *doctor) fail(format string, a ...any) {
	d.failures++
	d.line("✗", format, a...)
}
func (d *doctor) line(mark, format string, a ...any) {
	_, _ = fmt.Fprintf(d.w, "%s %s\n", mark, fmt.Sprintf(format, a...))
}

// managedSettingsDir is where Claude Code's admin-managed settings live.
var managedSettingsDir = func() string {
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/ClaudeCode"
	case "windows":
		pf := os.Getenv("ProgramFiles")
		if pf == "" {
			pf = `C:\Program Files`
		}
		return filepath.Join(pf, "ClaudeCode")
	default:
		return "/etc/claude-code"
	}
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	d := &doctor{w: stdout}

	// Snapshot which vars came from the shell before applying settings env,
	// so every value can be attributed to its source.
	shellHad := map[string]bool{}
	for _, k := range []string{"LITELLM_PROXY_URL", "ANTHROPIC_BASE_URL", "LITELLM_PROXY_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		shellHad[k] = os.Getenv(k) != ""
	}
	applied := applyClaudeSettingsEnv()
	fromSettings := map[string]bool{}
	for _, k := range applied {
		fromSettings[k] = true
	}
	sourceOf := func(k string) string {
		if fromSettings[k] {
			return "settings.json env"
		}
		if shellHad[k] {
			return "shell environment"
		}
		return "unset"
	}

	exe, _ := os.Executable()
	d.info("%s %s (%s)", binaryName, Version, exe)
	doctorUpdate(d)
	doctorSettings(d, exe)
	doctorManaged(d)

	if len(applied) > 0 {
		d.info("settings.json env provides: %s", strings.Join(applied, ", "))
	}

	cred, notes := resolveCredentialWithNotes()
	switch {
	case os.Getenv("LITELLM_PROXY_URL") != "":
		d.ok("proxy URL: %s (LITELLM_PROXY_URL, %s)", cred.baseURLOr(os.Getenv("LITELLM_PROXY_URL")), sourceOf("LITELLM_PROXY_URL"))
	case os.Getenv("ANTHROPIC_BASE_URL") != "":
		d.ok("proxy URL: %s (ANTHROPIC_BASE_URL, %s)", cred.baseURLOr(os.Getenv("ANTHROPIC_BASE_URL")), sourceOf("ANTHROPIC_BASE_URL"))
	case cred.gateway:
		d.ok("proxy URL: %s (from the LiteLLM gateway login)", cred.baseURL)
	default:
		d.fail("proxy URL: not set — set LITELLM_PROXY_URL or ANTHROPIC_BASE_URL (e.g. in the env block of %s)", claudeSettingsPath())
	}

	if cred.token == "" {
		d.fail("credential: none found — set LITELLM_PROXY_API_KEY (or ANTHROPIC_AUTH_TOKEN), or sign in to Claude Code through a LiteLLM gateway (/login)")
	} else {
		src := cred.source
		if fromSettings[src] {
			src += " from settings.json env"
		} else if shellHad[src] {
			src += " from the shell environment"
		}
		kind := "API key"
		if cred.gateway {
			kind = "LiteLLM gateway SSO token"
		}
		d.ok("credential: %s (%s)", kind, src)
	}
	for _, n := range notes {
		d.info("  %s", n)
	}

	if cred.token != "" && cred.baseURL != "" {
		doctorFetch(d, cred)
	}

	doctorCache(d)
	d.info("If the status line is still blank: accept the workspace trust prompt, and run `claude --debug` to see the command's stderr/exit code.")

	if d.failures > 0 {
		_, _ = fmt.Fprintf(stdout, "\n%d problem(s) found.\n", d.failures)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "\nAll checks passed.")
	return 0
}

// baseURLOr prefers the URL the credential is bound to (gateway mode may rewrite it).
func (c credential) baseURLOr(fallback string) string {
	if c.baseURL != "" {
		return c.baseURL
	}
	return fallback
}

func doctorUpdate(d *doctor) {
	if !updateCheckEnabled() {
		d.info("update check: disabled (dev build or LITELLM_PLUGIN_UPDATE_CHECK=0)")
		return
	}
	latest := fetchLatestVersion()
	switch {
	case latest == "":
		d.warn("update check: could not reach GitHub (set LITELLM_PLUGIN_UPDATE_CHECK=0 to disable it on restricted networks)")
	case isUpdateAvailable(Version, latest):
		d.warn("update available: %s — re-run the installer, or `brew upgrade` / `scoop update`", latest)
	default:
		d.ok("up to date (latest release %s)", latest)
	}
}

func doctorSettings(d *doctor, exe string) {
	path := claudeSettingsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		d.fail("settings: cannot read %s (%v) — run `%s setup`", path, err, binaryName)
		return
	}
	if bytes.HasPrefix(data, utf8BOM) {
		d.warn("settings: %s starts with a UTF-8 BOM, which some Claude Code builds reject — run `%s setup` to rewrite it", path, binaryName)
	}
	obj, err := parseOrderedObject(data)
	if err != nil {
		d.fail("settings: %s is not valid JSON: %v", path, err)
		return
	}
	raw, ok := obj.get("statusLine")
	if !ok || string(raw) == "null" {
		d.fail("statusLine: not configured in %s — run `%s setup`", path, binaryName)
		return
	}
	var sl struct {
		Command         string `json:"command"`
		RefreshInterval *int   `json:"refreshInterval"`
	}
	_ = json.Unmarshal(raw, &sl)
	if !isOurCommand(sl.Command) {
		d.warn("statusLine: runs %q, not this plugin (`%s setup --force` replaces it)", sl.Command, binaryName)
		return
	}
	target := resolveCommandPath(sl.Command)
	switch {
	case target == "":
		d.fail("statusLine: command %q does not resolve to an executable — run `%s setup` again", sl.Command, binaryName)
	case exe != "" && !sameFile(target, exe):
		d.warn("statusLine: command %q runs %s, a different copy than this one (%s)", sl.Command, target, exe)
	default:
		d.ok("statusLine: %s → %q", path, sl.Command)
	}
	if sl.RefreshInterval == nil {
		d.warn("statusLine: no refreshInterval, so idle sessions never refresh — run `%s setup` to add it", binaryName)
	}
}

// resolveCommandPath finds the executable a statusLine command would run
// (first word, ~ expanded, PATH lookup for bare names), or "".
func resolveCommandPath(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	var first string
	if cmd != "" && (cmd[0] == '"' || cmd[0] == '\'') {
		if end := strings.IndexByte(cmd[1:], cmd[0]); end >= 0 {
			first = cmd[1 : end+1]
		}
	} else if f := strings.Fields(cmd); len(f) > 0 {
		first = f[0]
	}
	if rest, ok := strings.CutPrefix(first, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			first = filepath.Join(home, rest)
		}
	}
	p, err := exec.LookPath(first)
	if err != nil {
		return ""
	}
	return p
}

func doctorManaged(d *doctor) {
	dir := managedSettingsDir()
	files := []string{filepath.Join(dir, "managed-settings.json")}
	if extra, err := filepath.Glob(filepath.Join(dir, "managed-settings.d", "*.json")); err == nil {
		sort.Strings(extra)
		files = append(files, extra...)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var m struct {
			AllowManagedHooksOnly bool            `json:"allowManagedHooksOnly"`
			DisableAllHooks       bool            `json:"disableAllHooks"`
			StatusLine            json.RawMessage `json:"statusLine"`
		}
		if json.Unmarshal(bytes.TrimPrefix(data, utf8BOM), &m) != nil {
			continue
		}
		if m.AllowManagedHooksOnly {
			d.warn("managed settings %s set allowManagedHooksOnly: a user statusLine is skipped silently", f)
		}
		if m.DisableAllHooks {
			d.warn("managed settings %s set disableAllHooks: the status line is disabled", f)
		}
		if len(m.StatusLine) > 0 && string(m.StatusLine) != "null" {
			d.warn("managed settings %s define their own statusLine, which overrides yours", f)
		}
	}
}

func doctorFetch(d *doctor, cred credential) {
	info, err := fetchBudget(cred)
	if err != nil {
		text, code := classifyError(err)
		d.fail("budget: %s — %v", text, err)
		if hint := errorHint(code, err); hint != "" {
			d.info("  hint: %s", hint)
		}
		return
	}
	writeBudgetCache(cred, info) // a successful check also warms the cache
	eff := resolveEffectiveBudget(info)
	if eff.MaxBudget == nil || *eff.MaxBudget <= 0 {
		d.warn("budget: connected, but no team/user max_budget is configured in LiteLLM (the status line shows \"No budget configured\")")
		return
	}
	spend := 0.0
	if eff.Spend != nil {
		spend = *eff.Spend
	}
	reset, label := formatTimeUntilReset(eff.BudgetResetAt, eff.BudgetDuration)
	extra := ""
	if reset != "" {
		extra = fmt.Sprintf(", %s reset in %s", strings.TrimSpace(label), reset)
	}
	d.ok("budget: $%.2f of $%.2f (%.0f%%)%s", spend, *eff.MaxBudget, spend / *eff.MaxBudget * 100, extra)
}

func errorHint(code string, err error) string {
	var hErr *httpStatusError
	switch {
	case code == "auth error":
		return "the proxy rejected the credential; check the key, or run /login in Claude Code if you use gateway SSO"
	case code == "connection error":
		return "check the proxy URL and your network/VPN; behind a proxy, set HTTPS_PROXY"
	case errors.As(err, &hErr) && hErr.Status == 404:
		return "the proxy URL should be the LiteLLM root (no /v1 or other path suffix)"
	}
	return ""
}

func doctorCache(d *doctor) {
	dir := cacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		d.warn("cache: %s is not writable (%v); every refresh will hit the network", dir, err)
		return
	}
	f, err := os.CreateTemp(dir, "doctor.tmp-*")
	if err != nil {
		d.warn("cache: %s is not writable (%v); every refresh will hit the network", dir, err)
		return
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	d.ok("cache: %s", dir)
}
