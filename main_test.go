package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFormatTimeUntilReset(t *testing.T) {
	tests := []struct {
		name     string
		resetAt  string
		expected string
	}{
		{
			name:     "empty string",
			resetAt:  "",
			expected: "unknown",
		},
		{
			name:     "invalid format",
			resetAt:  "not-a-date",
			expected: "unknown",
		},
		{
			name:     "past time",
			resetAt:  "2020-01-01T00:00:00Z",
			expected: "resetting",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatTimeUntilReset(tt.resetAt)
			if result != tt.expected {
				t.Errorf("formatTimeUntilReset(%q) = %q, want %q", tt.resetAt, result, tt.expected)
			}
		})
	}

	// Test future times (dynamic)
	t.Run("future time with days", func(t *testing.T) {
		future := time.Now().UTC().Add(50 * time.Hour)
		resetAt := future.Format("2006-01-02T15:04:05Z")
		result := formatTimeUntilReset(resetAt)
		if !strings.Contains(result, "d") {
			t.Errorf("expected result to contain 'd' for days, got %q", result)
		}
	})

	t.Run("future time with hours only", func(t *testing.T) {
		future := time.Now().UTC().Add(5 * time.Hour)
		resetAt := future.Format("2006-01-02T15:04:05Z")
		result := formatTimeUntilReset(resetAt)
		if !strings.Contains(result, "h") {
			t.Errorf("expected result to contain 'h' for hours, got %q", result)
		}
	})

	t.Run("future time with minutes only", func(t *testing.T) {
		future := time.Now().UTC().Add(30 * time.Minute)
		resetAt := future.Format("2006-01-02T15:04:05Z")
		result := formatTimeUntilReset(resetAt)
		if !strings.Contains(result, "m") {
			t.Errorf("expected result to contain 'm' for minutes, got %q", result)
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
				BudgetResetAt: "2020-01-01T00:00:00Z", // past time
			},
			expectColor:    ColorGreen,
			expectContains: []string{"LiteLLM:", "reset:", "resetting"},
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

		response := KeyInfoResponse{
			Info: KeyInfo{
				Spend:         &spend,
				MaxBudget:     &budget,
				BudgetResetAt: "2025-01-15T10:00:00Z",
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
