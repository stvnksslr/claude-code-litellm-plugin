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

func TestGenerateExamples(t *testing.T) {
	origVersion := Version
	defer func() { Version = origVersion }()
	Version = "v99.0.0"

	weekly := "7d"
	budget := 50.0

	// Layout constants
	padX, padY := 16, 16
	width := 762

	// --- Section 1: Full status line examples (simple → detailed) ---
	statusLabelHeight := 18
	statusLineHeight := 18
	statusRowHeight := statusLabelHeight + statusLineHeight + 8

	type statusExample struct {
		label string
		info  *KeyInfo
		// env holds LITELLM_PLUGIN_* overrides for this example.
		// Keys are auto-appended to the label via pluginEnvLabel.
		env map[string]string
	}

	spend20 := 20.0
	spend30 := 30.0
	resetAt := weeklyResetAt(0.5)
	examples := []statusExample{
		{
			label: "No budget configured",
			info:  &KeyInfo{Spend: &spend30},
		},
		{
			label: "No reset date available",
			info:  &KeyInfo{Spend: &spend30, MaxBudget: &budget},
		},
		{
			label: "Reset date available",
			info:  &KeyInfo{Spend: &spend20, MaxBudget: &budget, BudgetResetAt: &resetAt, BudgetDuration: &weekly},
		},
		{
			label: "Cost hidden",
			info:  &KeyInfo{Spend: &spend20, MaxBudget: &budget, BudgetResetAt: &resetAt, BudgetDuration: &weekly},
			env:   map[string]string{"LITELLM_PLUGIN_SHOW_COST": "0"},
		},
		{
			label: "Custom prefix",
			info:  &KeyInfo{Spend: &spend20, MaxBudget: &budget, BudgetResetAt: &resetAt, BudgetDuration: &weekly},
			env:   map[string]string{"LITELLM_PLUGIN_PREFIX": "💰"},
		},
		{
			label: "No prefix",
			info:  &KeyInfo{Spend: &spend20, MaxBudget: &budget, BudgetResetAt: &resetAt, BudgetDuration: &weekly},
			env:   map[string]string{"LITELLM_PLUGIN_PREFIX": ""},
		},
	}

	statusHeight := len(examples) * statusRowHeight

	// --- Section 2: 3x3 color reference grid ---
	sectionGap := 20
	rowLabelWidth := 130
	colWidth := 200
	headerHeight := 28
	rowHeight := 36
	gridHeight := headerHeight + 3*rowHeight

	height := padY + statusHeight + sectionGap + gridHeight + padY

	var svg strings.Builder
	fmt.Fprintf(&svg, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" font-family="monospace" font-size="14">`, width, height)
	fmt.Fprintf(&svg, `<rect width="%d" height="%d" rx="8" fill="#1e1e2e"/>`, width, height)

	// --- Render status line examples ---
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
			line = formatStatusLine(ex.info, "")
		})
		fmt.Fprintf(&svg, `<text x="%d" y="%d">%s</text>`,
			padX, baseY+statusLabelHeight+statusLineHeight, ansiToSpans(line))
	}

	// --- Render 3x3 grid ---
	type cell struct {
		percent         float64
		elapsedFraction float64
		possible        bool
	}

	grid := [3][3]cell{
		// Green fill (40% spent)
		{
			{40, 0.70, true}, // projected 57%  → green marker
			{40, 0.50, true}, // projected 80%  → yellow marker
			{40, 0.25, true}, // projected 160% → red marker
		},
		// Yellow fill (82% spent)
		{
			{0, 0, false},    // impossible: can't project < 75% with 82% spent
			{82, 0.95, true}, // projected 86%  → yellow marker
			{82, 0.55, true}, // projected 149% → red marker
		},
		// Red fill (95% spent)
		{
			{0, 0, false},    // impossible: can't project < 75% with 95% spent
			{95, 0.98, true}, // projected 97%  → yellow marker
			{95, 0.70, true}, // projected 136% → red marker
		},
	}

	rowLabels := []string{"&lt; 75% spent", "75-90% spent", "&gt; 90% spent"}
	colLabels := []string{"Under pace", "At pace", "Over pace"}

	gridY := padY + statusHeight + sectionGap

	fmt.Fprintf(&svg, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#374151" stroke-width="1"/>`,
		padX, gridY, width-padX, gridY)

	for j, label := range colLabels {
		x := padX + rowLabelWidth + j*colWidth + colWidth/2
		fmt.Fprintf(&svg, `<text x="%d" y="%d" fill="#9ca3af" text-anchor="middle" font-size="12">%s</text>`,
			x, gridY+18, label)
	}
	fmt.Fprintf(&svg, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#374151" stroke-width="1"/>`,
		padX, gridY+headerHeight, width-padX, gridY+headerHeight)

	for i := 0; i < 3; i++ {
		baseY := gridY + headerHeight + i*rowHeight
		textY := baseY + rowHeight/2 + 5

		if i > 0 {
			fmt.Fprintf(&svg, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#374151" stroke-width="1"/>`,
				padX, baseY, width-padX, baseY)
		}

		fmt.Fprintf(&svg, `<text x="%d" y="%d" fill="#9ca3af" font-size="12">%s</text>`,
			padX, textY, rowLabels[i])

		for j := 0; j < 3; j++ {
			c := grid[i][j]
			cellX := padX + rowLabelWidth + j*colWidth

			fmt.Fprintf(&svg, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#374151" stroke-width="1"/>`,
				cellX, gridY+headerHeight, cellX, gridY+gridHeight)

			if !c.possible {
				fmt.Fprintf(&svg, `<text x="%d" y="%d" fill="#4b5563" text-anchor="middle">—</text>`,
					cellX+colWidth/2, textY)
			} else {
				bar := renderProgressBar(c.percent, c.elapsedFraction, true)
				fmt.Fprintf(&svg, `<text x="%d" y="%d">%s</text>`,
					cellX+8, textY, ansiToSpans(bar))
			}
		}
	}

	svg.WriteString(`</svg>`)

	if err := os.WriteFile("examples.svg", []byte(svg.String()), 0644); err != nil {
		t.Fatal(err)
	}
	fmt.Println("wrote examples.svg")
}
