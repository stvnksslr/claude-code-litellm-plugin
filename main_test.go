package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func strPtr(s string) *string {
	return &s
}

func TestFormatTimeUntilReset(t *testing.T) {
	tests := []struct {
		name           string
		resetAt        *string
		budgetDuration *string
		expectedTime   string
		expectedLabel  string
	}{
		{
			name:           "nil resetAt and nil duration",
			resetAt:        nil,
			budgetDuration: nil,
			expectedTime:   "",
			expectedLabel:  "",
		},
		{
			name:           "empty string resetAt",
			resetAt:        strPtr(""),
			budgetDuration: nil,
			expectedTime:   "",
			expectedLabel:  "",
		},
		{
			name:           "invalid format",
			resetAt:        strPtr("not-a-date"),
			budgetDuration: nil,
			expectedTime:   "",
			expectedLabel:  "",
		},
		{
			name:           "past time",
			resetAt:        strPtr("2020-01-01T00:00:00Z"),
			budgetDuration: nil,
			expectedTime:   "resetting",
			expectedLabel:  "",
		},
		{
			name:           "with monthly duration label",
			resetAt:        strPtr("2020-01-01T00:00:00Z"),
			budgetDuration: strPtr("30d"),
			expectedTime:   "resetting",
			expectedLabel:  "monthly",
		},
		{
			name:           "with weekly duration label",
			resetAt:        strPtr("2020-01-01T00:00:00Z"),
			budgetDuration: strPtr("7d"),
			expectedTime:   "resetting",
			expectedLabel:  "weekly",
		},
		{
			name:           "with daily duration label",
			resetAt:        strPtr("2020-01-01T00:00:00Z"),
			budgetDuration: strPtr("1d"),
			expectedTime:   "resetting",
			expectedLabel:  "daily",
		},
		{
			name:           "unknown duration format shows unknown",
			resetAt:        nil,
			budgetDuration: strPtr("badformat"),
			expectedTime:   "unknown",
			expectedLabel:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resultTime, resultLabel := formatTimeUntilReset(tt.resetAt, tt.budgetDuration)
			if resultTime != tt.expectedTime {
				t.Errorf("formatTimeUntilReset time = %q, want %q", resultTime, tt.expectedTime)
			}
			if resultLabel != tt.expectedLabel {
				t.Errorf("formatTimeUntilReset label = %q, want %q", resultLabel, tt.expectedLabel)
			}
		})
	}

	// Test future times (dynamic)
	t.Run("future time with days", func(t *testing.T) {
		future := time.Now().UTC().Add(50 * time.Hour)
		resetAt := future.Format("2006-01-02T15:04:05Z")
		result, _ := formatTimeUntilReset(&resetAt, nil)
		if !strings.Contains(result, "d") {
			t.Errorf("expected result to contain 'd' for days, got %q", result)
		}
	})

	t.Run("future time with hours only", func(t *testing.T) {
		future := time.Now().UTC().Add(5 * time.Hour)
		resetAt := future.Format("2006-01-02T15:04:05Z")
		result, _ := formatTimeUntilReset(&resetAt, nil)
		if !strings.Contains(result, "h") {
			t.Errorf("expected result to contain 'h' for hours, got %q", result)
		}
	})

	t.Run("future time with minutes only", func(t *testing.T) {
		future := time.Now().UTC().Add(30 * time.Minute)
		resetAt := future.Format("2006-01-02T15:04:05Z")
		result, _ := formatTimeUntilReset(&resetAt, nil)
		if !strings.Contains(result, "m") {
			t.Errorf("expected result to contain 'm' for minutes, got %q", result)
		}
	})

	// Test budget_duration fallback
	t.Run("daily duration fallback", func(t *testing.T) {
		result, label := formatTimeUntilReset(nil, strPtr("1d"))
		if result == "" {
			t.Error("expected non-empty result for daily duration")
		}
		if label != "daily" {
			t.Errorf("expected label 'daily', got %q", label)
		}
	})

	t.Run("weekly duration fallback", func(t *testing.T) {
		result, label := formatTimeUntilReset(nil, strPtr("7d"))
		if result == "" {
			t.Error("expected non-empty result for weekly duration")
		}
		if label != "weekly" {
			t.Errorf("expected label 'weekly', got %q", label)
		}
	})

	t.Run("monthly duration fallback", func(t *testing.T) {
		result, label := formatTimeUntilReset(nil, strPtr("30d"))
		if result == "" {
			t.Error("expected non-empty result for monthly duration")
		}
		if label != "monthly" {
			t.Errorf("expected label 'monthly', got %q", label)
		}
	})
}

func TestCalculateNextReset(t *testing.T) {
	now := time.Now().UTC()
	tolerance := 2 * time.Second

	t.Run("daily reset (1d)", func(t *testing.T) {
		result := calculateNextReset("1d")
		expected := now.Add(24 * time.Hour)
		if diff := result.Sub(expected); diff < -tolerance || diff > tolerance {
			t.Errorf("calculateNextReset(1d) = %v, want approximately %v", result, expected)
		}
	})

	t.Run("daily reset (24h)", func(t *testing.T) {
		result := calculateNextReset("24h")
		expected := now.Add(24 * time.Hour)
		if diff := result.Sub(expected); diff < -tolerance || diff > tolerance {
			t.Errorf("calculateNextReset(24h) = %v, want approximately %v", result, expected)
		}
	})

	t.Run("daily alias", func(t *testing.T) {
		result := calculateNextReset("daily")
		expected := now.Add(24 * time.Hour)
		if diff := result.Sub(expected); diff < -tolerance || diff > tolerance {
			t.Errorf("calculateNextReset(daily) = %v, want approximately %v", result, expected)
		}
	})

	t.Run("weekly reset (7d)", func(t *testing.T) {
		result := calculateNextReset("7d")
		expected := now.Add(7 * 24 * time.Hour)
		if diff := result.Sub(expected); diff < -tolerance || diff > tolerance {
			t.Errorf("calculateNextReset(7d) = %v, want approximately %v", result, expected)
		}
		if result.Before(now) {
			t.Errorf("calculateNextReset(7d) = %v, should be in the future", result)
		}
	})

	t.Run("weekly alias", func(t *testing.T) {
		result := calculateNextReset("weekly")
		expected := now.Add(7 * 24 * time.Hour)
		if diff := result.Sub(expected); diff < -tolerance || diff > tolerance {
			t.Errorf("calculateNextReset(weekly) = %v, want approximately %v", result, expected)
		}
	})

	t.Run("monthly reset (30d)", func(t *testing.T) {
		result := calculateNextReset("30d")
		expected := now.Add(30 * 24 * time.Hour)
		if diff := result.Sub(expected); diff < -tolerance || diff > tolerance {
			t.Errorf("calculateNextReset(30d) = %v, want approximately %v", result, expected)
		}
		if result.Before(now) {
			t.Errorf("calculateNextReset(30d) = %v, should be in the future", result)
		}
	})

	t.Run("monthly alias", func(t *testing.T) {
		result := calculateNextReset("monthly")
		expected := now.Add(30 * 24 * time.Hour)
		if diff := result.Sub(expected); diff < -tolerance || diff > tolerance {
			t.Errorf("calculateNextReset(monthly) = %v, want approximately %v", result, expected)
		}
	})

	t.Run("custom hours duration", func(t *testing.T) {
		result := calculateNextReset("48h")
		expected := now.Add(48 * time.Hour)
		// Allow 1 second tolerance
		if result.Sub(expected).Abs() > time.Second {
			t.Errorf("calculateNextReset(48h) = %v, want approximately %v", result, expected)
		}
	})

	t.Run("unknown duration returns zero", func(t *testing.T) {
		result := calculateNextReset("invalid")
		if !result.IsZero() {
			t.Errorf("calculateNextReset(invalid) = %v, want zero time", result)
		}
	})
}

func TestParseCustomDuration(t *testing.T) {
	tests := []struct {
		input   string
		wantDur time.Duration
		wantOK  bool
	}{
		{"48h", 48 * time.Hour, true},
		{"2d", 48 * time.Hour, true},
		{"1d", 24 * time.Hour, true},
		{"30m", 30 * time.Minute, true},
		{"60s", 60 * time.Second, true},
		{"7d", 7 * 24 * time.Hour, true},
		{"30d", 30 * 24 * time.Hour, true},
		// Invalid cases
		{"", 0, false},
		{"h", 0, false},
		{"d", 0, false},
		{"abc", 0, false},
		{"1x", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, ok := parseCustomDuration(tt.input)
			if ok != tt.wantOK {
				t.Errorf("parseCustomDuration(%q) ok = %v, want %v", tt.input, ok, tt.wantOK)
			}
			if ok && got != tt.wantDur {
				t.Errorf("parseCustomDuration(%q) = %v, want %v", tt.input, got, tt.wantDur)
			}
		})
	}
}

func TestParseISOTime(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:    "ISO format with Z suffix",
			input:   "2024-01-15T10:30:00Z",
			wantErr: false,
		},
		{
			name:    "ISO format without Z",
			input:   "2024-01-15T10:30:00",
			wantErr: false,
		},
		{
			name:    "ISO format with microseconds",
			input:   "2024-01-15T10:30:00.123456Z",
			wantErr: false,
		},
		{
			name:    "space separator",
			input:   "2024-01-15 10:30:00",
			wantErr: false,
		},
		{
			name:    "invalid format",
			input:   "not-a-date",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseISOTime(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseISOTime(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestFormatStatusLine(t *testing.T) {
	spend25 := 25.0
	spend75 := 75.0
	spend95 := 95.0
	budget100 := 100.0
	zeroSpend := 0.0

	tests := []struct {
		name           string
		info           *KeyInfo
		expectColor    string
		expectContains []string
	}{
		{
			name: "green color under 75%",
			info: &KeyInfo{
				Spend:     &spend25,
				MaxBudget: &budget100,
			},
			expectColor:    ColorGreen,
			expectContains: []string{"LiteLLM:", "$25.00/$100.00", "(25%)"},
		},
		{
			name: "yellow color 75-90%",
			info: &KeyInfo{
				Spend:     &spend75,
				MaxBudget: &budget100,
			},
			expectColor:    ColorYellow,
			expectContains: []string{"LiteLLM:", "$75.00/$100.00", "(75%)"},
		},
		{
			name: "red color over 90%",
			info: &KeyInfo{
				Spend:     &spend95,
				MaxBudget: &budget100,
			},
			expectColor:    ColorRed,
			expectContains: []string{"LiteLLM:", "$95.00/$100.00", "(95%)"},
		},
		{
			name: "no budget limit",
			info: &KeyInfo{
				Spend:     &spend25,
				MaxBudget: nil,
			},
			expectColor:    ColorGreen,
			expectContains: []string{"LiteLLM:", "$25.00"},
		},
		{
			name: "zero spend",
			info: &KeyInfo{
				Spend:     &zeroSpend,
				MaxBudget: &budget100,
			},
			expectColor:    ColorGreen,
			expectContains: []string{"LiteLLM:", "$0.00/$100.00", "(0%)"},
		},
		{
			name: "nil spend",
			info: &KeyInfo{
				Spend:     nil,
				MaxBudget: &budget100,
			},
			expectColor:    ColorGreen,
			expectContains: []string{"LiteLLM:", "$0.00/$100.00", "(0%)"},
		},
		{
			name: "with reset time",
			info: &KeyInfo{
				Spend:         &spend25,
				MaxBudget:     &budget100,
				BudgetResetAt: strPtr("2020-01-01T00:00:00Z"), // past time
			},
			expectColor:    ColorGreen,
			expectContains: []string{"LiteLLM:", "reset:", "resetting"},
		},
		{
			name: "with budget duration only (monthly)",
			info: &KeyInfo{
				Spend:          &spend25,
				MaxBudget:      &budget100,
				BudgetDuration: strPtr("30d"),
			},
			expectColor:    ColorGreen,
			expectContains: []string{"LiteLLM:", "reset:"},
		},
		{
			name: "with budget duration only (daily)",
			info: &KeyInfo{
				Spend:          &spend25,
				MaxBudget:      &budget100,
				BudgetDuration: strPtr("1d"),
			},
			expectColor:    ColorGreen,
			expectContains: []string{"LiteLLM:", "reset:"},
		},
		{
			name: "with budget duration only (weekly)",
			info: &KeyInfo{
				Spend:          &spend25,
				MaxBudget:      &budget100,
				BudgetDuration: strPtr("7d"),
			},
			expectColor:    ColorGreen,
			expectContains: []string{"LiteLLM:", "reset:"},
		},
		{
			name: "unknown budget duration shows unknown",
			info: &KeyInfo{
				Spend:          &spend25,
				MaxBudget:      &budget100,
				BudgetDuration: strPtr("badformat"),
			},
			expectColor:    ColorGreen,
			expectContains: []string{"LiteLLM:", "reset: unknown"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatStatusLine(tt.info, "")

			if !strings.HasPrefix(result, tt.expectColor) {
				t.Errorf("expected result to start with color %q, got %q", tt.expectColor, result[:len(tt.expectColor)])
			}

			for _, s := range tt.expectContains {
				if !strings.Contains(result, s) {
					t.Errorf("expected result to contain %q, got %q", s, result)
				}
			}

			if !strings.Contains(result, ColorReset) {
				t.Errorf("expected result to contain color reset, got %q", result)
			}
		})
	}
}

func TestFormatError(t *testing.T) {
	result := formatError("Test error")

	if !strings.HasPrefix(result, ColorRed) {
		t.Errorf("expected error to start with red color")
	}
	if !strings.Contains(result, "LiteLLM: Test error") {
		t.Errorf("expected error message, got %q", result)
	}
	if !strings.HasSuffix(result, ColorReset) {
		t.Errorf("expected error to end with color reset")
	}
}

func TestGetEnvWithFallback(t *testing.T) {
	// Set up test env vars
	t.Setenv("TEST_VAR_1", "value1")
	t.Setenv("TEST_VAR_2", "value2")

	tests := []struct {
		name     string
		keys     []string
		expected string
	}{
		{
			name:     "first var exists",
			keys:     []string{"TEST_VAR_1", "TEST_VAR_2"},
			expected: "value1",
		},
		{
			name:     "fallback to second",
			keys:     []string{"NONEXISTENT", "TEST_VAR_2"},
			expected: "value2",
		},
		{
			name:     "none exist",
			keys:     []string{"NONEXISTENT1", "NONEXISTENT2"},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getEnvWithFallback(tt.keys...)
			if result != tt.expected {
				t.Errorf("getEnvWithFallback(%v) = %q, want %q", tt.keys, result, tt.expected)
			}
		})
	}
}

func TestIsDebug(t *testing.T) {
	t.Run("enabled with 1", func(t *testing.T) {
		t.Setenv("LITELLM_DEBUG", "1")
		if !isDebug() {
			t.Error("expected isDebug() = true when LITELLM_DEBUG=1")
		}
	})
	t.Run("enabled with true", func(t *testing.T) {
		t.Setenv("LITELLM_DEBUG", "true")
		if !isDebug() {
			t.Error("expected isDebug() = true when LITELLM_DEBUG=true")
		}
	})
	t.Run("disabled when unset", func(t *testing.T) {
		t.Setenv("LITELLM_DEBUG", "")
		if isDebug() {
			t.Error("expected isDebug() = false when LITELLM_DEBUG is empty")
		}
	})
	t.Run("disabled with 0", func(t *testing.T) {
		t.Setenv("LITELLM_DEBUG", "0")
		if isDebug() {
			t.Error("expected isDebug() = false when LITELLM_DEBUG=0")
		}
	})
}

func TestIsBetaEnabled(t *testing.T) {
	t.Run("enabled with 1", func(t *testing.T) {
		t.Setenv("LITELLM_PLUGIN_PROGRESS_BAR", "1")
		if !isProgressBarEnabled() {
			t.Error("expected isProgressBarEnabled() = true when LITELLM_PLUGIN_BETA_FEATURES=1")
		}
	})
	t.Run("enabled with true", func(t *testing.T) {
		t.Setenv("LITELLM_PLUGIN_PROGRESS_BAR", "true")
		if !isProgressBarEnabled() {
			t.Error("expected isProgressBarEnabled() = true when LITELLM_PLUGIN_BETA_FEATURES=true")
		}
	})
	t.Run("disabled when unset", func(t *testing.T) {
		t.Setenv("LITELLM_PLUGIN_PROGRESS_BAR", "")
		if !isProgressBarEnabled() {
			t.Error("expected isProgressBarEnabled() = true when unset")
		}
	})
	t.Run("disabled with 0", func(t *testing.T) {
		t.Setenv("LITELLM_PLUGIN_PROGRESS_BAR", "0")
		if isProgressBarEnabled() {
			t.Error("expected isProgressBarEnabled() = false when LITELLM_PLUGIN_BETA_FEATURES=0")
		}
	})
	t.Run("disabled with false", func(t *testing.T) {
		t.Setenv("LITELLM_PLUGIN_PROGRESS_BAR", "false")
		if isProgressBarEnabled() {
			t.Error("expected isProgressBarEnabled() = false when LITELLM_PLUGIN_BETA_FEATURES=false")
		}
	})
}

func TestGetKeyInfoWithMockServer(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	spend := 25.0
	budget := 100.0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify auth header
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		resetAt := "2025-01-15T10:00:00Z"
		response := KeyInfoResponse{
			Info: KeyInfo{
				Spend:         &spend,
				MaxBudget:     &budget,
				BudgetResetAt: &resetAt,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	// Set env var to use test server (clear higher-priority var so mock URL is used)
	t.Setenv("LITELLM_PROXY_URL", "")
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)

	info, err := getKeyInfo("test-token")
	if err != nil {
		t.Fatalf("getKeyInfo() error = %v", err)
	}

	if info.Spend == nil || *info.Spend != 25.0 {
		t.Errorf("expected spend = 25.0, got %v", info.Spend)
	}

	if info.MaxBudget == nil || *info.MaxBudget != 100.0 {
		t.Errorf("expected max_budget = 100.0, got %v", info.MaxBudget)
	}
}

func TestGetKeyInfoCaching(t *testing.T) {
	// Use a temp dir so the filesystem cache is fresh for this test
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	callCount := 0
	spend := 25.0
	budget := 100.0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		response := KeyInfoResponse{
			Info: KeyInfo{
				Spend:     &spend,
				MaxBudget: &budget,
			},
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	t.Setenv("LITELLM_PROXY_URL", "")
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)

	// First call — should hit API and write filesystem cache
	_, err := getKeyInfo("test-token")
	if err != nil {
		t.Fatalf("first getKeyInfo() error = %v", err)
	}

	// Second call — should read from filesystem cache, skip API
	_, err = getKeyInfo("test-token")
	if err != nil {
		t.Fatalf("second getKeyInfo() error = %v", err)
	}

	if callCount != 1 {
		t.Errorf("expected 1 API call (filesystem caching), got %d", callCount)
	}
}

func TestGetKeyInfoAuthError(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	t.Setenv("LITELLM_PROXY_URL", "")
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)

	_, err := getKeyInfo("bad-token")
	if err == nil {
		t.Fatal("expected auth error, got nil")
	}

	if !errors.Is(err, ErrAuth) {
		t.Errorf("expected ErrAuth, got %v", err)
	}
}

func TestGetKeyInfoForbiddenError(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	t.Setenv("LITELLM_PROXY_URL", "")
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)

	_, err := getKeyInfo("bad-token")
	if err == nil {
		t.Fatal("expected auth error for 403, got nil")
	}

	if !errors.Is(err, ErrAuth) {
		t.Errorf("expected ErrAuth for 403, got %v", err)
	}
}

func TestGetKeyInfoBudgetExceeded(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Budget has been exceeded! Current cost: 78.99, Max budget: 65.0","type":"budget_exceeded","param":null,"code":"400"}}`))
	}))
	defer server.Close()

	t.Setenv("LITELLM_PROXY_URL", "")
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)

	_, err := getKeyInfo("some-token")
	if err == nil {
		t.Fatal("expected budget exceeded error, got nil")
	}
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("expected ErrBudgetExceeded, got %v", err)
	}

	var bErr *BudgetExceededError
	if !errors.As(err, &bErr) {
		t.Fatal("expected error to be *BudgetExceededError")
	}
	if bErr.Spend != 78.99 {
		t.Errorf("expected Spend=78.99, got %v", bErr.Spend)
	}
	if bErr.MaxBudget != 65.0 {
		t.Errorf("expected MaxBudget=65.0, got %v", bErr.MaxBudget)
	}
}

func TestFetchKeyInfoEmptyBaseURL(t *testing.T) {
	t.Setenv("LITELLM_PROXY_URL", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")

	_, err := fetchKeyInfo("some-token")
	if err == nil {
		t.Fatal("expected error for empty baseURL, got nil")
	}
	if !strings.Contains(err.Error(), "no LiteLLM proxy URL") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestANSIColors(t *testing.T) {
	tests := []struct {
		name     string
		color    string
		expected string
	}{
		{"green", ColorGreen, "\x1b[32m"},
		{"yellow", ColorYellow, "\x1b[33m"},
		{"red", ColorRed, "\x1b[31m"},
		{"gray", ColorGray, "\x1b[90m"},
		{"reset", ColorReset, "\x1b[0m"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.color != tt.expected {
				t.Errorf("%s color = %q, want %q", tt.name, tt.color, tt.expected)
			}
		})
	}
}

func TestSemverGreater(t *testing.T) {
	tests := []struct {
		a, b     string
		expected bool
	}{
		{"1.2.3", "1.2.2", true},
		{"1.2.2", "1.2.3", false},
		{"2.0.0", "1.9.9", true},
		{"1.9.9", "2.0.0", false},
		{"1.2.3", "1.2.3", false},
		{"1.3.0", "1.2.9", true},
	}

	for _, tt := range tests {
		result := semverGreater(tt.a, tt.b)
		if result != tt.expected {
			t.Errorf("semverGreater(%q, %q) = %v, want %v", tt.a, tt.b, result, tt.expected)
		}
	}
}

func TestIsUpdateAvailable(t *testing.T) {
	tests := []struct {
		name     string
		current  string
		latest   string
		expected bool
	}{
		{"dev build skipped", "dev", "v1.0.0", false},
		{"no latest", "v1.0.0", "", false},
		{"newer available", "v1.0.0", "v1.0.1", true},
		{"up to date", "v1.0.1", "v1.0.1", false},
		{"older latest", "v1.1.0", "v1.0.0", false},
		{"major bump", "v1.0.0", "v2.0.0", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isUpdateAvailable(tt.current, tt.latest)
			if result != tt.expected {
				t.Errorf("isUpdateAvailable(%q, %q) = %v, want %v", tt.current, tt.latest, result, tt.expected)
			}
		})
	}
}

func TestFormatStatusLineWithUpdate(t *testing.T) {
	spend := 25.0
	budget := 100.0
	info := &KeyInfo{Spend: &spend, MaxBudget: &budget}

	// Save and restore Version
	origVersion := Version
	defer func() { Version = origVersion }()

	Version = "v1.0.0"
	result := formatStatusLine(info, "v1.1.0")
	if !strings.Contains(result, "update: v1.1.0") {
		t.Errorf("expected update notice, got %q", result)
	}
	if !strings.Contains(result, ColorYellow) {
		t.Errorf("expected yellow color in update notice, got %q", result)
	}
}

func TestFormatStatusLineNoUpdate(t *testing.T) {
	spend := 25.0
	budget := 100.0
	info := &KeyInfo{Spend: &spend, MaxBudget: &budget}

	origVersion := Version
	defer func() { Version = origVersion }()

	Version = "v1.0.0"
	result := formatStatusLine(info, "v1.0.0") // same version
	if strings.Contains(result, "update:") {
		t.Errorf("expected no update notice for same version, got %q", result)
	}
}

func TestGetPrefix(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		if err := os.Unsetenv("LITELLM_PLUGIN_PREFIX"); err != nil {
			t.Fatal(err)
		}
		if got := getPrefix(); got != "LiteLLM: " {
			t.Errorf("expected 'LiteLLM: ', got %q", got)
		}
	})
	t.Run("blank when set to empty string", func(t *testing.T) {
		t.Setenv("LITELLM_PLUGIN_PREFIX", "")
		if got := getPrefix(); got != "" {
			t.Errorf("expected empty string, got %q", got)
		}
	})
	t.Run("custom prefix with trailing space", func(t *testing.T) {
		t.Setenv("LITELLM_PLUGIN_PREFIX", "Budget")
		if got := getPrefix(); got != "Budget " {
			t.Errorf("expected 'Budget ', got %q", got)
		}
	})
}

func TestFormatStatusLineShowCost(t *testing.T) {
	spend := 26.71
	budget := 65.0
	info := &KeyInfo{Spend: &spend, MaxBudget: &budget}

	t.Run("cost shown by default", func(t *testing.T) {
		t.Setenv("LITELLM_PLUGIN_SHOW_COST", "")
		result := formatStatusLine(info, "")
		if !strings.Contains(result, "$26.71") {
			t.Errorf("expected dollar amounts in output, got %q", result)
		}
	})

	t.Run("cost shown when set to 1", func(t *testing.T) {
		t.Setenv("LITELLM_PLUGIN_SHOW_COST", "1")
		result := formatStatusLine(info, "")
		if !strings.Contains(result, "$26.71") {
			t.Errorf("expected dollar amounts in output, got %q", result)
		}
	})

	t.Run("cost hidden when set to 0", func(t *testing.T) {
		t.Setenv("LITELLM_PLUGIN_SHOW_COST", "0")
		result := formatStatusLine(info, "")
		if strings.Contains(result, "$") {
			t.Errorf("expected no dollar amounts in output, got %q", result)
		}
		if !strings.Contains(result, "41%") {
			t.Errorf("expected percentage in output, got %q", result)
		}
	})

	t.Run("cost hidden when set to false", func(t *testing.T) {
		t.Setenv("LITELLM_PLUGIN_SHOW_COST", "false")
		result := formatStatusLine(info, "")
		if strings.Contains(result, "$") {
			t.Errorf("expected no dollar amounts in output, got %q", result)
		}
	})
}

func TestGetLatestVersionCaching(t *testing.T) {
	// Use a temp dir so the filesystem cache is fresh for this test
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	// Write a version to the cache and verify readUpdateCache returns it
	writeUpdateCache("v1.2.3")

	version, ok := readUpdateCache()
	if !ok {
		t.Fatal("expected readUpdateCache to return ok=true after write")
	}
	if version != "v1.2.3" {
		t.Errorf("expected v1.2.3 from cache, got %q", version)
	}
}

func TestUpdateCacheExpiry(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmpDir)

	// Write a stale cache entry (timestamp 2 hours in the past, beyond the 1h TTL)
	staleEntry := UpdateCacheEntry{
		Timestamp:     time.Now().Add(-2 * time.Hour).UnixMilli(),
		LatestVersion: "v1.0.0",
	}
	staleData, err := json.Marshal(staleEntry)
	if err != nil {
		t.Fatalf("failed to marshal stale entry: %v", err)
	}
	if mkErr := os.MkdirAll(cacheDir(), 0o755); mkErr != nil {
		t.Fatalf("failed to create cache dir: %v", mkErr)
	}
	if wErr := os.WriteFile(updateCacheFile(), staleData, 0o600); wErr != nil {
		t.Fatalf("failed to write stale cache: %v", wErr)
	}

	// Should be rejected as stale
	_, ok := readUpdateCache()
	if ok {
		t.Error("expected readUpdateCache to return ok=false for stale cache")
	}
}

func TestBudgetCacheReadWrite(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	spend := 42.0
	budget := 100.0
	info := &KeyInfo{Spend: &spend, MaxBudget: &budget}

	// Nothing in cache yet
	_, ok := readBudgetCache()
	if ok {
		t.Error("expected readBudgetCache to return ok=false with empty cache")
	}

	// Write and read back
	writeBudgetCache(info)
	got, ok := readBudgetCache()
	if !ok {
		t.Fatal("expected readBudgetCache to return ok=true after write")
	}
	if got.Spend == nil || *got.Spend != 42.0 {
		t.Errorf("expected spend = 42.0, got %v", got.Spend)
	}
	if got.MaxBudget == nil || *got.MaxBudget != 100.0 {
		t.Errorf("expected budget = 100.0, got %v", got.MaxBudget)
	}
}

func TestResolveEffectiveBudget(t *testing.T) {
	keySpend := 10.0
	keyBudget := 50.0
	teamSpend := 25.0
	teamBudget := 100.0
	zeroBudget := 0.0
	resetAt := "2025-01-15T10:00:00Z"
	duration := "30d"

	tests := []struct {
		name          string
		info          *KeyInfo
		wantSpend     float64
		wantMaxBudget float64
		wantNilBudget bool
	}{
		{
			name: "key budget present — key budget used",
			info: &KeyInfo{
				Spend:         &keySpend,
				MaxBudget:     &keyBudget,
				TeamSpend:     &teamSpend,
				TeamMaxBudget: &teamBudget,
			},
			wantSpend:     keySpend,
			wantMaxBudget: keyBudget,
		},
		{
			name: "no key budget — team budget used",
			info: &KeyInfo{
				Spend:              &keySpend,
				TeamSpend:          &teamSpend,
				TeamMaxBudget:      &teamBudget,
				TeamBudgetResetAt:  &resetAt,
				TeamBudgetDuration: &duration,
			},
			wantSpend:     teamSpend,
			wantMaxBudget: teamBudget,
		},
		{
			name: "key budget zero — team budget used",
			info: &KeyInfo{
				Spend:         &keySpend,
				MaxBudget:     &zeroBudget,
				TeamSpend:     &teamSpend,
				TeamMaxBudget: &teamBudget,
			},
			wantSpend:     teamSpend,
			wantMaxBudget: teamBudget,
		},
		{
			name:          "no budgets — passthrough",
			info:          &KeyInfo{Spend: &keySpend},
			wantSpend:     keySpend,
			wantNilBudget: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveEffectiveBudget(tt.info)
			if tt.wantNilBudget {
				if got.MaxBudget != nil && *got.MaxBudget > 0 {
					t.Errorf("expected nil/zero MaxBudget, got %v", *got.MaxBudget)
				}
				return
			}
			if got.Spend == nil || *got.Spend != tt.wantSpend {
				t.Errorf("Spend = %v, want %v", got.Spend, tt.wantSpend)
			}
			if got.MaxBudget == nil || *got.MaxBudget != tt.wantMaxBudget {
				t.Errorf("MaxBudget = %v, want %v", got.MaxBudget, tt.wantMaxBudget)
			}
		})
	}
}

func TestFormatStatusLineTeamBudget(t *testing.T) {
	keySpend := 10.0
	teamSpend := 40.0
	teamBudget := 100.0
	duration := "30d"

	t.Run("team budget shown when key has no budget", func(t *testing.T) {
		info := &KeyInfo{
			Spend:              &keySpend,
			TeamSpend:          &teamSpend,
			TeamMaxBudget:      &teamBudget,
			TeamBudgetDuration: &duration,
		}
		result := formatStatusLine(info, "")
		if !strings.Contains(result, "$40.00/$100.00") {
			t.Errorf("expected team spend/budget in output, got %q", result)
		}
		if !strings.Contains(result, "(40%)") {
			t.Errorf("expected team usage percentage, got %q", result)
		}
	})

	t.Run("key budget takes priority over team budget", func(t *testing.T) {
		keyBudget := 50.0
		info := &KeyInfo{
			Spend:         &keySpend,
			MaxBudget:     &keyBudget,
			TeamSpend:     &teamSpend,
			TeamMaxBudget: &teamBudget,
		}
		result := formatStatusLine(info, "")
		if !strings.Contains(result, "$10.00/$50.00") {
			t.Errorf("expected key spend/budget in output, got %q", result)
		}
	})
}

func TestFetchTeamInfo(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	memberBudget := 65.0
	memberDuration := "7d"
	resetAt := "2026-04-06T00:00:00Z"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/team/info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("team_id") != "team-123" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		response := TeamInfoAPIResponse{
			TeamInfo: TeamInfoData{
				BudgetDuration: &memberDuration,
				BudgetResetAt:  &resetAt,
				TeamMemberBudgetTable: &TeamMemberBudgetTable{
					MaxBudget:      &memberBudget,
					BudgetDuration: &memberDuration,
				},
			},
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	t.Setenv("LITELLM_PROXY_URL", "")
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)

	data, err := fetchTeamInfo("test-token", "team-123")
	if err != nil {
		t.Fatalf("fetchTeamInfo() error = %v", err)
	}
	if data.TeamMemberBudgetTable == nil {
		t.Fatal("expected non-nil TeamMemberBudgetTable")
	}
	if data.TeamMemberBudgetTable.MaxBudget == nil || *data.TeamMemberBudgetTable.MaxBudget != 65.0 {
		t.Errorf("expected MaxBudget=65.0, got %v", data.TeamMemberBudgetTable.MaxBudget)
	}
	if data.BudgetResetAt == nil || *data.BudgetResetAt != resetAt {
		t.Errorf("expected BudgetResetAt=%q, got %v", resetAt, data.BudgetResetAt)
	}
}

func TestGetKeyInfoMergesTeamBudget(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	keySpend := 4.0
	memberBudget := 65.0
	memberDuration := "7d"
	resetAt := "2026-04-06T00:00:00Z"
	teamID := "team-123"
	keyDuration := "7d"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key/info":
			resp := KeyInfoResponse{
				Info: KeyInfo{
					Spend:          &keySpend,
					TeamID:         &teamID,
					BudgetDuration: &keyDuration,
					BudgetResetAt:  &resetAt,
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/team/info":
			resp := TeamInfoAPIResponse{
				TeamInfo: TeamInfoData{
					BudgetDuration: &memberDuration,
					BudgetResetAt:  &resetAt,
					TeamMemberBudgetTable: &TeamMemberBudgetTable{
						MaxBudget:      &memberBudget,
						BudgetDuration: &memberDuration,
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("LITELLM_PROXY_URL", "")
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)

	info, err := getKeyInfo("test-token")
	if err != nil {
		t.Fatalf("getKeyInfo() error = %v", err)
	}
	if info.TeamMaxBudget == nil || *info.TeamMaxBudget != 65.0 {
		t.Errorf("expected TeamMaxBudget=65.0, got %v", info.TeamMaxBudget)
	}
	if info.TeamSpend == nil || *info.TeamSpend != 4.0 {
		t.Errorf("expected TeamSpend=4.0 (key spend), got %v", info.TeamSpend)
	}

	// formatStatusLine should show member budget, not raw spend
	result := formatStatusLine(info, "")
	if !strings.Contains(result, "$4.00/$65.00") {
		t.Errorf("expected $4.00/$65.00 in output, got %q", result)
	}
}

func TestZeroBudgetDivision(t *testing.T) {
	spend := 10.0
	zeroBudget := 0.0

	info := &KeyInfo{
		Spend:     &spend,
		MaxBudget: &zeroBudget,
	}

	// Should not panic on zero budget
	result := formatStatusLine(info, "")

	// With zero budget, it should show just the spend (like no budget)
	if !strings.Contains(result, "$10.00") {
		t.Errorf("expected result to contain spend, got %q", result)
	}
}

func TestRenderProgressBar(t *testing.T) {
	tests := []struct {
		name            string
		percent         float64
		elapsedFraction float64
		hasTimeInfo     bool
		wantContains    []string
		wantNotContains []string
	}{
		{
			name:            "partial fill without marker",
			percent:         50,
			hasTimeInfo:     false,
			wantContains:    []string{"[", "]", "█", "░"},
			wantNotContains: []string{"│"},
		},
		{
			name:            "full fill at 100%",
			percent:         100,
			hasTimeInfo:     false,
			wantContains:    []string{"█"},
			wantNotContains: []string{"░"},
		},
		{
			name:            "pace marker present with time info",
			percent:         50,
			elapsedFraction: 0.5,
			hasTimeInfo:     true,
			wantContains:    []string{"█", "│", "░"},
		},
		{
			name:            "green marker when under budget",
			percent:         25,
			elapsedFraction: 0.5,
			hasTimeInfo:     true,
			wantContains:    []string{"│", ColorGreen},
		},
		{
			name:            "yellow marker at projected 100%",
			percent:         50,
			elapsedFraction: 0.5,
			hasTimeInfo:     true,
			wantContains:    []string{"│", ColorYellow},
		},
		{
			name:            "red marker when over projected budget",
			percent:         80,
			elapsedFraction: 0.5,
			hasTimeInfo:     true,
			wantContains:    []string{"│", ColorRed},
		},
		{
			name:            "low elapsed skips pace projection",
			percent:         50,
			elapsedFraction: 0.02,
			hasTimeInfo:     true,
			wantContains:    []string{"│"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := renderProgressBar(tt.percent, tt.elapsedFraction, tt.hasTimeInfo)
			for _, s := range tt.wantContains {
				if !strings.Contains(result, s) {
					t.Errorf("expected %q in result, got %q", s, result)
				}
			}
			for _, s := range tt.wantNotContains {
				if strings.Contains(result, s) {
					t.Errorf("unexpected %q in result, got %q", s, result)
				}
			}
		})
	}
}

func TestCalculateBudgetPeriod(t *testing.T) {
	durations := []struct {
		name       string
		duration   string
		advance    time.Duration
		wantPeriod time.Duration
		wantOK     bool
	}{
		{"daily", "1d", 24 * time.Hour, 24 * time.Hour, true},
		{"weekly", "7d", 7 * 24 * time.Hour, 7 * 24 * time.Hour, true},
		{"custom 48h", "48h", 48 * time.Hour, 48 * time.Hour, true},
		{"monthly alias", "monthly", 30 * 24 * time.Hour, 30 * 24 * time.Hour, true},
		{"weekly alias", "weekly", 7 * 24 * time.Hour, 7 * 24 * time.Hour, true},
		{"nil duration", "", 0, 0, false},
		{"invalid duration", "invalid", 0, 0, false},
	}

	for _, tt := range durations {
		t.Run(tt.name, func(t *testing.T) {
			var dur *string
			if tt.duration != "" {
				dur = strPtr(tt.duration)
			}
			info := &KeyInfo{BudgetDuration: dur}
			if tt.wantOK {
				resetAt := time.Now().UTC().Add(tt.advance).Format(time.RFC3339)
				info.BudgetResetAt = strPtr(resetAt)
			}
			start, end, ok := calculateBudgetPeriod(info)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && end.Sub(start) != tt.wantPeriod {
				t.Errorf("period = %v, want %v", end.Sub(start), tt.wantPeriod)
			}
		})
	}
}

func TestCalculateElapsedFraction(t *testing.T) {
	t.Run("no duration returns false", func(t *testing.T) {
		info := &KeyInfo{}
		_, ok := calculateElapsedFraction(info)
		if ok {
			t.Error("expected ok=false with no duration")
		}
	})

	t.Run("daily budget returns fraction between 0 and 1", func(t *testing.T) {
		tomorrow := time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
		resetAt := tomorrow.Format(time.RFC3339)
		info := &KeyInfo{
			BudgetDuration: strPtr("1d"),
			BudgetResetAt:  strPtr(resetAt),
		}
		frac, ok := calculateElapsedFraction(info)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if frac < 0 || frac > 1.0 {
			t.Errorf("elapsed fraction = %f, want [0, 1]", frac)
		}
		if frac == 0 {
			t.Error("expected non-zero elapsed fraction mid-day")
		}
	})

	t.Run("past reset returns clamped 1.0", func(t *testing.T) {
		yesterday := time.Now().UTC().Add(-24 * time.Hour)
		resetAt := yesterday.Format(time.RFC3339)
		info := &KeyInfo{
			BudgetDuration: strPtr("1d"),
			BudgetResetAt:  strPtr(resetAt),
		}
		frac, ok := calculateElapsedFraction(info)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if frac != 1.0 {
			t.Errorf("elapsed fraction = %f, want 1.0 (clamped)", frac)
		}
	})
}
