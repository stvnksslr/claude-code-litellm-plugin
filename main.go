package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
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
	CacheTTLMs         = 30_000 // 30 seconds in milliseconds
	HTTPTimeout        = 10 * time.Second
	MaxRetries         = 3
	InitialBackoffMs   = 1_000           // 1 second
	CooldownMs         = 5 * 60 * 1_000  // 5 minutes in milliseconds
	UpdateCheckTTLMs   = 60 * 60 * 1_000 // 1 hour in milliseconds
	UpdateCheckTimeout = 5 * time.Second
)

// Cache for budget info
var (
	cachedBudgetInfo *KeyInfo
	cacheTimestamp   int64
	cooldownUntil    int64
	cacheMutex       sync.Mutex
)

// Cache for update check
var (
	cachedLatestVersion  string
	updateCacheTimestamp int64
	updateMutex          sync.Mutex
)

// resetCache clears all cache state (exported for testing)
func resetCache() {
	cacheMutex.Lock()
	defer cacheMutex.Unlock()
	cachedBudgetInfo = nil
	cacheTimestamp = 0
	cooldownUntil = 0
}

// resetUpdateCache clears the update check cache (for testing)
func resetUpdateCache() {
	updateMutex.Lock()
	defer updateMutex.Unlock()
	cachedLatestVersion = ""
	updateCacheTimestamp = 0
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

// getLatestVersion returns the latest GitHub release tag, using a 1-hour cache
func getLatestVersion() string {
	updateMutex.Lock()
	defer updateMutex.Unlock()

	now := time.Now().UnixMilli()
	if cachedLatestVersion != "" && (now-updateCacheTimestamp) < UpdateCheckTTLMs {
		return cachedLatestVersion
	}

	latest := fetchLatestVersion()
	if latest != "" {
		cachedLatestVersion = latest
		updateCacheTimestamp = now
	}
	return cachedLatestVersion
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

// getKeyInfo fetches budget info from the LiteLLM API with caching and exponential backoff
func getKeyInfo(apiKey string) (*KeyInfo, error) {
	cacheMutex.Lock()
	defer cacheMutex.Unlock()

	now := time.Now().UnixMilli()

	// Check if we're in cooldown period
	if cooldownUntil > 0 && now < cooldownUntil {
		return nil, fmt.Errorf("cooldown")
	}

	// Return cached data if still valid
	if cachedBudgetInfo != nil && (now-cacheTimestamp) < CacheTTLMs {
		return cachedBudgetInfo, nil
	}

	// Try to fetch with exponential backoff
	var lastErr error
	for attempt := 0; attempt <= MaxRetries; attempt++ {
		if attempt > 0 {
			// Calculate exponential backoff delay
			backoffMs := InitialBackoffMs * (1 << (attempt - 1)) // 1s, 2s, 4s
			time.Sleep(time.Duration(backoffMs) * time.Millisecond)
		}

		info, err := fetchKeyInfo(apiKey)
		if err == nil {
			// Success - cache result and clear cooldown
			cachedBudgetInfo = info
			cacheTimestamp = now
			cooldownUntil = 0
			return info, nil
		}

		lastErr = err
	}

	// All retries failed - enter cooldown
	cooldownUntil = now + CooldownMs
	return nil, lastErr
}

// fetchKeyInfo makes the actual API call
func fetchKeyInfo(apiKey string) (*KeyInfo, error) {
	baseURL := getBaseURL()
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
		return nil, fmt.Errorf("auth error: status=%d url=%s body=%s", resp.StatusCode, url, string(body))
	}

	if resp.StatusCode != 200 {
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

// calculateNextReset calculates the next reset time based on budget_duration
// Supports formats like "30d", "7d", "1d", "24h", etc.
// Returns the next reset time according to LiteLLM's reset rules:
// - Daily (24h/1d): Reset at midnight every day
// - Weekly (7d): Reset on Monday at midnight
// - Monthly (30d): Reset on the 1st of each month at midnight
func calculateNextReset(duration string) time.Time {
	now := time.Now().UTC()

	// Parse duration string (e.g., "30d", "7d", "1d", "24h")
	duration = strings.TrimSpace(strings.ToLower(duration))

	switch duration {
	case "30d", "1mo", "monthly":
		// Monthly: Reset on 1st of next month at midnight
		year, month, _ := now.Date()
		nextMonth := month + 1
		nextYear := year
		if nextMonth > 12 {
			nextMonth = 1
			nextYear++
		}
		return time.Date(nextYear, nextMonth, 1, 0, 0, 0, 0, time.UTC)

	case "7d", "weekly":
		// Weekly: Reset on next Monday at midnight
		daysUntilMonday := (8 - int(now.Weekday())) % 7
		if daysUntilMonday == 0 {
			daysUntilMonday = 7 // If today is Monday, next reset is next Monday
		}
		nextMonday := now.AddDate(0, 0, daysUntilMonday)
		return time.Date(nextMonday.Year(), nextMonday.Month(), nextMonday.Day(), 0, 0, 0, 0, time.UTC)

	case "1d", "24h", "daily":
		// Daily: Reset at next midnight
		tomorrow := now.AddDate(0, 0, 1)
		return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 0, 0, 0, 0, time.UTC)

	default:
		// Try to parse as a custom duration (e.g., "48h", "2d")
		if d, ok := parseCustomDuration(duration); ok {
			return now.Add(d)
		}
		// Fallback: return zero time (will show "unknown")
		return time.Time{}
	}
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

// formatTimeUntilReset formats the time remaining until budget reset
// Returns (timeString, durationLabel)
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

	duration := strings.TrimSpace(strings.ToLower(*keyInfo.BudgetDuration))

	// Determine period end
	var periodEnd time.Time
	if keyInfo.BudgetResetAt != nil && *keyInfo.BudgetResetAt != "" {
		if t, err := parseISOTime(*keyInfo.BudgetResetAt); err == nil {
			periodEnd = t
		}
	}
	if periodEnd.IsZero() {
		periodEnd = calculateNextReset(*keyInfo.BudgetDuration)
		if periodEnd.IsZero() {
			return time.Time{}, time.Time{}, false
		}
	}

	// Determine period start based on duration type
	var periodStart time.Time
	switch duration {
	case "30d", "1mo", "monthly":
		periodStart = periodEnd.AddDate(0, -1, 0)
	case "7d", "weekly":
		periodStart = periodEnd.AddDate(0, 0, -7)
	case "1d", "24h", "daily":
		periodStart = periodEnd.AddDate(0, 0, -1)
	default:
		if d, ok := parseCustomDuration(duration); ok {
			periodStart = periodEnd.Add(-d)
		} else {
			return time.Time{}, time.Time{}, false
		}
	}

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
	// Consume stdin (Claude Code sends session data, but we don't use it)
	_, _ = io.ReadAll(os.Stdin)

	token := getToken()
	if token == "" {
		fmt.Println(formatError("No API key"))
		return
	}

	info, err := getKeyInfo(token)
	if err != nil {
		errStr := err.Error()
		debug := isDebug()

		if strings.Contains(errStr, "cooldown") {
			fmt.Println(formatError("Cooldown (retrying in 5m)"))
		} else if strings.Contains(errStr, "auth error") {
			if debug {
				fmt.Println(formatError("Auth error: " + errStr))
			} else {
				fmt.Println(formatError("Auth error"))
			}
		} else if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "connection") || strings.Contains(errStr, "dial") {
			if debug {
				fmt.Println(formatError("Connection error: " + errStr))
			} else {
				fmt.Println(formatError("Connection error"))
			}
		} else {
			if debug {
				fmt.Println(formatError("Error: " + errStr))
			} else {
				fmt.Println(formatError("Error"))
			}
		}
		return
	}

	latestVersion := getLatestVersion()
	fmt.Println(formatStatusLine(info, latestVersion))
}
