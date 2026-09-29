package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Version is set at build time via -ldflags="-X main.Version=vX.Y.Z"
var Version = "dev"

// GitHub repo for update checks
const GitHubRepo = "stvnksslr/claude-code-litellm-plugin"

// ANSI color codes
const (
	ColorGreen  = "\x1b[32m"
	ColorYellow = "\x1b[33m"
	ColorRed    = "\x1b[31m"
	ColorGray   = "\x1b[90m"
	ColorReset  = "\x1b[0m"
)

// Cache configuration
const (
	CacheTTLMs         = 30_000          // 30 seconds in milliseconds
	BudgetFailTTLMs    = 10_000          // negative-cache window for failed budget fetches
	HTTPTimeout        = 3 * time.Second // fast failure for subprocess/statusline use
	UpdateCheckTTLMs   = 60 * 60 * 1_000 // 1 hour in milliseconds
	UpdateCheckTimeout = 5 * time.Second
)

// ErrAuth is returned when the API responds with a 401 or 403 status.
var ErrAuth = errors.New("auth error")

// ErrNoAPIKey is returned when neither LITELLM_PROXY_API_KEY nor ANTHROPIC_AUTH_TOKEN is set
// and Claude Code's credential store holds no usable LiteLLM gateway SSO token.
var ErrNoAPIKey = errors.New("no api key")

// ErrBudgetExceeded is returned when the API reports the key's budget has been exceeded.
var ErrBudgetExceeded = errors.New("budget exceeded")

// BudgetExceededError wraps ErrBudgetExceeded with the spend/budget values parsed from the error message.
type BudgetExceededError struct {
	Spend     float64
	MaxBudget float64
}

func (e *BudgetExceededError) Error() string { return ErrBudgetExceeded.Error() }
func (e *BudgetExceededError) Is(target error) bool {
	return target == ErrBudgetExceeded
}
func (e *BudgetExceededError) Unwrap() error { return ErrBudgetExceeded }

// liteLLMError is the error envelope returned by LiteLLM on non-2xx responses.
type liteLLMError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// BudgetCacheEntry is the on-disk representation of a cached budget API response.
type BudgetCacheEntry struct {
	Timestamp int64   `json:"timestamp"` // Unix milliseconds
	Info      KeyInfo `json:"info"`
}

// UpdateCacheEntry is the on-disk representation of a cached GitHub version check.
type UpdateCacheEntry struct {
	Timestamp     int64  `json:"timestamp"` // Unix milliseconds
	LatestVersion string `json:"latest_version"`
}

// BudgetFailEntry is the on-disk negative-cache record of a failed budget fetch.
// It captures enough to reconstruct an equivalent error (so main()'s classification
// keeps working) without making another network call within BudgetFailTTLMs.
type BudgetFailEntry struct {
	Timestamp int64   `json:"timestamp"`            // Unix milliseconds
	Kind      string  `json:"kind"`                 // "auth" | "budget" | "transport"
	Message   string  `json:"message,omitempty"`    // original error text, for debug output
	Spend     float64 `json:"spend,omitempty"`      // populated when Kind == "budget"
	MaxBudget float64 `json:"max_budget,omitempty"` // populated when Kind == "budget"
}

// cachedError reconstructs a previously-seen fetch error from the negative cache.
// Unwrap exposes a sentinel (e.g. ErrAuth) so errors.Is still matches, while Error
// preserves the original message for debug output. A nil sentinel matches nothing.
type cachedError struct {
	msg      string
	sentinel error
}

func (c *cachedError) Error() string { return c.msg }
func (c *cachedError) Unwrap() error { return c.sentinel }

// KeyInfoResponse represents the API response structure
type KeyInfoResponse struct {
	Info KeyInfo `json:"info"`
}

// KeyInfo represents the budget information
type KeyInfo struct {
	Spend          *float64 `json:"spend"`
	MaxBudget      *float64 `json:"max_budget"`
	BudgetResetAt  *string  `json:"budget_reset_at"`
	BudgetDuration *string  `json:"budget_duration"`
	TeamID         *string  `json:"team_id"`
	UserID         *string  `json:"user_id"`
	// Team-level budget fields (populated from /team/info when key has no max_budget)
	TeamSpend          *float64 `json:"team_spend"`
	TeamMaxBudget      *float64 `json:"team_max_budget"`
	TeamBudgetResetAt  *string  `json:"team_budget_reset_at"`
	TeamBudgetDuration *string  `json:"team_budget_duration"`
}

// TeamMemberBudgetTable holds the per-user budget within a team. It backs both
// team_info.team_member_budget_table and each team_memberships[].litellm_budget_table.
type TeamMemberBudgetTable struct {
	MaxBudget      *float64 `json:"max_budget"`
	BudgetDuration *string  `json:"budget_duration"`
	BudgetResetAt  *string  `json:"budget_reset_at"`
}

// TeamInfoData is the nested team_info object in the /team/info response.
type TeamInfoData struct {
	Spend                 *float64               `json:"spend"`
	MaxBudget             *float64               `json:"max_budget"`
	BudgetDuration        *string                `json:"budget_duration"`
	BudgetResetAt         *string                `json:"budget_reset_at"`
	TeamMemberBudgetTable *TeamMemberBudgetTable `json:"team_member_budget_table"`
}

// TeamMembership represents a single entry in the team_memberships array.
// LitellmBudgetTable carries this member's own budget — on many LiteLLM instances
// the per-member budget lives here rather than in team_info.
type TeamMembership struct {
	UserID             string                 `json:"user_id"`
	TeamID             string                 `json:"team_id"`
	Spend              *float64               `json:"spend"`
	LitellmBudgetTable *TeamMemberBudgetTable `json:"litellm_budget_table"`
}

// TeamInfoAPIResponse is the top-level /team/info response.
type TeamInfoAPIResponse struct {
	TeamInfo        TeamInfoData     `json:"team_info"`
	TeamMemberships []TeamMembership `json:"team_memberships"`
}

// resolveEffectiveBudget returns a *KeyInfo populated with the budget to display.
// The team budget is the only source of truth — key-level spend/budget is intentionally
// ignored to avoid confusing fallbacks. When no team budget exists, an empty *KeyInfo is
// returned so no key spend leaks into the statusline.
func resolveEffectiveBudget(info *KeyInfo) *KeyInfo {
	if info.TeamMaxBudget != nil && *info.TeamMaxBudget > 0 {
		return &KeyInfo{
			Spend:          info.TeamSpend,
			MaxBudget:      info.TeamMaxBudget,
			BudgetResetAt:  info.TeamBudgetResetAt,
			BudgetDuration: info.TeamBudgetDuration,
		}
	}
	return &KeyInfo{}
}

// GitHubRelease represents the GitHub releases API response
type GitHubRelease struct {
	TagName string `json:"tag_name"`
}

// StatusInput captures the subset of Claude Code's stdin JSON we care about.
// All fields are optional — missing/null fields are treated as absent.
type StatusInput struct {
	Model struct {
		DisplayName string `json:"display_name"`
		ID          string `json:"id"`
	} `json:"model"`
	ContextWindow *struct {
		UsedPercentage *float64 `json:"used_percentage"`
	} `json:"context_window"`
}

// readStatusInput decodes the JSON payload Claude Code sends on stdin.
// Any parse failure (empty stdin, malformed JSON) yields a zero-valued
// StatusInput — the plugin must keep rendering even when stdin is unusable.
func readStatusInput(r io.Reader) StatusInput {
	var input StatusInput
	_ = json.NewDecoder(r).Decode(&input)
	return input
}

// cacheDir returns the directory used for filesystem caching.
// Respects XDG_CACHE_HOME; falls back to $HOME/.cache.
func cacheDir() string {
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return filepath.Join(xdg, "claude-code-litellm")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "claude-code-litellm")
}

// cacheKey returns a short, stable hash of the credential's base URL + token so
// budget cache files don't bleed across different proxies/keys (e.g. per-project
// configs that point at different LiteLLM instances or use different keys). It
// hashes the credential actually in flight — not a re-resolution of the ambient
// env/Keychain state, which can change between resolution and cache access.
func cacheKey(c credential) string {
	sum := sha256.Sum256([]byte(c.baseURL + "\x00" + c.token))
	return hex.EncodeToString(sum[:])[:12]
}

func budgetCacheFile(c credential) string {
	return filepath.Join(cacheDir(), "budget-"+cacheKey(c)+".json")
}

// budgetFailCacheFile holds the negative-cache marker for failed budget fetches,
// namespaced per-key so a failure for one key doesn't suppress fetches for another.
func budgetFailCacheFile(c credential) string {
	return filepath.Join(cacheDir(), "budget-fail-"+cacheKey(c)+".json")
}

// updateCacheFile is intentionally NOT namespaced by key: the latest GitHub release
// is identical regardless of which LiteLLM key/URL is in use, and a shared file means
// a single backoff is honored across keys (fewer GitHub calls under rate limits).
func updateCacheFile() string {
	return filepath.Join(cacheDir(), "update.json")
}

// writeFileAtomic writes data to path atomically: it writes to a uniquely-named temp
// file in the same directory, then renames it into place. Rename is atomic on the same
// filesystem, so concurrent statusline processes (or goroutines) never observe a torn
// file. The directory must already exist.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// readBudgetCache reads cached budget info from disk.
// Returns nil, false if the cache is missing, corrupt, or older than CacheTTLMs.
func readBudgetCache(c credential) (*KeyInfo, bool) {
	data, err := os.ReadFile(budgetCacheFile(c))
	if err != nil {
		return nil, false
	}
	var entry BudgetCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, false
	}
	if time.Now().UnixMilli()-entry.Timestamp >= CacheTTLMs {
		return nil, false
	}
	return &entry.Info, true
}

// writeBudgetCache writes budget info to the filesystem cache.
// Errors are silently ignored — caching is best-effort.
func writeBudgetCache(c credential, info *KeyInfo) {
	if info == nil {
		return
	}
	entry := BudgetCacheEntry{
		Timestamp: time.Now().UnixMilli(),
		Info:      *info,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if err := os.MkdirAll(cacheDir(), 0o755); err != nil {
		return
	}
	_ = writeFileAtomic(budgetCacheFile(c), data, 0o600)
}

// readBudgetFailCache returns a recent failed-fetch record, if one exists within
// BudgetFailTTLMs. Returns nil, false when absent, corrupt, or expired.
func readBudgetFailCache(c credential) (*BudgetFailEntry, bool) {
	data, err := os.ReadFile(budgetFailCacheFile(c))
	if err != nil {
		return nil, false
	}
	var entry BudgetFailEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, false
	}
	if time.Now().UnixMilli()-entry.Timestamp >= BudgetFailTTLMs {
		return nil, false
	}
	return &entry, true
}

// writeBudgetFailCache records a failed budget fetch so subsequent refreshes back off
// instead of re-blocking on the network. Errors are silently ignored — best-effort.
func writeBudgetFailCache(c credential, fetchErr error) {
	if c.token == "" {
		return
	}
	entry := BudgetFailEntry{
		Timestamp: time.Now().UnixMilli(),
		Message:   fetchErr.Error(),
		Kind:      "transport",
	}
	var bErr *BudgetExceededError
	switch {
	case errors.As(fetchErr, &bErr):
		entry.Kind = "budget"
		entry.Spend = bErr.Spend
		entry.MaxBudget = bErr.MaxBudget
	case errors.Is(fetchErr, ErrAuth):
		entry.Kind = "auth"
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if err := os.MkdirAll(cacheDir(), 0o755); err != nil {
		return
	}
	_ = writeFileAtomic(budgetFailCacheFile(c), data, 0o600)
}

// errorFromFailEntry rebuilds an error equivalent to the original failed fetch so
// callers (and main()'s error classification) behave identically without a network call.
func errorFromFailEntry(e *BudgetFailEntry) error {
	switch e.Kind {
	case "budget":
		return &BudgetExceededError{Spend: e.Spend, MaxBudget: e.MaxBudget}
	case "auth":
		return &cachedError{msg: e.Message, sentinel: ErrAuth}
	default:
		return &cachedError{msg: e.Message}
	}
}

// readUpdateCache reads the cached latest GitHub release version from disk.
// Returns "", false if the cache is missing, corrupt, or older than UpdateCheckTTLMs.
func readUpdateCache() (string, bool) {
	data, err := os.ReadFile(updateCacheFile())
	if err != nil {
		return "", false
	}
	var entry UpdateCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return "", false
	}
	if time.Now().UnixMilli()-entry.Timestamp >= UpdateCheckTTLMs {
		return "", false
	}
	return entry.LatestVersion, true
}

// writeUpdateCache persists the latest version string to disk.
// An empty version is persisted deliberately: it records "checked, nothing newer
// (or the check failed)" so a failed/rate-limited GitHub call backs off for the full
// TTL instead of being retried — and re-blocking — on every statusline refresh.
// Errors are silently ignored — caching is best-effort.
func writeUpdateCache(version string) {
	entry := UpdateCacheEntry{
		Timestamp:     time.Now().UnixMilli(),
		LatestVersion: version,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if err := os.MkdirAll(cacheDir(), 0o755); err != nil {
		return
	}
	_ = writeFileAtomic(updateCacheFile(), data, 0o600)
}

// fetchLatestVersion calls the GitHub releases API to get the latest release tag
func fetchLatestVersion() string {
	url := "https://api.github.com/repos/" + GitHubRepo + "/releases/latest"
	client := &http.Client{Timeout: UpdateCheckTimeout}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "claude-code-litellm-plugin/"+Version)

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var release GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return ""
	}
	return release.TagName
}

// getLatestVersion returns the latest GitHub release tag, using a 1-hour filesystem cache.
// Each invocation is a fresh process, so the cache must live on disk.
func getLatestVersion() string {
	if version, ok := readUpdateCache(); ok {
		return version
	}
	latest := fetchLatestVersion()
	// Persist even on "" (failure / rate-limit) so the next refresh reads the cache
	// and backs off rather than re-attempting the network call.
	writeUpdateCache(latest)
	return latest
}

// semverGreater returns true if version a is greater than version b.
// Both should be in "major.minor.patch" format (leading 'v' stripped).
func semverGreater(a, b string) bool {
	parse := func(v string) (int, int, int) {
		var major, minor, patch int
		_, _ = fmt.Sscanf(v, "%d.%d.%d", &major, &minor, &patch)
		return major, minor, patch
	}
	am, an, ap := parse(a)
	bm, bn, bp := parse(b)
	if am != bm {
		return am > bm
	}
	if an != bn {
		return an > bn
	}
	return ap > bp
}

// isUpdateAvailable returns true if latest is a newer semver than current.
func isUpdateAvailable(current, latest string) bool {
	if current == "dev" || latest == "" {
		return false
	}
	c := strings.TrimPrefix(current, "v")
	l := strings.TrimPrefix(latest, "v")
	return l != "" && semverGreater(l, c)
}

// getEnvWithFallback returns the first non-empty environment variable value
func getEnvWithFallback(keys ...string) string {
	for _, key := range keys {
		if val := os.Getenv(key); val != "" {
			return val
		}
	}
	return ""
}

// getBaseURL returns the LiteLLM base URL from environment
func getBaseURL() string {
	url := getEnvWithFallback("LITELLM_PROXY_URL", "ANTHROPIC_BASE_URL")
	return strings.TrimSuffix(url, "/")
}

// credential is the auth token for one run plus the proxy base URL it may be
// sent to. Budget fetches and cache namespacing use these fields rather than
// re-resolving ambient env/Keychain state, which can change mid-run.
type credential struct {
	token   string
	baseURL string
	// gateway marks a LiteLLM gateway SSO token (enterpriseGateway.jwt).
	// Gateway tokens authenticate but are not DB key rows, so budgets come
	// from /user/info instead of /key/info.
	gateway bool
}

// gatewayPathSuffix is the path Claude Code appends to the LiteLLM issuer when
// it stores the gateway URL (e.g. https://litellm.example/claude_code_gateway).
const gatewayPathSuffix = "/claude_code_gateway"

// resolveCredential returns the auth token and the base URL it is bound to.
// Precedence:
//  1. LITELLM_PROXY_API_KEY env var
//  2. ANTHROPIC_AUTH_TOKEN env var
//  3. the LiteLLM gateway SSO token (enterpriseGateway.jwt) from Claude Code's
//     credential store
//
// API-key mode is unchanged — env vars win exactly as before. The credential
// store is only consulted when neither env var is set, and its token is only
// ever sent to the gateway that issued it, over HTTPS (loopback excepted).
// Claude Code's Anthropic OAuth token (claudeAiOauth) is deliberately never
// used: it is a first-party Anthropic credential, not a LiteLLM one.
func resolveCredential() credential {
	baseURL := getBaseURL()
	if tok := getEnvWithFallback("LITELLM_PROXY_API_KEY", "ANTHROPIC_AUTH_TOKEN"); tok != "" {
		return credential{token: tok, baseURL: baseURL}
	}
	for _, read := range credentialSources() {
		payload := read()
		if len(payload) == 0 {
			continue
		}
		jwt, gatewayURL := extractGatewayToken(payload)
		if jwt == "" {
			continue
		}
		if target, ok := gatewayTarget(baseURL, gatewayURL); ok {
			return credential{token: jwt, baseURL: target, gateway: true}
		}
	}
	return credential{baseURL: baseURL}
}

// extractGatewayToken returns the LiteLLM gateway SSO token and the gateway URL
// it was issued for from a credential-store payload. The store shape is
// {"enterpriseGateway":{"jwt":"…","url":"https://…/claude_code_gateway","expiresAt":1790782037250},…}
// with expiresAt in epoch milliseconds. Returns "" when absent, expired,
// unparsable, or missing its URL (an unbound token can't be safely sent).
//
// Expiry comes only from the store's expiresAt: LiteLLM encrypts the token
// (it is not a plain signed JWT), so its own expiry claim can't be read
// client-side. The proxy still rejects a lapsed token with 401.
func extractGatewayToken(payload []byte) (jwt, gatewayURL string) {
	var creds struct {
		EnterpriseGateway *struct {
			JWT       string   `json:"jwt"`
			URL       string   `json:"url"`
			ExpiresAt *float64 `json:"expiresAt"`
		} `json:"enterpriseGateway"`
	}
	if err := json.Unmarshal(payload, &creds); err != nil || creds.EnterpriseGateway == nil {
		return "", ""
	}
	gw := creds.EnterpriseGateway
	if gw.JWT == "" || gw.URL == "" || expired(gw.ExpiresAt) {
		return "", ""
	}
	return gw.JWT, gw.URL
}

// gatewayTarget decides where a gateway token may be sent. The token is bound
// to the gateway that issued it: with a configured base URL, the two must share
// scheme, host, and port; with none, the stored gateway URL (minus the
// /claude_code_gateway suffix) becomes the base URL. Either way the target must
// use HTTPS, or plain HTTP to a loopback host.
func gatewayTarget(baseURL, gatewayURL string) (string, bool) {
	gw, err := url.Parse(gatewayURL)
	if err != nil || gw.Host == "" || !secureTransport(gw) {
		return "", false
	}
	if baseURL == "" {
		return strings.TrimSuffix(strings.TrimSuffix(gw.Scheme+"://"+gw.Host+gw.Path, "/"), gatewayPathSuffix), true
	}
	base, err := url.Parse(baseURL)
	if err != nil || !sameOrigin(base, gw) {
		return "", false
	}
	return baseURL, true
}

// secureTransport reports whether a credential-store token may travel to u:
// HTTPS always, plain HTTP only to a loopback host (local proxies/tests).
func secureTransport(u *url.URL) bool {
	switch strings.ToLower(u.Scheme) {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	return false
}

// sameOrigin compares scheme, host, and effective port (default ports filled in).
func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if strings.EqualFold(u.Scheme, "https") {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		port(a) == port(b)
}

// expired reports whether an epoch-milliseconds timestamp has passed. A nil
// timestamp (unknown expiry) is treated as still valid.
func expired(ms *float64) bool {
	return ms != nil && time.Now().After(time.UnixMilli(int64(*ms)))
}

// LITELLM_PLUGIN_CLAUDE_CREDENTIALS_FILE overrides the default location for
// non-standard installs and tests. Returns "" when no path can be determined.
func credentialsFilePath() string {
	if p := os.Getenv("LITELLM_PLUGIN_CLAUDE_CREDENTIALS_FILE"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

// credentialSources returns lazy readers for Claude Code's credential store,
// most-specific first. resolveCredential stops at the first usable token, so
// in the common case at most one Keychain read happens per refresh.
//  1. the credentials file (LITELLM_PLUGIN_CLAUDE_CREDENTIALS_FILE override or
//     ~/.claude/.credentials.json) — when the override is set it is the only
//     source, keeping tests and non-standard installs deterministic
//  2. on macOS, the login Keychain item Claude Code maintains (service
//     "Claude Code-credentials"): the account-anchored item first, then the
//     accountless lookup. Claude Code stores credentials only in the Keychain
//     on macOS, so gateway-SSO users have no credentials file.
func credentialSources() []func() []byte {
	readFile := func() []byte {
		path := credentialsFilePath()
		if path == "" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		return data
	}
	if os.Getenv("LITELLM_PLUGIN_CLAUDE_CREDENTIALS_FILE") != "" {
		return []func() []byte{readFile}
	}
	sources := []func() []byte{readFile}
	if runtime.GOOS == "darwin" {
		sources = append(sources, keychainReadWithAccount, keychainRead)
	}
	return sources
}

// keychainService is the generic-password service name Claude Code uses.
const keychainService = "Claude Code-credentials"

// keychainRead reads the accountless Keychain lookup — the same query Claude
// Code's own reader falls back to. Swapped out in tests. Returns nil when
// nothing usable is found.
var keychainRead = func() []byte {
	return runSecurity("find-generic-password", "-s", keychainService, "-w")
}

// keychainReadWithAccount queries the Keychain item anchored to the current
// user's account attribute — Claude Code writes one credential item per
// account, and the accountless lookup can return a stale item from an older
// login. Swapped out in tests.
var keychainReadWithAccount = func() []byte {
	name := keychainAccountName()
	if name == "" {
		return nil
	}
	return runSecurity("find-generic-password", "-a", name, "-s", keychainService, "-w")
}

// keychainAccountName resolves the account attribute Claude Code uses for its
// Keychain item (observed: the local username).
func keychainAccountName() string {
	if name := os.Getenv("USER"); name != "" {
		return name
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

// securityBin is invoked by absolute path so a binary named "security" earlier
// on PATH can't impersonate it and feed us a token.
const securityBin = "/usr/bin/security"

// runSecurity shells out to /usr/bin/security, the only supported way to read
// the login Keychain from a non-entitled process. The read is fail-fast (1s
// budget — a locked Keychain would otherwise block on an unlock prompt) and
// the token is never logged.
func runSecurity(args ...string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, securityBin, args...).Output()
	if err != nil {
		return nil
	}
	data := []byte(strings.TrimSpace(string(out)))
	if json.Valid(data) {
		return data
	}
	return nil
}

// isShowCostEnabled returns true only when LITELLM_PLUGIN_SHOW_COST is explicitly enabled.
// Default is false — percent-only display, no dollar amounts.
func isShowCostEnabled() bool {
	val := os.Getenv("LITELLM_PLUGIN_SHOW_COST")
	return val == "1" || val == "true"
}

// getPrefix returns the status line prefix.
// Precedence: LITELLM_PLUGIN_PREFIX (if set, even to empty) > stdin model display name > "LiteLLM: ".
func getPrefix(input StatusInput) string {
	if val, ok := os.LookupEnv("LITELLM_PLUGIN_PREFIX"); ok {
		if val == "" {
			return ""
		}
		return val + " "
	}
	if name := strings.TrimSpace(input.Model.DisplayName); name != "" {
		return name + ": "
	}
	return "LiteLLM: "
}

// budgetInfo fetches budget info for a credential, using a 30-second
// filesystem cache to avoid hitting the API on every statusline refresh. Each
// invocation of this binary is a fresh process, so all state must live on
// disk. Virtual keys go through /key/info; gateway SSO tokens go through
// /user/info — they authenticate but are not DB key rows, so /key/info fails
// on them with a "where.token" lookup error. Either way a /team/info call then
// populates the team budget fields — the only budget the statusline displays.
func budgetInfo(c credential) (*KeyInfo, error) {
	if info, ok := readBudgetCache(c); ok {
		return info, nil
	}
	// Recent failure → back off and replay the cached error instead of re-blocking
	// on the network every refresh while the proxy is down / key is bad / over budget.
	if failed, ok := readBudgetFailCache(c); ok {
		return nil, errorFromFailEntry(failed)
	}
	var info *KeyInfo
	var err error
	if c.gateway {
		info, err = fetchUserInfo(c)
		if err == nil {
			enrichTeamBudget(c, info)
			// Gateway tokens enforce the JWT-scoped team's budget; when that
			// team is unbudgeted, the user-level budget is what gates requests.
			if (info.TeamMaxBudget == nil || *info.TeamMaxBudget <= 0) && info.MaxBudget != nil {
				info.TeamSpend = info.Spend
				info.TeamMaxBudget = info.MaxBudget
				info.TeamBudgetDuration = info.BudgetDuration
				info.TeamBudgetResetAt = info.BudgetResetAt
			}
		}
	} else {
		info, err = fetchKeyInfoWithTeam(c)
	}
	if err != nil {
		writeBudgetFailCache(c, err)
		return nil, err
	}
	writeBudgetCache(c, info)
	return info, nil
}

// fetchKeyInfoWithTeam fetches /key/info and enriches it with the team budget
// (the only budget the statusline displays — key-level spend is never shown).
func fetchKeyInfoWithTeam(c credential) (*KeyInfo, error) {
	info, err := fetchKeyInfo(c)
	if err != nil {
		return nil, err
	}
	enrichTeamBudget(c, info)
	return info, nil
}

// enrichTeamBudget overlays the team budget onto info via /team/info — the
// only budget the statusline displays; key/user-level spend is never shown.
// Primary source is this member's own litellm_budget_table row; fallbacks are
// team_member_budget_table, then the team's own max_budget. Spend is always
// paired with the budget's own source — never the key's spend. Best-effort:
// a failed call leaves info untouched.
func enrichTeamBudget(c credential, info *KeyInfo) {
	if info.TeamID == nil || *info.TeamID == "" {
		return
	}
	teamResp, err := fetchTeamInfo(c, *info.TeamID)
	if err != nil {
		return
	}
	ti := teamResp.TeamInfo
	if info.UserID != nil && *info.UserID != "" {
		for _, m := range teamResp.TeamMemberships {
			if m.UserID != *info.UserID {
				continue
			}
			if m.LitellmBudgetTable != nil && m.LitellmBudgetTable.MaxBudget != nil {
				info.TeamSpend = m.Spend
				info.TeamMaxBudget = m.LitellmBudgetTable.MaxBudget
				info.TeamBudgetDuration = m.LitellmBudgetTable.BudgetDuration
				info.TeamBudgetResetAt = m.LitellmBudgetTable.BudgetResetAt
			}
			break
		}
	}
	if info.TeamMaxBudget == nil {
		switch {
		case ti.TeamMemberBudgetTable != nil && ti.TeamMemberBudgetTable.MaxBudget != nil:
			info.TeamMaxBudget = ti.TeamMemberBudgetTable.MaxBudget
			info.TeamBudgetDuration = ti.TeamMemberBudgetTable.BudgetDuration
		case ti.MaxBudget != nil:
			info.TeamMaxBudget = ti.MaxBudget
		}
		if info.TeamMaxBudget != nil {
			info.TeamSpend = ti.Spend
			if info.TeamBudgetResetAt == nil {
				info.TeamBudgetResetAt = ti.BudgetResetAt
			}
			if info.TeamBudgetDuration == nil {
				info.TeamBudgetDuration = ti.BudgetDuration
			}
		}
	}
}

// UserInfoResponse is the /user/info response LiteLLM serves to the
// authenticated key's user — the budget source for gateway SSO tokens.
type UserInfoResponse struct {
	UserID   string        `json:"user_id"`
	UserInfo UserInfoData  `json:"user_info"`
	Teams    []GatewayTeam `json:"teams"`
}

// UserInfoData carries the user-level budget from /user/info, plus the user
// record's own team list — the source of the gateway token's scoped team.
type UserInfoData struct {
	Teams          []string `json:"teams"`
	Spend          *float64 `json:"spend"`
	MaxBudget      *float64 `json:"max_budget"`
	BudgetDuration *string  `json:"budget_duration"`
	BudgetResetAt  *string  `json:"budget_reset_at"`
}

// GatewayTeam is one entry of /user/info's top-level teams array. That array is
// NOT the user record's team order — for proxy admins it lists every team on
// the proxy — so it is only used to look up the scoped team's fields.
type GatewayTeam struct {
	TeamID         *string  `json:"team_id"`
	TeamAlias      *string  `json:"team_alias"`
	Spend          *float64 `json:"spend"`
	MaxBudget      *float64 `json:"max_budget"`
	BudgetDuration *string  `json:"budget_duration"`
	BudgetResetAt  *string  `json:"budget_reset_at"`
}

// fetchUserInfo fetches budget info via /user/info — the entry point that
// works for LiteLLM gateway SSO tokens. /key/info looks the token up as a DB
// key row and errors on gateway tokens, while /user/info resolves the
// authenticated user. Team budget detail arrives via the shared /team/info
// enrichment; when the scoped team is unbudgeted, the caller falls back to the
// user-level budget.
//
// The team id is an approximation: LiteLLM encrypts the gateway token, so its
// team claim can't be read client-side. LiteLLM scopes the token to "the first
// team on the developer's user record" (user_info.teams[0]); exact for
// single-team users, best-effort for multi-team users, since the proxy picks
// from a DB query without an explicit order.
func fetchUserInfo(c credential) (*KeyInfo, error) {
	baseURL := c.baseURL
	if baseURL == "" {
		return nil, fmt.Errorf("no LiteLLM proxy URL configured (set LITELLM_PROXY_URL or ANTHROPIC_BASE_URL)")
	}
	endpoint := baseURL + "/user/info"

	client := &http.Client{Timeout: HTTPTimeout}
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("request creation failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connection error: %w [url=%s]", err, endpoint)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("status=%d url=%s: %w", resp.StatusCode, endpoint, ErrAuth)
	}

	if resp.StatusCode != 200 {
		var litellmErr liteLLMError
		if json.Unmarshal(body, &litellmErr) == nil && litellmErr.Error.Type == "budget_exceeded" {
			bErr := &BudgetExceededError{}
			_, _ = fmt.Sscanf(litellmErr.Error.Message, "Budget has been exceeded! Current cost: %f, Max budget: %f", &bErr.Spend, &bErr.MaxBudget)
			return nil, bErr
		}
		return nil, fmt.Errorf("HTTP error: status=%d url=%s body=%s", resp.StatusCode, endpoint, truncateBody(body))
	}

	var response UserInfoResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("JSON parse error: %w [body=%s]", err, truncateBody(body))
	}

	info := &KeyInfo{
		Spend:          response.UserInfo.Spend,
		MaxBudget:      response.UserInfo.MaxBudget,
		BudgetDuration: response.UserInfo.BudgetDuration,
		BudgetResetAt:  response.UserInfo.BudgetResetAt,
	}
	if response.UserID != "" {
		id := response.UserID
		info.UserID = &id
	}
	if len(response.UserInfo.Teams) == 0 {
		return info, nil
	}
	teamID := response.UserInfo.Teams[0]
	info.TeamID = &teamID
	for _, t := range response.Teams {
		if t.TeamID != nil && *t.TeamID == teamID {
			info.TeamSpend = t.Spend
			info.TeamMaxBudget = t.MaxBudget
			info.TeamBudgetDuration = t.BudgetDuration
			info.TeamBudgetResetAt = t.BudgetResetAt
			break
		}
	}
	return info, nil
}

// fetchKeyInfo makes the actual API call
func fetchKeyInfo(c credential) (*KeyInfo, error) {
	baseURL := c.baseURL
	if baseURL == "" {
		return nil, fmt.Errorf("no LiteLLM proxy URL configured (set LITELLM_PROXY_URL or ANTHROPIC_BASE_URL)")
	}
	url := baseURL + "/key/info"

	client := &http.Client{Timeout: HTTPTimeout}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("request creation failed: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connection error: %w [url=%s]", err, url)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("status=%d url=%s: %w", resp.StatusCode, url, ErrAuth)
	}

	if resp.StatusCode != 200 {
		var litellmErr liteLLMError
		if json.Unmarshal(body, &litellmErr) == nil && litellmErr.Error.Type == "budget_exceeded" {
			bErr := &BudgetExceededError{}
			_, _ = fmt.Sscanf(litellmErr.Error.Message, "Budget has been exceeded! Current cost: %f, Max budget: %f", &bErr.Spend, &bErr.MaxBudget)
			return nil, bErr
		}
		return nil, fmt.Errorf("HTTP error: status=%d url=%s body=%s", resp.StatusCode, url, truncateBody(body))
	}

	var response KeyInfoResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("JSON parse error: %w [body=%s]", err, truncateBody(body))
	}

	return &response.Info, nil
}

// fetchTeamInfo calls /team/info to get team-level budget data.
// Returns nil, error on failure — callers treat this as best-effort.
func fetchTeamInfo(c credential, teamID string) (*TeamInfoAPIResponse, error) {
	baseURL := c.baseURL
	if baseURL == "" {
		return nil, fmt.Errorf("no LiteLLM proxy URL configured")
	}
	endpoint := baseURL + "/team/info?team_id=" + url.QueryEscape(teamID)

	client := &http.Client{Timeout: HTTPTimeout}
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("team info HTTP error: status=%d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var response TeamInfoAPIResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// maxErrorBody caps how much of a response body is echoed into error text,
// which is persisted to the budget-fail cache.
const maxErrorBody = 200

// truncateBody trims a response body for inclusion in error messages. Auth
// failures omit the body entirely — LiteLLM's token lookup errors can echo
// token-derived details.
func truncateBody(body []byte) string {
	if len(body) <= maxErrorBody {
		return string(body)
	}
	return string(body[:maxErrorBody]) + "…(truncated)"
}

// parseISOTime parses an ISO 8601 datetime string with timezone support
func parseISOTime(s string) (time.Time, error) {
	// Try common formats with timezone support
	formats := []string{
		time.RFC3339, // "2006-01-02T15:04:05Z07:00"
		"2006-01-02T15:04:05.999999Z07:00",
		"2006-01-02T15:04:05.999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
	}

	for _, format := range formats {
		if t, err := time.Parse(format, s); err == nil {
			return t.UTC(), nil
		}
	}

	return time.Time{}, fmt.Errorf("unable to parse time: %s", s)
}

// normalizeDuration maps named duration aliases to their canonical day/hour form.
func normalizeDuration(duration string) string {
	switch strings.TrimSpace(strings.ToLower(duration)) {
	case "monthly", "1mo":
		return "30d"
	case "weekly":
		return "7d"
	case "daily":
		return "1d"
	default:
		return strings.TrimSpace(strings.ToLower(duration))
	}
}

// calculateNextReset calculates when the budget will next reset.
// Returns now + duration as a rolling window, matching LiteLLM's actual reset behavior.
// Returns zero time if the duration format is unrecognized.
func calculateNextReset(duration string) time.Time {
	normalized := normalizeDuration(duration)
	if d, ok := parseCustomDuration(normalized); ok {
		return time.Now().UTC().Add(d)
	}
	return time.Time{}
}

// formatDuration formats a time.Duration as a human-readable string
func formatDuration(diff time.Duration) string {
	if diff <= 0 {
		return "resetting"
	}

	days := int(diff.Hours()) / 24
	hours := int(diff.Hours()) % 24
	minutes := int(diff.Minutes()) % 60

	if days > 0 {
		return fmt.Sprintf("%dd%dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh", hours)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	return "resetting"
}

// getDurationLabel returns a human-readable label for the budget duration
func getDurationLabel(duration string) string {
	duration = strings.TrimSpace(strings.ToLower(duration))
	switch duration {
	case "30d", "1mo", "monthly":
		return "monthly"
	case "7d", "weekly":
		return "weekly"
	case "1d", "24h", "daily":
		return "daily"
	default:
		return ""
	}
}

// formatTimeUntilReset formats the time remaining until budget reset.
// Returns (timeString, durationLabel). timeString is "unknown" if the duration
// format is present but unrecognized.
func formatTimeUntilReset(resetAt *string, budgetDuration *string) (string, string) {
	now := time.Now().UTC()
	var durationLabel string

	if budgetDuration != nil && *budgetDuration != "" {
		durationLabel = getDurationLabel(*budgetDuration)
	}

	// First try to use budget_reset_at if provided
	if resetAt != nil && *resetAt != "" {
		t, err := parseISOTime(*resetAt)
		if err == nil {
			return formatDuration(t.Sub(now)), durationLabel
		}
	}

	// Fall back to calculating from budget_duration
	if budgetDuration != nil && *budgetDuration != "" {
		nextReset := calculateNextReset(*budgetDuration)
		if !nextReset.IsZero() {
			return formatDuration(nextReset.Sub(now)), durationLabel
		}
		// Duration is set but format is unrecognized — tell the user
		return "unknown", durationLabel
	}

	return "", ""
}

// budgetColor returns the ANSI color code for a budget usage percentage.
func budgetColor(percent float64) string {
	if percent >= 90 {
		return ColorRed
	}
	if percent >= 75 {
		return ColorYellow
	}
	return ColorGreen
}

// circleGlyph returns a Unicode quadrant-fill glyph approximating the given
// usage percentage as a circular gauge.
// Buckets: empty (≤0) · quarter (<30) · half (<60) · three-quarter (<85) · full (≥85).
func circleGlyph(percent float64) string {
	switch {
	case percent <= 0:
		return "○"
	case percent < 30:
		return "◔"
	case percent < 60:
		return "◑"
	case percent < 85:
		return "◕"
	default:
		return "●"
	}
}

// parseCustomDuration parses a duration string like "48h", "2d" into time.Duration
func parseCustomDuration(duration string) (time.Duration, bool) {
	if len(duration) < 2 {
		return 0, false
	}
	suffix := duration[len(duration)-1]
	valueStr := duration[:len(duration)-1]
	var value int
	if _, err := fmt.Sscanf(valueStr, "%d", &value); err != nil {
		return 0, false
	}
	switch suffix {
	case 'd':
		return time.Duration(value) * 24 * time.Hour, true
	case 'h':
		return time.Duration(value) * time.Hour, true
	case 'm':
		return time.Duration(value) * time.Minute, true
	case 's':
		return time.Duration(value) * time.Second, true
	}
	return 0, false
}

// contextColor returns the ANSI color code for a context-window usage percentage.
// Mirrors budgetColor's thresholds but kept as a separate function so the two
// can drift independently if user feedback warrants it.
func contextColor(percent float64) string {
	if percent >= 85 {
		return ColorRed
	}
	if percent >= 70 {
		return ColorYellow
	}
	return ColorGreen
}

// formatContextSegment renders the " | 📖 ● <pct>%[ — suggestion]" segment from
// Claude Code's stdin payload. Returns "" when the context window data is absent
// (no field, null pointer, or pre-first-API-call).
func formatContextSegment(input StatusInput) string {
	if input.ContextWindow == nil || input.ContextWindow.UsedPercentage == nil {
		return ""
	}
	pct := *input.ContextWindow.UsedPercentage
	if pct < 0 {
		pct = 0
	} else if pct > 100 {
		pct = 100
	}
	color := contextColor(pct)
	suggestion := ""
	switch {
	case pct >= 85:
		suggestion = " — run /compact or /clear"
	case pct >= 70:
		suggestion = " — consider /compact"
	}
	return fmt.Sprintf(" %s|%s 📖 %s%s%s %.0f%%%s%s",
		ColorGray, ColorReset, color, circleGlyph(pct), ColorReset, pct, suggestion, ColorReset)
}

// formatStatusLine formats the budget info as a colored status circle with optional
// dollar amounts, reset countdown, and context-window segment.
// latestVersion is the latest GitHub release tag (empty string to skip update notice).
func formatStatusLine(info *KeyInfo, latestVersion string, input StatusInput) string {
	info = resolveEffectiveBudget(info)
	spend := 0.0
	if info.Spend != nil {
		spend = *info.Spend
	}

	updateStr := ""
	if isUpdateAvailable(Version, latestVersion) {
		updateStr = fmt.Sprintf(" %s| update: %s%s", ColorYellow, latestVersion, ColorReset)
	}

	contextStr := formatContextSegment(input)
	prefix := getPrefix(input)

	if info.MaxBudget == nil || *info.MaxBudget <= 0 {
		// No team budget resolved — key-level spend is intentionally not shown as a fallback.
		return formatError("no budget configured", input)
	}

	budget := *info.MaxBudget
	percent := (spend / budget) * 100
	absColor := budgetColor(percent)

	var budgetStr string
	if isShowCostEnabled() {
		budgetStr = fmt.Sprintf("$%.2f/$%.2f (%.0f%%)", spend, budget, percent)
	} else {
		budgetStr = fmt.Sprintf("%.0f%%", percent)
	}

	resetStr := ""
	resetTime, durationLabel := formatTimeUntilReset(info.BudgetResetAt, info.BudgetDuration)
	if resetTime != "" {
		if durationLabel != "" {
			resetStr = fmt.Sprintf(" %s%s reset: %s%s", ColorGray, durationLabel, resetTime, ColorReset)
		} else {
			resetStr = fmt.Sprintf(" %s reset: %s%s", ColorGray, resetTime, ColorReset)
		}
	}

	line := fmt.Sprintf("%s%s%s%s %s%s%s",
		prefix, absColor, circleGlyph(percent), ColorReset, absColor, budgetStr, ColorReset)

	line += resetStr + updateStr + contextStr
	return line
}

// formatError formats an error message with red color
func formatError(msg string, input StatusInput) string {
	return fmt.Sprintf("%s%s%s%s", ColorRed, getPrefix(input), msg, ColorReset)
}

// StatusJSON is the structured output emitted with --json, consumed by the VS Code
// extension (and any other caller that prefers data over ANSI text). All fields are
// optional — absent values are omitted so a minimal payload stays minimal.
//
// Text is the fully-rendered status line (ANSI stripped) — editors forward it
// directly instead of re-implementing the format. Percent is kept alongside so
// editors can apply their own background-color theming (which can't be forwarded).
type StatusJSON struct {
	Prefix          string  `json:"prefix,omitempty"`
	Text            string  `json:"text"`
	Percent         float64 `json:"percent"`
	Spend           float64 `json:"spend,omitempty"`
	MaxBudget       float64 `json:"max_budget,omitempty"`
	HasBudget       bool    `json:"has_budget"`
	ResetLabel      string  `json:"reset_label,omitempty"`
	ResetTime       string  `json:"reset_time,omitempty"`
	UpdateAvailable string  `json:"update_available,omitempty"`
	ContextPercent  float64 `json:"context_percent,omitempty"`
	HasContext      bool    `json:"has_context"`
	Error           string  `json:"error,omitempty"`
}

// stripANSI removes all ANSI escape sequences from s, leaving plain text.
func stripANSI(s string) string {
	for _, c := range []string{ColorRed, ColorYellow, ColorGreen, ColorGray, ColorReset} {
		s = strings.ReplaceAll(s, c, "")
	}
	return s
}

// renderLine produces the fully-rendered status line (with ANSI color) for the
// current state. Both the stdout path (main) and the --json Text field use this,
// so the two output modes can never drift. buildStatusJSON strips the ANSI for JSON.
func renderLine(info *KeyInfo, latestVersion string, input StatusInput, err error) string {
	if err != nil {
		switch {
		case errors.Is(err, ErrBudgetExceeded):
			var bErr *BudgetExceededError
			if errors.As(err, &bErr) && bErr.MaxBudget > 0 {
				pct := (bErr.Spend / bErr.MaxBudget) * 100
				return fmt.Sprintf("%s%s$%.2f/$%.2f (%.0f%%) | Budget exceeded%s",
					ColorRed, getPrefix(input), bErr.Spend, bErr.MaxBudget, pct, ColorReset)
			}
			return formatError("Budget exceeded", input)
		case errors.Is(err, ErrAuth):
			return formatError("Auth error", input)
		case strings.Contains(err.Error(), "timeout") ||
			strings.Contains(err.Error(), "connection") ||
			strings.Contains(err.Error(), "dial"):
			return formatError("Connection error", input)
		default:
			if errors.Is(err, ErrNoAPIKey) {
				return formatError("No API key", input)
			}
			return formatError("Error", input)
		}
	}
	if info == nil {
		return formatError("Error", input)
	}
	return formatStatusLine(info, latestVersion, input)
}

// buildStatusJSON gathers the same data as the ANSI path but returns a structured
// StatusJSON. info may be nil (fetch failed); err carries the reason. The caller
// decides whether to render the ANSI line or emit this struct.
func buildStatusJSON(info *KeyInfo, latestVersion string, input StatusInput, err error) StatusJSON {
	out := StatusJSON{Prefix: strings.TrimSpace(getPrefix(input))}
	out.Text = stripANSI(renderLine(info, latestVersion, input, err))

	if input.ContextWindow != nil && input.ContextWindow.UsedPercentage != nil {
		pct := *input.ContextWindow.UsedPercentage
		if pct < 0 {
			pct = 0
		} else if pct > 100 {
			pct = 100
		}
		out.HasContext = true
		out.ContextPercent = pct
	}

	if isUpdateAvailable(Version, latestVersion) {
		out.UpdateAvailable = latestVersion
	}

	if err != nil {
		switch {
		case errors.Is(err, ErrBudgetExceeded):
			var bErr *BudgetExceededError
			if errors.As(err, &bErr) && bErr.MaxBudget > 0 {
				out.HasBudget = true
				out.Spend = bErr.Spend
				out.MaxBudget = bErr.MaxBudget
				out.Percent = (bErr.Spend / bErr.MaxBudget) * 100
			}
			out.Error = "budget exceeded"
		case errors.Is(err, ErrAuth):
			out.Error = "auth error"
		case errors.Is(err, ErrNoAPIKey):
			out.Error = "no api key"
		case strings.Contains(err.Error(), "timeout") ||
			strings.Contains(err.Error(), "connection") ||
			strings.Contains(err.Error(), "dial"):
			out.Error = "connection error"
		default:
			out.Error = "error"
		}
		return out
	}

	if info == nil {
		out.Error = "error"
		return out
	}

	info = resolveEffectiveBudget(info)
	if info.MaxBudget == nil || *info.MaxBudget <= 0 {
		out.Error = "no budget configured"
		return out
	}

	out.HasBudget = true
	out.MaxBudget = *info.MaxBudget
	if info.Spend != nil {
		out.Spend = *info.Spend
	}
	out.Percent = (out.Spend / out.MaxBudget) * 100

	resetTime, durationLabel := formatTimeUntilReset(info.BudgetResetAt, info.BudgetDuration)
	out.ResetTime = resetTime
	out.ResetLabel = durationLabel

	return out
}

func main() {
	args := os.Args[1:]

	if len(args) > 0 && (args[0] == "--version" || args[0] == "-v") {
		fmt.Println(Version)
		return
	}

	jsonMode := len(args) > 0 && args[0] == "--json"

	input := readStatusInput(os.Stdin)

	cred := resolveCredential()
	if cred.token == "" {
		noKey := fmt.Errorf("%w", ErrNoAPIKey)
		if jsonMode {
			emitJSON(buildStatusJSON(nil, "", input, noKey))
			return
		}
		fmt.Println(renderLine(nil, "", input, noKey))
		return
	}

	info, err := budgetInfo(cred)
	latestVersion := getLatestVersion()

	if jsonMode {
		emitJSON(buildStatusJSON(info, latestVersion, input, err))
		return
	}

	if err != nil || info == nil {
		fmt.Println(renderLine(info, latestVersion, input, err))
		return
	}

	fmt.Println(renderLine(info, latestVersion, input, nil))
}

// emitJSON marshals out to stdout. A failure to marshal would indicate a programming
// error (nil pointers on the struct fields can't happen), so it panics — the binary
// should never produce unparseable JSON in --json mode.
func emitJSON(out StatusJSON) {
	data, err := json.Marshal(out)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(data))
}
