package main

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// weeklyResetAt returns a BudgetResetAt string for a weekly budget where
// the given fraction of the period has already elapsed.
func weeklyResetAt(elapsedFraction float64) string {
	period := 7 * 24 * time.Hour
	remaining := time.Duration(float64(period) * (1 - elapsedFraction))
	return time.Now().UTC().Add(remaining).Format(time.RFC3339)
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

var ansiToSVGColor = map[string]string{
	ColorGreen:  "#22c55e",
	ColorYellow: "#eab308",
	ColorRed:    "#ef4444",
	ColorGray:   "#6b7280",
	ColorReset:  "#d1d5db",
}

func ansiToSpans(s string) string {
	var buf strings.Builder
	color := ansiToSVGColor[ColorReset]
	parts := ansiRe.Split(s, -1)
	codes := ansiRe.FindAllString(s, -1)

	for i, part := range parts {
		if part != "" {
			escaped := strings.ReplaceAll(strings.ReplaceAll(part, "&", "&amp;"), "<", "&lt;")
			fmt.Fprintf(&buf, `<tspan fill="%s">%s</tspan>`, color, escaped)
		}
		if i < len(codes) {
			if c, ok := ansiToSVGColor[codes[i]]; ok {
				color = c
			}
		}
	}
	return buf.String()
}

// pluginEnvLabel builds a parenthetical annotation from any LITELLM_PLUGIN_* keys
// in env, sorted for stable output. Returns "" if no matching keys exist.
func pluginEnvLabel(env map[string]string) string {
	var parts []string
	for k, v := range env {
		if strings.HasPrefix(k, "LITELLM_PLUGIN_") {
			parts = append(parts, k+"="+v)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	return " (" + strings.Join(parts, ", ") + ")"
}

// withEnv sets the given env vars, calls f, then restores the originals.
func withEnv(env map[string]string, f func()) {
	orig := make(map[string]*string, len(env))
	for k, v := range env {
		if cur, ok := os.LookupEnv(k); ok {
			s := cur
			orig[k] = &s
		} else {
			orig[k] = nil
		}
		os.Setenv(k, v) //nolint:errcheck
	}
	f()
	for k, v := range orig {
		if v == nil {
			os.Unsetenv(k) //nolint:errcheck
		} else {
			os.Setenv(k, *v) //nolint:errcheck
		}
	}
}

// ctxInput builds a StatusInput with the given model name and context percentage.
// Pass nil pct to leave the context window absent.
func ctxInput(model string, pct *float64) StatusInput {
	var in StatusInput
	in.Model.DisplayName = model
	if pct != nil {
		in.ContextWindow = &struct {
			UsedPercentage *float64 `json:"used_percentage"`
		}{UsedPercentage: pct}
	}
	return in
}

func f64(v float64) *float64 { return &v }

func TestGenerateExamples(t *testing.T) {
	origVersion := Version
	defer func() { Version = origVersion }()
	Version = "v99.0.0"

	// Ensure no ambient env vars bleed into the rendered examples.
	if err := os.Unsetenv("LITELLM_PLUGIN_SHOW_COST"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("LITELLM_PLUGIN_PREFIX"); err != nil {
		t.Fatal(err)
	}

	weekly := "7d"
	budget := 50.0

	// Layout constants
	padX, padY := 16, 16
	width := 920

	statusLabelHeight := 18
	statusLineHeight := 18
	statusRowHeight := statusLabelHeight + statusLineHeight + 8

	type statusExample struct {
		label string
		info  *KeyInfo
		input StatusInput
		// env holds LITELLM_PLUGIN_* overrides for this example.
		// Keys are auto-appended to the label via pluginEnvLabel.
		env map[string]string
	}

	// Spends chosen so circle glyphs span all five buckets (○ ◔ ◑ ◕ ●).
	spend0 := 0.0   // 0%   → ○
	spend10 := 10.0 // 20%  → ◔
	spend20 := 20.0 // 40%  → ◑
	spend35 := 35.0 // 70%  → ◕
	spend48 := 48.0 // 96%  → ●
	resetAt := weeklyResetAt(0.5)
	// Budgets are team-member budgets — the only budget the statusline displays.
	examples := []statusExample{
		{
			label: "Empty budget (0%) — ○",
			info:  &KeyInfo{TeamSpend: &spend0, TeamMaxBudget: &budget, TeamBudgetResetAt: &resetAt, TeamBudgetDuration: &weekly},
			input: ctxInput("Opus 4.7", nil),
		},
		{
			label: "Quarter budget (20%) — ◔",
			info:  &KeyInfo{TeamSpend: &spend10, TeamMaxBudget: &budget, TeamBudgetResetAt: &resetAt, TeamBudgetDuration: &weekly},
			input: ctxInput("Opus 4.7", nil),
		},
		{
			label: "Half budget (40%) — ◑",
			info:  &KeyInfo{TeamSpend: &spend20, TeamMaxBudget: &budget, TeamBudgetResetAt: &resetAt, TeamBudgetDuration: &weekly},
			input: ctxInput("Opus 4.7", nil),
		},
		{
			label: "Three-quarter budget (70%) — ◕",
			info:  &KeyInfo{TeamSpend: &spend35, TeamMaxBudget: &budget, TeamBudgetResetAt: &resetAt, TeamBudgetDuration: &weekly},
			input: ctxInput("Opus 4.7", nil),
		},
		{
			label: "Full budget (96%) — ●",
			info:  &KeyInfo{TeamSpend: &spend48, TeamMaxBudget: &budget, TeamBudgetResetAt: &resetAt, TeamBudgetDuration: &weekly},
			input: ctxInput("Opus 4.7", nil),
		},
		{
			label: "Context segment — low (no suggestion)",
			info:  &KeyInfo{TeamSpend: &spend20, TeamMaxBudget: &budget, TeamBudgetResetAt: &resetAt, TeamBudgetDuration: &weekly},
			input: ctxInput("Opus 4.7", f64(30)),
		},
		{
			label: "Context segment — warn (consider /compact)",
			info:  &KeyInfo{TeamSpend: &spend20, TeamMaxBudget: &budget, TeamBudgetResetAt: &resetAt, TeamBudgetDuration: &weekly},
			input: ctxInput("Opus 4.7", f64(78)),
		},
		{
			label: "Context segment — critical (run /compact or /clear)",
			info:  &KeyInfo{TeamSpend: &spend20, TeamMaxBudget: &budget, TeamBudgetResetAt: &resetAt, TeamBudgetDuration: &weekly},
			input: ctxInput("Opus 4.7", f64(92)),
		},
		{
			label: "Dollar amounts restored",
			info:  &KeyInfo{TeamSpend: &spend20, TeamMaxBudget: &budget, TeamBudgetResetAt: &resetAt, TeamBudgetDuration: &weekly},
			input: ctxInput("Opus 4.7", f64(45)),
			env:   map[string]string{"LITELLM_PLUGIN_SHOW_COST": "1"},
		},
		{
			label: "Custom prefix wins over model name",
			info:  &KeyInfo{TeamSpend: &spend20, TeamMaxBudget: &budget, TeamBudgetResetAt: &resetAt, TeamBudgetDuration: &weekly},
			input: ctxInput("Opus 4.7", f64(45)),
			env:   map[string]string{"LITELLM_PLUGIN_PREFIX": "💰"},
		},
		{
			// Key spend present but no team budget → the red no-budget state (key spend is ignored).
			label: "No budget configured",
			info:  &KeyInfo{Spend: &spend20},
			input: ctxInput("Opus 4.7", f64(45)),
		},
		{
			label: "Team budget",
			info: &KeyInfo{
				TeamSpend:          &spend20,
				TeamMaxBudget:      &budget,
				TeamBudgetResetAt:  &resetAt,
				TeamBudgetDuration: &weekly,
			},
			input: ctxInput("Sonnet 4.6", f64(45)),
		},
		{
			label: "Falls back to LiteLLM: when no stdin",
			info:  &KeyInfo{TeamSpend: &spend20, TeamMaxBudget: &budget, TeamBudgetResetAt: &resetAt, TeamBudgetDuration: &weekly},
		},
	}

	statusHeight := len(examples) * statusRowHeight
	height := padY + statusHeight + padY

	var svg strings.Builder
	fmt.Fprintf(&svg, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" font-family="monospace" font-size="14">`, width, height)
	fmt.Fprintf(&svg, `<rect width="%d" height="%d" rx="8" fill="#1e1e2e"/>`, width, height)

	for i, ex := range examples {
		baseY := padY + i*statusRowHeight

		if i > 0 {
			fmt.Fprintf(&svg, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#374151" stroke-width="1"/>`,
				padX, baseY, width-padX, baseY)
		}

		label := ex.label + pluginEnvLabel(ex.env)
		fmt.Fprintf(&svg, `<text x="%d" y="%d" fill="#9ca3af" font-size="12">%s</text>`,
			padX, baseY+statusLabelHeight, label)

		var line string
		withEnv(ex.env, func() {
			line = formatStatusLine(ex.info, "", ex.input)
		})
		fmt.Fprintf(&svg, `<text x="%d" y="%d">%s</text>`,
			padX, baseY+statusLabelHeight+statusLineHeight, ansiToSpans(line))
	}

	svg.WriteString(`</svg>`)

	if err := os.WriteFile("examples.svg", []byte(svg.String()), 0644); err != nil {
		t.Fatal(err)
	}
	fmt.Println("wrote examples.svg")
}
