package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
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
}

// GitHubRelease represents the GitHub releases API response
type GitHubRelease struct {
	TagName string `json:"tag_name"`
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

func budgetCacheFile() string {
	return filepath.Join(cacheDir(), "budget.json")
}

func updateCacheFile() string {
	return filepath.Join(cacheDir(), "update.json")
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
	_ = os.WriteFile(budgetCacheFile(), data, 0o600)
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
// Errors are silently ignored — caching is best-effort.
func writeUpdateCache(version string) {
	if version == "" {
		return
	}
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
	_ = os.WriteFile(updateCacheFile(), data, 0o600)
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
	if latest != "" {
		writeUpdateCache(latest)
	}
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

// isBetaEnabled returns true if beta features are enabled via LITELLM_PLUGIN_BETA_FEATURES environment variable
func isBetaEnabled() bool {
	val := os.Getenv("LITELLM_PLUGIN_BETA_FEATURES")
	return val == "1" || val == "true"
}

// getKeyInfo fetches budget info from the LiteLLM API, using a 30-second filesystem cache
// to avoid hitting the API on every statusline refresh.
// Each invocation of this binary is a fresh process, so all state must live on disk.
func getKeyInfo(apiKey string) (*KeyInfo, error) {
	if info, ok := readBudgetCache(); ok {
		return info, nil
	}
	info, err := fetchKeyInfo(apiKey)
	if err != nil {
		return nil, err
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

// calculateBudgetPeriod returns the start and end of the current budget period.
// Requires BudgetDuration to determine period length.
// If BudgetResetAt is set, it overrides the calculated period end.
func calculateBudgetPeriod(keyInfo *KeyInfo) (time.Time, time.Time, bool) {
	if keyInfo.BudgetDuration == nil || *keyInfo.BudgetDuration == "" {
		return time.Time{}, time.Time{}, false
	}

	normalized := normalizeDuration(*keyInfo.BudgetDuration)
	d, ok := parseCustomDuration(normalized)
	if !ok {
		return time.Time{}, time.Time{}, false
	}

	// Determine period end
	var periodEnd time.Time
	if keyInfo.BudgetResetAt != nil && *keyInfo.BudgetResetAt != "" {
		if t, err := parseISOTime(*keyInfo.BudgetResetAt); err == nil {
			periodEnd = t
		}
	}
	if periodEnd.IsZero() {
		periodEnd = time.Now().UTC().Add(d)
	}

	periodStart := periodEnd.Add(-d)
	return periodStart, periodEnd, true
}

// calculateElapsedFraction returns what fraction of the current budget period has elapsed (0.0–1.0).
func calculateElapsedFraction(keyInfo *KeyInfo) (float64, bool) {
	periodStart, periodEnd, ok := calculateBudgetPeriod(keyInfo)
	if !ok {
		return 0, false
	}

	now := time.Now().UTC()
	total := periodEnd.Sub(periodStart)
	elapsed := now.Sub(periodStart)

	if total <= 0 {
		return 0, false
	}

	fraction := elapsed.Seconds() / total.Seconds()
	// Clamp to [0.0, 1.0] to handle edge cases like clock skew
	if fraction < 0 {
		fraction = 0
	} else if fraction > 1.0 {
		fraction = 1.0
	}
	return fraction, true
}

// renderProgressBar renders a 20-character progress bar with optional pace marker.
// Fill color is based on absolute spend thresholds.
// Marker color is based on projected end-of-period usage.
func renderProgressBar(percent float64, elapsedFraction float64, hasTimeInfo bool) string {
	const barWidth = 20

	fillWidth := int(math.Round(percent / 100.0 * float64(barWidth)))
	if fillWidth > barWidth {
		fillWidth = barWidth
	}
	if fillWidth < 0 {
		fillWidth = 0
	}

	markerPos := -1
	if hasTimeInfo {
		markerPos = int(math.Round(elapsedFraction * float64(barWidth)))
		if markerPos >= barWidth {
			markerPos = barWidth - 1
		}
		if markerPos < 0 {
			markerPos = 0
		}
	}

	// Absolute color for fill
	absColor := budgetColor(percent)

	// Pace color for marker
	paceColor := absColor
	if hasTimeInfo && elapsedFraction > 0.03 && elapsedFraction < 1.0 {
		projected := percent / elapsedFraction
		if projected > 100 {
			paceColor = ColorRed
		} else if projected >= 75 {
			paceColor = ColorYellow
		} else {
			paceColor = ColorGreen
		}
	}

	var buf strings.Builder
	buf.WriteString("[")

	lastColor := ""
	for i := 0; i < barWidth; i++ {
		var color, char string
		if i == markerPos {
			color, char = paceColor, "│"
		} else if i < fillWidth {
			color, char = absColor, "█"
		} else {
			color, char = ColorGray, "░"
		}
		if color != lastColor {
			buf.WriteString(color)
			lastColor = color
		}
		buf.WriteString(char)
	}

	buf.WriteString(ColorReset)
	buf.WriteString("]")
	return buf.String()
}

// formatStatusLine formats the budget info as a colored progress bar.
// latestVersion is the latest GitHub release tag (empty string to skip update notice).
func formatStatusLine(info *KeyInfo, latestVersion string) string {
	spend := 0.0
	if info.Spend != nil {
		spend = *info.Spend
	}

	updateStr := ""
	if isUpdateAvailable(Version, latestVersion) {
		updateStr = fmt.Sprintf(" %s| update: %s%s", ColorYellow, latestVersion, ColorReset)
	}

	if info.MaxBudget == nil || *info.MaxBudget <= 0 {
		// No budget set — just show spend
		return fmt.Sprintf("%sLiteLLM: $%.2f%s%s", ColorGreen, spend, ColorReset, updateStr)
	}

	budget := *info.MaxBudget
	percent := (spend / budget) * 100

	// Absolute color for text
	absColor := budgetColor(percent)

	budgetStr := fmt.Sprintf("$%.2f/$%.2f", spend, budget)

	// Reset info
	resetStr := ""
	resetTime, durationLabel := formatTimeUntilReset(info.BudgetResetAt, info.BudgetDuration)
	if resetTime != "" {
		if durationLabel != "" {
			resetStr = fmt.Sprintf(" %s%s reset: %s%s", ColorGray, durationLabel, resetTime, ColorReset)
		} else {
			resetStr = fmt.Sprintf(" %s reset: %s%s", ColorGray, resetTime, ColorReset)
		}
	}

	line := fmt.Sprintf("%sLiteLLM: %s (%.0f%%)%s", absColor, budgetStr, percent, ColorReset)

	if isBetaEnabled() {
		elapsedFraction, hasTimeInfo := calculateElapsedFraction(info)
		progressBar := renderProgressBar(percent, elapsedFraction, hasTimeInfo)
		line += " " + progressBar
	}

	line += updateStr + resetStr

	return line
}

// formatError formats an error message with red color
func formatError(msg string) string {
	return fmt.Sprintf("%sLiteLLM: %s%s", ColorRed, msg, ColorReset)
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println(Version)
		return
	}

	// Consume stdin (Claude Code sends session data, but we don't use it)
	_, _ = io.ReadAll(os.Stdin)

	token := getToken()
	if token == "" {
		fmt.Println(formatError("No API key"))
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
				fmt.Printf("%sLiteLLM: $%.2f/$%.2f (%.0f%%) | Budget exceeded%s\n",
					ColorRed, bErr.Spend, bErr.MaxBudget, percent, ColorReset)
			} else {
				fmt.Println(formatError("Budget exceeded"))
			}
		case errors.Is(err, ErrAuth):
			if debug {
				fmt.Println(formatError("Auth error: " + err.Error()))
			} else {
				fmt.Println(formatError("Auth error"))
			}
		case strings.Contains(err.Error(), "timeout") ||
			strings.Contains(err.Error(), "connection") ||
			strings.Contains(err.Error(), "dial"):
			if debug {
				fmt.Println(formatError("Connection error: " + err.Error()))
			} else {
				fmt.Println(formatError("Connection error"))
			}
		default:
			if debug {
				fmt.Println(formatError("Error: " + err.Error()))
			} else {
				fmt.Println(formatError("Error"))
			}
		}
		return
	}

	latestVersion := getLatestVersion()
	fmt.Println(formatStatusLine(info, latestVersion))
}
