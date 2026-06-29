package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

// cacheKey returns a short, stable hash of the active base URL + token so budget
// cache files don't bleed across different proxies/keys (e.g. per-project configs
// that point at different LiteLLM instances or use different keys).
func cacheKey() string {
	sum := sha256.Sum256([]byte(getBaseURL() + "\x00" + getToken()))
	return hex.EncodeToString(sum[:])[:12]
}

func budgetCacheFile() string {
	return filepath.Join(cacheDir(), "budget-"+cacheKey()+".json")
}

// budgetFailCacheFile holds the negative-cache marker for failed budget fetches.
// Namespaced per-key so a failure for one key doesn't suppress fetches for another.
func budgetFailCacheFile() string {
	return filepath.Join(cacheDir(), "budget-fail-"+cacheKey()+".json")
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
func readBudgetCache() (*KeyInfo, bool) {
	data, err := os.ReadFile(budgetCacheFile())
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
func writeBudgetCache(info *KeyInfo) {
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
	_ = writeFileAtomic(budgetCacheFile(), data, 0o600)
}

// readBudgetFailCache returns a recent failed-fetch record, if one exists within
// BudgetFailTTLMs. Returns nil, false when absent, corrupt, or expired.
func readBudgetFailCache() (*BudgetFailEntry, bool) {
	data, err := os.ReadFile(budgetFailCacheFile())
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
func writeBudgetFailCache(fetchErr error) {
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
	_ = writeFileAtomic(budgetFailCacheFile(), data, 0o600)
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

// getToken returns the API token from environment
func getToken() string {
	return getEnvWithFallback("LITELLM_PROXY_API_KEY", "ANTHROPIC_AUTH_TOKEN")
}

// isDebug returns true if debug mode is enabled via LITELLM_DEBUG environment variable
func isDebug() bool {
	val := os.Getenv("LITELLM_DEBUG")
	return val == "1" || val == "true"
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

// getKeyInfo fetches budget info from the LiteLLM API, using a 30-second filesystem cache
// to avoid hitting the API on every statusline refresh.
// Each invocation of this binary is a fresh process, so all state must live on disk.
// When the key has a team_id, a second call to /team/info populates the team budget
// fields — the only budget the statusline displays (key-level budget is ignored).
func getKeyInfo(apiKey string) (*KeyInfo, error) {
	if info, ok := readBudgetCache(); ok {
		return info, nil
	}
	// Recent failure → back off and replay the cached error instead of re-blocking
	// on the network every refresh while the proxy is down / key is bad / over budget.
	if failed, ok := readBudgetFailCache(); ok {
		return nil, errorFromFailEntry(failed)
	}
	info, err := fetchKeyInfo(apiKey)
	if err != nil {
		writeBudgetFailCache(err)
		return nil, err
	}
	if info.TeamID != nil && *info.TeamID != "" {
		if teamResp, err := fetchTeamInfo(apiKey, *info.TeamID); err == nil {
			ti := teamResp.TeamInfo
			// Primary source: this member's own per-member budget from team_memberships.
			// Both the budget and its matching spend come from the same membership row;
			// the key's own spend is never used (it tracks a different, confusing window).
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
			// Fallback for instances that expose the budget at the team level instead:
			// team_member_budget_table, else the team's own max_budget. Spend is paired
			// with the team total — again, never the key's spend.
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
	}
	writeBudgetCache(info)
	return info, nil
}

// fetchKeyInfo makes the actual API call
func fetchKeyInfo(apiKey string) (*KeyInfo, error) {
	baseURL := getBaseURL()
	if baseURL == "" {
		return nil, fmt.Errorf("no LiteLLM proxy URL configured (set LITELLM_PROXY_URL or ANTHROPIC_BASE_URL)")
	}
	url := baseURL + "/key/info"

	client := &http.Client{Timeout: HTTPTimeout}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("request creation failed: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
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
		return nil, fmt.Errorf("status=%d url=%s body=%s: %w", resp.StatusCode, url, string(body), ErrAuth)
	}

	if resp.StatusCode != 200 {
		var litellmErr liteLLMError
		if json.Unmarshal(body, &litellmErr) == nil && litellmErr.Error.Type == "budget_exceeded" {
			bErr := &BudgetExceededError{}
			_, _ = fmt.Sscanf(litellmErr.Error.Message, "Budget has been exceeded! Current cost: %f, Max budget: %f", &bErr.Spend, &bErr.MaxBudget)
			return nil, bErr
		}
		return nil, fmt.Errorf("HTTP error: status=%d url=%s body=%s", resp.StatusCode, url, string(body))
	}

	var response KeyInfoResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("JSON parse error: %w [body=%s]", err, string(body))
	}

	return &response.Info, nil
}

// fetchTeamInfo calls /team/info to get team-level budget data.
// Returns nil, error on failure — callers treat this as best-effort.
func fetchTeamInfo(apiKey, teamID string) (*TeamInfoAPIResponse, error) {
	baseURL := getBaseURL()
	if baseURL == "" {
		return nil, fmt.Errorf("no LiteLLM proxy URL configured")
	}
	endpoint := baseURL + "/team/info?team_id=" + url.QueryEscape(teamID)

	client := &http.Client{Timeout: HTTPTimeout}
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
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

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println(Version)
		return
	}

	input := readStatusInput(os.Stdin)

	token := getToken()
	if token == "" {
		fmt.Println(formatError("No API key", input))
		return
	}

	info, err := getKeyInfo(token)
	if err != nil {
		debug := isDebug()
		switch {
		case errors.Is(err, ErrBudgetExceeded):
			var bErr *BudgetExceededError
			if errors.As(err, &bErr) && bErr.MaxBudget > 0 {
				percent := (bErr.Spend / bErr.MaxBudget) * 100
				fmt.Printf("%s%s$%.2f/$%.2f (%.0f%%) | Budget exceeded%s\n",
					ColorRed, getPrefix(input), bErr.Spend, bErr.MaxBudget, percent, ColorReset)
			} else {
				fmt.Println(formatError("Budget exceeded", input))
			}
		case errors.Is(err, ErrAuth):
			if debug {
				fmt.Println(formatError("Auth error: "+err.Error(), input))
			} else {
				fmt.Println(formatError("Auth error", input))
			}
		case strings.Contains(err.Error(), "timeout") ||
			strings.Contains(err.Error(), "connection") ||
			strings.Contains(err.Error(), "dial"):
			if debug {
				fmt.Println(formatError("Connection error: "+err.Error(), input))
			} else {
				fmt.Println(formatError("Connection error", input))
			}
		default:
			if debug {
				fmt.Println(formatError("Error: "+err.Error(), input))
			} else {
				fmt.Println(formatError("Error", input))
			}
		}
		return
	}

	latestVersion := getLatestVersion()
	fmt.Println(formatStatusLine(info, latestVersion, input))
}
