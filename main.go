package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

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
	CacheTTLMs       = 30_000 // 30 seconds in milliseconds
	HTTPTimeout      = 10 * time.Second
	MaxRetries       = 3
	InitialBackoffMs = 1_000          // 1 second
	CooldownMs       = 5 * 60 * 1_000 // 5 minutes in milliseconds
)

// Cache for budget info
var (
	cachedBudgetInfo *KeyInfo
	cacheTimestamp   int64
	cooldownUntil    int64
	cacheMutex       sync.Mutex
)

// resetCache clears all cache state (exported for testing)
func resetCache() {
	cacheMutex.Lock()
	defer cacheMutex.Unlock()
	cachedBudgetInfo = nil
	cacheTimestamp = 0
	cooldownUntil = 0
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
		// Try to parse as a duration with suffix (e.g., "48h", "2d")
		if len(duration) > 1 {
			suffix := duration[len(duration)-1]
			valueStr := duration[:len(duration)-1]
			var value int
			if _, err := fmt.Sscanf(valueStr, "%d", &value); err == nil {
				switch suffix {
				case 'd':
					return now.AddDate(0, 0, value)
				case 'h':
					return now.Add(time.Duration(value) * time.Hour)
				case 'm':
					return now.Add(time.Duration(value) * time.Minute)
				case 's':
					return now.Add(time.Duration(value) * time.Second)
				}
			}
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

// formatStatusLine formats the budget info as a colored status line
func formatStatusLine(info *KeyInfo) string {
	spend := 0.0
	if info.Spend != nil {
		spend = *info.Spend
	}

	var color string
	var budgetStr string
	var percentStr string

	if info.MaxBudget != nil && *info.MaxBudget > 0 {
		budget := *info.MaxBudget
		percent := (spend / budget) * 100

		if percent >= 90 {
			color = ColorRed
		} else if percent >= 75 {
			color = ColorYellow
		} else {
			color = ColorGreen
		}

		budgetStr = fmt.Sprintf("$%.2f/$%.2f", spend, budget)
		percentStr = fmt.Sprintf(" (%.0f%%)", percent)
	} else {
		color = ColorGreen
		budgetStr = fmt.Sprintf("$%.2f", spend)
		percentStr = ""
	}

	resetStr := ""
	resetTime, durationLabel := formatTimeUntilReset(info.BudgetResetAt, info.BudgetDuration)
	if resetTime != "" {
		if durationLabel != "" {
			resetStr = fmt.Sprintf(" %s| %s reset: %s%s", ColorGray, durationLabel, resetTime, ColorReset)
		} else {
			resetStr = fmt.Sprintf(" %s| reset: %s%s", ColorGray, resetTime, ColorReset)
		}
	}

	return fmt.Sprintf("%sLiteLLM: %s%s%s%s", color, budgetStr, percentStr, ColorReset, resetStr)
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

	fmt.Println(formatStatusLine(info))
}
