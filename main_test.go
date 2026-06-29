package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
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
	// Default-off SHOW_COST is the new normal; make sure no ambient env leaks in.
	t.Setenv("LITELLM_PLUGIN_SHOW_COST", "")
	t.Setenv("LITELLM_PLUGIN_PREFIX", "")

	spend25 := 25.0
	spend75 := 75.0
	spend95 := 95.0
	budget100 := 100.0
	zeroSpend := 0.0

	tests := []struct {
		name              string
		info              *KeyInfo
		expectColor       string
		expectContains    []string
		expectNotContains []string
	}{
		{
			name: "green circle under 75%",
			info: &KeyInfo{
				TeamSpend:     &spend25,
				TeamMaxBudget: &budget100,
			},
			expectColor:       ColorGreen,
			expectContains:    []string{"◔", "25%"},
			expectNotContains: []string{"$"},
		},
		{
			name: "yellow circle 75-90%",
			info: &KeyInfo{
				TeamSpend:     &spend75,
				TeamMaxBudget: &budget100,
			},
			expectColor:       ColorYellow,
			expectContains:    []string{"◕", "75%"},
			expectNotContains: []string{"$"},
		},
		{
			name: "red circle over 90%",
			info: &KeyInfo{
				TeamSpend:     &spend95,
				TeamMaxBudget: &budget100,
			},
			expectColor:    ColorRed,
			expectContains: []string{"●", "95%"}, // 95% lands in the "full" bucket (≥85)
		},
		{
			name: "no team budget shows error, never key spend",
			info: &KeyInfo{
				Spend:         &spend25, // key spend present but must be ignored
				TeamMaxBudget: nil,
			},
			expectColor:       ColorRed,
			expectContains:    []string{"no budget configured"},
			expectNotContains: []string{"$"},
		},
		{
			name: "zero spend",
			info: &KeyInfo{
				TeamSpend:     &zeroSpend,
				TeamMaxBudget: &budget100,
			},
			expectColor:    ColorGreen,
			expectContains: []string{"○", "0%"},
		},
		{
			name: "nil spend",
			info: &KeyInfo{
				TeamSpend:     nil,
				TeamMaxBudget: &budget100,
			},
			expectColor:    ColorGreen,
			expectContains: []string{"○", "0%"},
		},
		{
			name: "with reset time",
			info: &KeyInfo{
				TeamSpend:         &spend25,
				TeamMaxBudget:     &budget100,
				TeamBudgetResetAt: strPtr("2020-01-01T00:00:00Z"), // past time
			},
			expectColor:    ColorGreen,
			expectContains: []string{"◔", "reset:", "resetting"},
		},
		{
			name: "with budget duration only (monthly)",
			info: &KeyInfo{
				TeamSpend:          &spend25,
				TeamMaxBudget:      &budget100,
				TeamBudgetDuration: strPtr("30d"),
			},
			expectColor:    ColorGreen,
			expectContains: []string{"◔", "reset:"},
		},
		{
			name: "unknown budget duration shows unknown",
			info: &KeyInfo{
				TeamSpend:          &spend25,
				TeamMaxBudget:      &budget100,
				TeamBudgetDuration: strPtr("badformat"),
			},
			expectColor:    ColorGreen,
			expectContains: []string{"◔", "reset: unknown"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatStatusLine(tt.info, "", StatusInput{})

			if !strings.Contains(result, tt.expectColor) {
				t.Errorf("expected result to contain color %q, got %q", tt.expectColor, result)
			}

			for _, s := range tt.expectContains {
				if !strings.Contains(result, s) {
					t.Errorf("expected result to contain %q, got %q", s, result)
				}
			}

			for _, s := range tt.expectNotContains {
				if strings.Contains(result, s) {
					t.Errorf("expected result NOT to contain %q, got %q", s, result)
				}
			}

			if !strings.Contains(result, ColorReset) {
				t.Errorf("expected result to contain color reset, got %q", result)
			}
		})
	}
}

func TestFormatError(t *testing.T) {
	t.Setenv("LITELLM_PLUGIN_PREFIX", "")
	if err := os.Unsetenv("LITELLM_PLUGIN_PREFIX"); err != nil {
		t.Fatal(err)
	}

	result := formatError("Test error", StatusInput{})

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

func TestReadStatusInput(t *testing.T) {
	t.Run("valid JSON with model and context", func(t *testing.T) {
		in := strings.NewReader(`{"model":{"display_name":"Opus 4.7","id":"claude-opus-4-7"},"context_window":{"used_percentage":78.5}}`)
		got := readStatusInput(in)
		if got.Model.DisplayName != "Opus 4.7" {
			t.Errorf("DisplayName = %q, want %q", got.Model.DisplayName, "Opus 4.7")
		}
		if got.Model.ID != "claude-opus-4-7" {
			t.Errorf("ID = %q, want %q", got.Model.ID, "claude-opus-4-7")
		}
		if got.ContextWindow == nil || got.ContextWindow.UsedPercentage == nil || *got.ContextWindow.UsedPercentage != 78.5 {
			t.Errorf("UsedPercentage missing or wrong: %+v", got.ContextWindow)
		}
	})
	t.Run("null used_percentage", func(t *testing.T) {
		in := strings.NewReader(`{"context_window":{"used_percentage":null}}`)
		got := readStatusInput(in)
		if got.ContextWindow == nil {
			t.Fatal("expected non-nil ContextWindow")
		}
		if got.ContextWindow.UsedPercentage != nil {
			t.Errorf("expected nil UsedPercentage, got %v", *got.ContextWindow.UsedPercentage)
		}
	})
	t.Run("missing context_window", func(t *testing.T) {
		in := strings.NewReader(`{"model":{"display_name":"Sonnet"}}`)
		got := readStatusInput(in)
		if got.ContextWindow != nil {
			t.Errorf("expected nil ContextWindow, got %+v", got.ContextWindow)
		}
	})
	t.Run("empty stdin yields zero value", func(t *testing.T) {
		got := readStatusInput(strings.NewReader(""))
		if got.Model.DisplayName != "" || got.ContextWindow != nil {
			t.Errorf("expected zero value, got %+v", got)
		}
	})
	t.Run("malformed JSON yields zero value", func(t *testing.T) {
		got := readStatusInput(strings.NewReader("not json"))
		if got.Model.DisplayName != "" || got.ContextWindow != nil {
			t.Errorf("expected zero value, got %+v", got)
		}
	})
}

func TestCircleGlyph(t *testing.T) {
	tests := []struct {
		pct  float64
		want string
	}{
		{-5, "○"},
		{0, "○"},
		{1, "◔"},
		{29, "◔"},
		{30, "◑"},
		{59, "◑"},
		{60, "◕"},
		{84, "◕"},
		{85, "●"},
		{100, "●"},
		{150, "●"},
	}
	for _, tt := range tests {
		if got := circleGlyph(tt.pct); got != tt.want {
			t.Errorf("circleGlyph(%v) = %q, want %q", tt.pct, got, tt.want)
		}
	}
}

func TestContextColor(t *testing.T) {
	tests := []struct {
		pct  float64
		want string
	}{
		{0, ColorGreen},
		{69, ColorGreen},
		{70, ColorYellow},
		{84, ColorYellow},
		{85, ColorRed},
		{100, ColorRed},
	}
	for _, tt := range tests {
		if got := contextColor(tt.pct); got != tt.want {
			t.Errorf("contextColor(%v) = %q, want %q", tt.pct, got, tt.want)
		}
	}
}

func TestFormatContextSegment(t *testing.T) {
	mk := func(pct *float64) StatusInput {
		var in StatusInput
		if pct != nil {
			in.ContextWindow = &struct {
				UsedPercentage *float64 `json:"used_percentage"`
			}{UsedPercentage: pct}
		}
		return in
	}
	f := func(v float64) *float64 { return &v }

	t.Run("absent context window omits segment", func(t *testing.T) {
		if got := formatContextSegment(StatusInput{}); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
	t.Run("nil used_percentage omits segment", func(t *testing.T) {
		if got := formatContextSegment(mk(nil)); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
	t.Run("low usage shows segment without suggestion", func(t *testing.T) {
		got := formatContextSegment(mk(f(30)))
		if !strings.Contains(got, "📖") || !strings.Contains(got, "◑") || !strings.Contains(got, "30%") {
			t.Errorf("missing parts in %q", got)
		}
		if strings.Contains(got, "/compact") || strings.Contains(got, "/clear") {
			t.Errorf("did not expect suggestion at 30%%, got %q", got)
		}
		if !strings.Contains(got, ColorGreen) {
			t.Errorf("expected green at 30%%, got %q", got)
		}
	})
	t.Run("warn threshold suggests /compact", func(t *testing.T) {
		got := formatContextSegment(mk(f(78)))
		if !strings.Contains(got, "consider /compact") {
			t.Errorf("missing suggestion, got %q", got)
		}
		if !strings.Contains(got, ColorYellow) {
			t.Errorf("expected yellow at 78%%, got %q", got)
		}
	})
	t.Run("critical threshold urges /compact or /clear", func(t *testing.T) {
		got := formatContextSegment(mk(f(92)))
		if !strings.Contains(got, "run /compact or /clear") {
			t.Errorf("missing urgent suggestion, got %q", got)
		}
		if !strings.Contains(got, ColorRed) {
			t.Errorf("expected red at 92%%, got %q", got)
		}
	})
	t.Run("clamps values above 100", func(t *testing.T) {
		got := formatContextSegment(mk(f(150)))
		if !strings.Contains(got, "100%") {
			t.Errorf("expected clamped 100%%, got %q", got)
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
	info := &KeyInfo{TeamSpend: &spend, TeamMaxBudget: &budget}

	// Save and restore Version
	origVersion := Version
	defer func() { Version = origVersion }()

	Version = "v1.0.0"
	result := formatStatusLine(info, "v1.1.0", StatusInput{})
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
	info := &KeyInfo{TeamSpend: &spend, TeamMaxBudget: &budget}

	origVersion := Version
	defer func() { Version = origVersion }()

	Version = "v1.0.0"
	result := formatStatusLine(info, "v1.0.0", StatusInput{})
	if strings.Contains(result, "update:") {
		t.Errorf("expected no update notice for same version, got %q", result)
	}
}

func modelInput(name string) StatusInput {
	var in StatusInput
	in.Model.DisplayName = name
	return in
}

func TestGetPrefix(t *testing.T) {
	t.Run("default when unset and no model", func(t *testing.T) {
		if err := os.Unsetenv("LITELLM_PLUGIN_PREFIX"); err != nil {
			t.Fatal(err)
		}
		if got := getPrefix(StatusInput{}); got != "LiteLLM: " {
			t.Errorf("expected 'LiteLLM: ', got %q", got)
		}
	})
	t.Run("model display name used when env unset", func(t *testing.T) {
		if err := os.Unsetenv("LITELLM_PLUGIN_PREFIX"); err != nil {
			t.Fatal(err)
		}
		if got := getPrefix(modelInput("Opus 4.7")); got != "Opus 4.7: " {
			t.Errorf("expected 'Opus 4.7: ', got %q", got)
		}
	})
	t.Run("env override beats model name", func(t *testing.T) {
		t.Setenv("LITELLM_PLUGIN_PREFIX", "Budget")
		if got := getPrefix(modelInput("Opus 4.7")); got != "Budget " {
			t.Errorf("expected 'Budget ', got %q", got)
		}
	})
	t.Run("blank env override beats model name", func(t *testing.T) {
		t.Setenv("LITELLM_PLUGIN_PREFIX", "")
		if got := getPrefix(modelInput("Opus 4.7")); got != "" {
			t.Errorf("expected empty string, got %q", got)
		}
	})
	t.Run("custom prefix with trailing space", func(t *testing.T) {
		t.Setenv("LITELLM_PLUGIN_PREFIX", "Budget")
		if got := getPrefix(StatusInput{}); got != "Budget " {
			t.Errorf("expected 'Budget ', got %q", got)
		}
	})
}

func TestFormatStatusLineShowCost(t *testing.T) {
	spend := 26.71
	budget := 65.0
	info := &KeyInfo{TeamSpend: &spend, TeamMaxBudget: &budget}

	t.Run("cost hidden by default", func(t *testing.T) {
		t.Setenv("LITELLM_PLUGIN_SHOW_COST", "")
		result := formatStatusLine(info, "", StatusInput{})
		if strings.Contains(result, "$") {
			t.Errorf("expected no dollar amounts by default, got %q", result)
		}
		if !strings.Contains(result, "41%") {
			t.Errorf("expected percentage in output, got %q", result)
		}
	})

	t.Run("cost shown when set to 1", func(t *testing.T) {
		t.Setenv("LITELLM_PLUGIN_SHOW_COST", "1")
		result := formatStatusLine(info, "", StatusInput{})
		if !strings.Contains(result, "$26.71") {
			t.Errorf("expected dollar amounts in output, got %q", result)
		}
	})

	t.Run("cost shown when set to true", func(t *testing.T) {
		t.Setenv("LITELLM_PLUGIN_SHOW_COST", "true")
		result := formatStatusLine(info, "", StatusInput{})
		if !strings.Contains(result, "$26.71") {
			t.Errorf("expected dollar amounts in output, got %q", result)
		}
	})

	t.Run("cost hidden when set to 0", func(t *testing.T) {
		t.Setenv("LITELLM_PLUGIN_SHOW_COST", "0")
		result := formatStatusLine(info, "", StatusInput{})
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
			name: "team budget used; key budget ignored",
			info: &KeyInfo{
				Spend:         &keySpend,
				MaxBudget:     &keyBudget, // present but must be ignored
				TeamSpend:     &teamSpend,
				TeamMaxBudget: &teamBudget,
			},
			wantSpend:     teamSpend,
			wantMaxBudget: teamBudget,
		},
		{
			name: "team budget with reset/duration",
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
			name:          "no team budget — empty, key spend not leaked",
			info:          &KeyInfo{Spend: &keySpend, MaxBudget: &keyBudget},
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
				if got.Spend != nil {
					t.Errorf("expected no spend to leak, got %v", *got.Spend)
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
	// These tests exercise team-vs-key budget selection; pin SHOW_COST=1 so we
	// can verify dollar values match the resolved source.
	t.Setenv("LITELLM_PLUGIN_SHOW_COST", "1")
	t.Setenv("LITELLM_PLUGIN_PREFIX", "LiteLLM:")

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
		result := formatStatusLine(info, "", StatusInput{})
		if !strings.Contains(result, "$40.00/$100.00") {
			t.Errorf("expected team spend/budget in output, got %q", result)
		}
		if !strings.Contains(result, "(40%)") {
			t.Errorf("expected team usage percentage, got %q", result)
		}
	})

	t.Run("key budget ignored when team budget present", func(t *testing.T) {
		keyBudget := 50.0
		info := &KeyInfo{
			Spend:         &keySpend,
			MaxBudget:     &keyBudget, // present but must be ignored
			TeamSpend:     &teamSpend,
			TeamMaxBudget: &teamBudget,
		}
		result := formatStatusLine(info, "", StatusInput{})
		if !strings.Contains(result, "$40.00/$100.00") {
			t.Errorf("expected team spend/budget (key ignored), got %q", result)
		}
		if strings.Contains(result, "$10.00") || strings.Contains(result, "$50.00") {
			t.Errorf("key spend/budget must not appear, got %q", result)
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
	if data.TeamInfo.TeamMemberBudgetTable == nil {
		t.Fatal("expected non-nil TeamMemberBudgetTable")
	}
	if data.TeamInfo.TeamMemberBudgetTable.MaxBudget == nil || *data.TeamInfo.TeamMemberBudgetTable.MaxBudget != 65.0 {
		t.Errorf("expected MaxBudget=65.0, got %v", data.TeamInfo.TeamMemberBudgetTable.MaxBudget)
	}
	if data.TeamInfo.BudgetResetAt == nil || *data.TeamInfo.BudgetResetAt != resetAt {
		t.Errorf("expected BudgetResetAt=%q, got %v", resetAt, data.TeamInfo.BudgetResetAt)
	}
}

// TestGetKeyInfoUsesMembershipBudget covers the primary source: the per-member budget
// in team_memberships[].litellm_budget_table, paired with the membership's own spend.
// The key's own spend is present but must be ignored.
func TestGetKeyInfoUsesMembershipBudget(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	keySpend := 16.54 // present on the key but must be ignored
	memberSpend := 4.0
	memberBudget := 65.0
	memberDuration := "7d"
	resetAt := "2026-04-06T00:00:00Z"
	teamID := "team-123"
	userID := "steven.kessler@pitchbook.com"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key/info":
			resp := KeyInfoResponse{
				Info: KeyInfo{
					Spend:  &keySpend,
					TeamID: &teamID,
					UserID: &userID,
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/team/info":
			resp := TeamInfoAPIResponse{
				TeamMemberships: []TeamMembership{
					{
						UserID: userID,
						TeamID: teamID,
						Spend:  &memberSpend,
						LitellmBudgetTable: &TeamMemberBudgetTable{
							MaxBudget:      &memberBudget,
							BudgetDuration: &memberDuration,
							BudgetResetAt:  &resetAt,
						},
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
		t.Errorf("expected TeamMaxBudget=65.0 from membership, got %v", info.TeamMaxBudget)
	}
	if info.TeamSpend == nil || *info.TeamSpend != memberSpend {
		t.Errorf("expected TeamSpend=%v (membership spend), got %v", memberSpend, info.TeamSpend)
	}
	if info.TeamBudgetResetAt == nil || *info.TeamBudgetResetAt != resetAt {
		t.Errorf("expected TeamBudgetResetAt=%q, got %v", resetAt, info.TeamBudgetResetAt)
	}

	// Display shows the membership budget; the key's $16.54 spend never appears.
	t.Setenv("LITELLM_PLUGIN_SHOW_COST", "1")
	result := formatStatusLine(info, "", StatusInput{})
	if !strings.Contains(result, "$4.00/$65.00") {
		t.Errorf("expected $4.00/$65.00 in output, got %q", result)
	}
	if strings.Contains(result, "16.54") {
		t.Errorf("key spend must not appear, got %q", result)
	}
}

// TestGetKeyInfoIgnoresKeySpendWhenNoMembership: when the caller's membership isn't
// found, fall back to the team-level budget table paired with the team total spend —
// never the key's own spend.
func TestGetKeyInfoIgnoresKeySpendWhenNoMembership(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	keySpend := 1.33 // must never be displayed
	memberBudget := 65.0
	memberDuration := "7d"
	resetAt := "2026-04-13T00:00:00Z"
	teamID := "team-pfe-sat"
	userID := "steven.kessler@pitchbook.com"
	otherUserID := "other.user@pitchbook.com"
	otherSpend := 12.80

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key/info":
			resp := KeyInfoResponse{
				Info: KeyInfo{
					Spend:  &keySpend,
					TeamID: &teamID,
					UserID: &userID,
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/team/info":
			// Memberships exist but do NOT include the calling user
			resp := TeamInfoAPIResponse{
				TeamInfo: TeamInfoData{
					Spend:          &otherSpend, // team total
					BudgetDuration: &memberDuration,
					BudgetResetAt:  &resetAt,
					TeamMemberBudgetTable: &TeamMemberBudgetTable{
						MaxBudget:      &memberBudget,
						BudgetDuration: &memberDuration,
					},
				},
				TeamMemberships: []TeamMembership{
					{UserID: otherUserID, TeamID: teamID, Spend: &otherSpend},
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
	if info.TeamMaxBudget == nil || *info.TeamMaxBudget != memberBudget {
		t.Errorf("expected TeamMaxBudget=%v from team-level table, got %v", memberBudget, info.TeamMaxBudget)
	}
	if info.TeamSpend == nil || *info.TeamSpend != otherSpend {
		t.Errorf("expected TeamSpend=%v (team total), got %v", otherSpend, info.TeamSpend)
	}

	t.Setenv("LITELLM_PLUGIN_SHOW_COST", "1")
	result := formatStatusLine(info, "", StatusInput{})
	if !strings.Contains(result, "$12.80/$65.00") {
		t.Errorf("expected $12.80/$65.00 in output, got %q", result)
	}
	if strings.Contains(result, "1.33") {
		t.Errorf("key spend must not appear, got %q", result)
	}
}

func TestGetKeyInfoFallsBackToTeamLevelBudget(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	keySpend := 3.0
	teamBudget := 500.0
	teamSpend := 120.0
	teamDuration := "30d"
	resetAt := "2026-06-01T00:00:00Z"
	teamID := "team-eng"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key/info":
			resp := KeyInfoResponse{
				Info: KeyInfo{
					Spend:  &keySpend,
					TeamID: &teamID,
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/team/info":
			resp := TeamInfoAPIResponse{
				TeamInfo: TeamInfoData{
					Spend:          &teamSpend,
					MaxBudget:      &teamBudget,
					BudgetDuration: &teamDuration,
					BudgetResetAt:  &resetAt,
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
	if info.TeamMaxBudget == nil || *info.TeamMaxBudget != teamBudget {
		t.Errorf("expected TeamMaxBudget=%v, got %v", teamBudget, info.TeamMaxBudget)
	}
	if info.TeamSpend == nil || *info.TeamSpend != teamSpend {
		t.Errorf("expected TeamSpend=%v (team total), got %v", teamSpend, info.TeamSpend)
	}

	t.Setenv("LITELLM_PLUGIN_SHOW_COST", "1")
	result := formatStatusLine(info, "", StatusInput{})
	if !strings.Contains(result, "$120.00/$500.00") {
		t.Errorf("expected $120.00/$500.00 in output, got %q", result)
	}
}

func TestZeroBudgetDivision(t *testing.T) {
	spend := 10.0
	zeroBudget := 0.0

	info := &KeyInfo{
		TeamSpend:     &spend,
		TeamMaxBudget: &zeroBudget,
	}

	// Should not panic on zero budget; a zero team budget reads as "no budget".
	result := formatStatusLine(info, "", StatusInput{})

	if !strings.Contains(result, "no budget configured") {
		t.Errorf("expected no-budget message for zero budget, got %q", result)
	}
}

func TestFormatStatusLineWithModelAndContext(t *testing.T) {
	t.Setenv("LITELLM_PLUGIN_SHOW_COST", "")
	if err := os.Unsetenv("LITELLM_PLUGIN_PREFIX"); err != nil {
		t.Fatal(err)
	}

	spend := 25.0
	budget := 100.0
	info := &KeyInfo{TeamSpend: &spend, TeamMaxBudget: &budget}

	pct := 78.0
	in := StatusInput{}
	in.Model.DisplayName = "Opus 4.7"
	in.ContextWindow = &struct {
		UsedPercentage *float64 `json:"used_percentage"`
	}{UsedPercentage: &pct}

	result := formatStatusLine(info, "", in)

	if !strings.HasPrefix(result, "Opus 4.7: ") {
		t.Errorf("expected model display name as prefix, got %q", result)
	}
	if !strings.Contains(result, "◔") {
		t.Errorf("expected quarter-fill circle for 25%% budget, got %q", result)
	}
	if !strings.Contains(result, "25%") {
		t.Errorf("expected budget percentage, got %q", result)
	}
	if !strings.Contains(result, "📖") {
		t.Errorf("expected context book glyph, got %q", result)
	}
	if !strings.Contains(result, "◕") {
		t.Errorf("expected three-quarter-fill circle for 78%% context, got %q", result)
	}
	if !strings.Contains(result, "78%") {
		t.Errorf("expected context percentage, got %q", result)
	}
	if !strings.Contains(result, "consider /compact") {
		t.Errorf("expected /compact suggestion at 78%%, got %q", result)
	}
}

// TestCacheKeyNamespacing verifies the budget cache is keyed by base URL + token, so
// switching keys/projects never surfaces another config's budget (M1).
func TestCacheKeyNamespacing(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	spend := 42.0
	budget := 100.0
	info := &KeyInfo{Spend: &spend, MaxBudget: &budget}

	t.Setenv("LITELLM_PROXY_URL", "https://a.example")
	t.Setenv("LITELLM_PROXY_API_KEY", "key-a")
	writeBudgetCache(info)
	if _, ok := readBudgetCache(); !ok {
		t.Fatal("expected cache hit for original key+URL")
	}

	// Different token → must miss.
	t.Setenv("LITELLM_PROXY_API_KEY", "key-b")
	if _, ok := readBudgetCache(); ok {
		t.Error("expected cache miss after switching token")
	}

	// Different base URL → must miss.
	t.Setenv("LITELLM_PROXY_API_KEY", "key-a")
	t.Setenv("LITELLM_PROXY_URL", "https://b.example")
	if _, ok := readBudgetCache(); ok {
		t.Error("expected cache miss after switching base URL")
	}

	// Back to the original pair → still cached.
	t.Setenv("LITELLM_PROXY_URL", "https://a.example")
	if _, ok := readBudgetCache(); !ok {
		t.Error("expected cache hit after returning to original key+URL")
	}
}

// TestWriteBudgetCacheConcurrent verifies concurrent writers never leave a torn file
// thanks to atomic write-then-rename (H2). Run with -race for the strongest signal.
func TestWriteBudgetCacheConcurrent(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("LITELLM_PROXY_URL", "https://concurrent.example")
	t.Setenv("LITELLM_PROXY_API_KEY", "key-c")

	spend := 42.0
	budget := 100.0
	info := &KeyInfo{Spend: &spend, MaxBudget: &budget}

	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			writeBudgetCache(info)
		}()
	}
	wg.Wait()

	got, ok := readBudgetCache()
	if !ok {
		t.Fatal("expected a valid (non-torn) cache after concurrent writes")
	}
	if got.Spend == nil || *got.Spend != 42.0 {
		t.Errorf("cache corrupted by concurrent writes: %+v", got)
	}
}

// TestGetKeyInfoNegativeCache verifies a failed fetch is negative-cached so the next
// refresh within the window does not re-hit the network (H1).
func TestGetKeyInfoNegativeCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	t.Setenv("LITELLM_PROXY_URL", "")
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)

	if _, err := getKeyInfo("test-token"); err == nil {
		t.Fatal("expected error on first call")
	}
	if _, err := getKeyInfo("test-token"); err == nil {
		t.Fatal("expected error on second call")
	}
	if callCount != 1 {
		t.Errorf("expected 1 API call (negative cache suppresses the retry), got %d", callCount)
	}
}

// TestGetKeyInfoNegativeCachePreservesAuthError verifies the replayed error still
// satisfies errors.Is(ErrAuth) after being reconstructed from the negative cache (H1).
func TestGetKeyInfoNegativeCachePreservesAuthError(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	t.Setenv("LITELLM_PROXY_URL", "")
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)

	_, err1 := getKeyInfo("bad-token")
	if !errors.Is(err1, ErrAuth) {
		t.Fatalf("expected ErrAuth on first call, got %v", err1)
	}
	_, err2 := getKeyInfo("bad-token")
	if !errors.Is(err2, ErrAuth) {
		t.Errorf("expected ErrAuth replayed from negative cache, got %v", err2)
	}
	if callCount != 1 {
		t.Errorf("expected 1 API call (auth failure negative-cached), got %d", callCount)
	}
}

// TestUpdateCacheNegativeBackoff verifies an empty (failed/rate-limited) update check
// is still cached so the next refresh backs off instead of re-calling GitHub (H1).
func TestUpdateCacheNegativeBackoff(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	writeUpdateCache("")
	v, ok := readUpdateCache()
	if !ok {
		t.Fatal("expected empty update result to be cached for backoff")
	}
	if v != "" {
		t.Errorf("expected empty cached version, got %q", v)
	}
}
