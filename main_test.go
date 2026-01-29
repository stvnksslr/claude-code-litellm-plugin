package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	t.Run("daily reset", func(t *testing.T) {
		result := calculateNextReset("1d")
		// Should be tomorrow at midnight
		expected := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		if !result.Equal(expected) {
			t.Errorf("calculateNextReset(1d) = %v, want %v", result, expected)
		}
	})

	t.Run("daily reset (24h)", func(t *testing.T) {
		result := calculateNextReset("24h")
		expected := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		if !result.Equal(expected) {
			t.Errorf("calculateNextReset(24h) = %v, want %v", result, expected)
		}
	})

	t.Run("weekly reset", func(t *testing.T) {
		result := calculateNextReset("7d")
		// Should be next Monday at midnight
		if result.Weekday() != time.Monday {
			t.Errorf("calculateNextReset(7d) weekday = %v, want Monday", result.Weekday())
		}
		if result.Before(now) {
			t.Errorf("calculateNextReset(7d) = %v, should be in the future", result)
		}
	})

	t.Run("monthly reset", func(t *testing.T) {
		result := calculateNextReset("30d")
		// Should be 1st of next month at midnight
		if result.Day() != 1 {
			t.Errorf("calculateNextReset(30d) day = %d, want 1", result.Day())
		}
		if result.Before(now) {
			t.Errorf("calculateNextReset(30d) = %v, should be in the future", result)
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatStatusLine(tt.info)

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

func TestGetKeyInfoWithMockServer(t *testing.T) {
	// Reset cache before test
	resetCache()

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

	// Set env var to use test server
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
	// Reset cache before test
	resetCache()

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

	t.Setenv("ANTHROPIC_BASE_URL", server.URL)

	// First call
	_, err := getKeyInfo("test-token")
	if err != nil {
		t.Fatalf("first getKeyInfo() error = %v", err)
	}

	// Second call (should use cache)
	_, err = getKeyInfo("test-token")
	if err != nil {
		t.Fatalf("second getKeyInfo() error = %v", err)
	}

	if callCount != 1 {
		t.Errorf("expected 1 API call (caching), got %d", callCount)
	}
}

func TestGetKeyInfoAuthError(t *testing.T) {
	// Reset cache before test
	resetCache()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	t.Setenv("ANTHROPIC_BASE_URL", server.URL)

	_, err := getKeyInfo("bad-token")
	if err == nil {
		t.Fatal("expected auth error, got nil")
	}

	if !strings.Contains(err.Error(), "auth error") {
		t.Errorf("expected auth error, got %v", err)
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

func TestZeroBudgetDivision(t *testing.T) {
	spend := 10.0
	zeroBudget := 0.0

	info := &KeyInfo{
		Spend:     &spend,
		MaxBudget: &zeroBudget,
	}

	// Should not panic on zero budget
	result := formatStatusLine(info)

	// With zero budget, it should show just the spend (like no budget)
	if !strings.Contains(result, "$10.00") {
		t.Errorf("expected result to contain spend, got %q", result)
	}
}
