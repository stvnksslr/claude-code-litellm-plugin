package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
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
	// CacheTTLMs is the fresh-cache window for successful budget fetches. The
	// statusline refreshes every few seconds; this TTL — not the refresh rate —
	// is what sets the request volume against the proxy (2 requests per TTL
	// per active user).
	CacheTTLMs       = 60_000          // 60 seconds in milliseconds
	BudgetFailTTLMs  = 10_000          // legacy flat negative-cache window (pre-backoff entries)
	HTTPTimeout      = 3 * time.Second // fast failure for subprocess/statusline use
	UpdateCheckTTLMs = 60 * 60 * 1_000 // 1 hour in milliseconds
	// UpdateCheckTimeout is short because the check runs alongside the budget fetch
	// and editors kill the binary after a few seconds.
	UpdateCheckTimeout = 2 * time.Second
	// BudgetDeadline bounds the whole budget fetch (/key/info or /user/info plus
	// /team/info), so the binary answers before editor plugins time it out.
	BudgetDeadline = 4 * time.Second
	// Negative-cache backoff for failed budget fetches: escalates per
	// consecutive failure and carries jitter so a large population doesn't
	// retry against the proxy in lockstep (see writeBudgetFailCache).
	BudgetFailBaseMs   = 10_000 // first-failure backoff
	BudgetFailCapMs    = 60_000 // backoff cap for consecutive failures
	BudgetFailJitterMs = 5_000  // uniform jitter added on top of the backoff
	// StaleCacheMaxAgeMs caps how old a budget cache entry may be and still be
	// shown as last-known data when the proxy is unreachable (see budgetInfo).
	// Beyond this the data is too old to present as the current budget.
	StaleCacheMaxAgeMs = 6 * 60 * 60 * 1_000 // 6 hours in milliseconds
	// inflightFreshMs bounds how long an in-flight fetch marker is honored. It
	// must outlive the longest legitimate fetch (BudgetDeadline) so a live
	// fetch is never double-started, but stay short enough that a process
	// killed mid-fetch can't stall refreshes for long.
	inflightFreshMs = 15_000
	// CacheTTLJitterMs is the ±jitter applied to the fresh-cache TTL at write
	// time, so a fleet-wide restart (everyone's Claude Code updating at once)
	// doesn't phase-lock every user's refresh cycle to the same moment.
	CacheTTLJitterMs = 6_000
)

// ErrAuth is returned when the API responds with a 401 or 403 status.
var ErrAuth = errors.New("auth error")

// ErrNoAPIKey is returned when neither LITELLM_PROXY_API_KEY nor ANTHROPIC_AUTH_TOKEN is set
// and Claude Code's credential store holds no usable LiteLLM gateway SSO token.
var ErrNoAPIKey = errors.New("no api key")

// ErrNoBaseURL is returned when a token is available but neither LITELLM_PROXY_URL
// nor ANTHROPIC_BASE_URL is set (and no gateway URL could be derived).
var ErrNoBaseURL = errors.New("no LiteLLM proxy URL configured (set LITELLM_PROXY_URL or ANTHROPIC_BASE_URL)")

// ErrBudgetExceeded is returned when the API reports the key's budget has been exceeded.
var ErrBudgetExceeded = errors.New("budget exceeded")

// ErrTransport marks network-level failures (timeouts, dial errors, truncated
// transfers). Fetchers don't return it directly — isTransportErr detects their
// errors structurally — but replayed negative-cache entries carry it as a
// sentinel so classification survives the loss of the original error value.
var ErrTransport = errors.New("transport error")

// httpStatusError is a non-2xx response that isn't an auth or budget error.
type httpStatusError struct {
	Status int
	URL    string
	Body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("HTTP error: status=%d url=%s body=%s", e.Status, e.URL, e.Body)
}

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
	// FreshTTL is the jittered fresh-window (ms) picked at write time, so all
	// readers agree on one expiry instead of re-rolling jitter per read.
	// Zero on legacy entries — those fall back to the flat CacheTTLMs.
	FreshTTL int64 `json:"fresh_ttl,omitempty"`
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
	Kind      string  `json:"kind"`                 // "auth" | "budget" | "transport" | "other"
	Message   string  `json:"message,omitempty"`    // original error text, for debug output
	Spend     float64 `json:"spend,omitempty"`      // populated when Kind == "budget"
	MaxBudget float64 `json:"max_budget,omitempty"` // populated when Kind == "budget"
	// BackoffUntil is the jittered, escalating retry deadline (Unix ms) written
	// by writeBudgetFailCache; entries replay their cached error until then.
	// Zero on legacy entries — those fall back to the flat BudgetFailTTLMs.
	BackoffUntil int64 `json:"backoff_until,omitempty"`
	// Attempts counts consecutive failures for the backoff ladder; resets when
	// the previous failure is old enough that the proxy likely recovered.
	Attempts int `json:"attempts,omitempty"`
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

	// stale marks that this info was not freshly fetched in this call —
	// either the stale-cache fallback after a transport failure, or a
	// deferral while another process holds the in-flight fetch. Renderers
	// label it "(cached)". Unexported: it is a render-time flag, never
	// marshaled into the cache file or parsed from API responses — only a
	// real fetch sets fresh data.
	stale bool
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
			stale:          info.stale,
		}
	}
	// Preserve the flag in the no-budget branch too: stale data that said
	// "no budget" must still render its "(cached)" label.
	return &KeyInfo{stale: info.stale}
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
	Effort struct {
		Level string `json:"level"`
	} `json:"effort"`
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
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Never fall back to a relative path: Claude Code runs us from the project dir.
		return filepath.Join(os.TempDir(), "claude-code-litellm")
	}
	return filepath.Join(home, ".cache", "claude-code-litellm")
}

// writeCacheFile writes one cache file atomically, creating the cache dir and
// sweeping temp files orphaned by killed processes. Best-effort.
func writeCacheFile(path string, data []byte) {
	dir := cacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	sweepStaleTemps(dir)
	_ = writeFileAtomic(path, data, 0o600)
}

// cacheKey returns a short, stable hash of the credential's base URL + token so
// budget cache files don't bleed across different proxies/keys (e.g. per-project
// configs that point at different LiteLLM instances or use different keys). It
// hashes the credential actually in flight — not a re-resolution of the ambient
// env/Keychain state, which can change between resolution and cache access.
func cacheKey(c credential) string {
	sum := sha256.Sum256([]byte(c.baseURL + "\x00" + c.token))
	return hex.EncodeToString(sum[:])[:12]
}

func budgetCacheFile(c credential) string {
	return filepath.Join(cacheDir(), "budget-"+cacheKey(c)+".json")
}

// budgetFailCacheFile holds the negative-cache marker for failed budget fetches,
// namespaced per-key so a failure for one key doesn't suppress fetches for another.
func budgetFailCacheFile(c credential) string {
	return filepath.Join(cacheDir(), "budget-fail-"+cacheKey(c)+".json")
}

// budgetInflightFile marks a credential's budget fetch as in progress. A
// user's sessions share the cache dir, so concurrent refreshes (and Claude
// Code's cancel-and-respawn of a slow refresh) can defer to whichever process
// is already fetching instead of racing it against the proxy.
func budgetInflightFile(c credential) string {
	return filepath.Join(cacheDir(), "budget-inflight-"+cacheKey(c)+".json")
}

// inflightEntry is the on-disk in-flight fetch marker. Owner holds a per-claim
// nonce so cleanup only ever removes the marker its own process wrote — a slow
// process finishing after another took over its orphaned marker must not
// delete the successor's claim.
type inflightEntry struct {
	Timestamp int64  `json:"timestamp"`       // Unix milliseconds
	Owner     string `json:"owner,omitempty"` // claim nonce
}

// inflightActive reports whether a fresh marker exists — some process is
// (or was, within inflightFreshMs) already fetching this credential.
func inflightActive(c credential) bool {
	data, err := os.ReadFile(budgetInflightFile(c))
	if err != nil {
		return false
	}
	var entry inflightEntry
	if json.Unmarshal(data, &entry) != nil {
		return false
	}
	return cacheFresh(entry.Timestamp, inflightFreshMs)
}

// claimInflight creates the marker exclusively: exactly one concurrent process
// wins the fetch for a credential; losers serve the stale cache. A marker left
// by a killed process never runs its cleanup, so a stale or corrupt marker is
// an orphan — remove it and take over. Returns the claim nonce on success.
func claimInflight(c credential) (string, bool) {
	if nonce, ok := claimMarker(c); ok {
		return nonce, true
	}
	// Orphan takeover: the marker is stale or unreadable. Detach it with a
	// rename rather than a remove — exactly one concurrent taker's rename
	// succeeds (a loser gets ENOENT and just fails to claim).
	if inflightActive(c) {
		return "", false
	}
	takeover := budgetInflightFile(c) + ".tmp-takeover-" + strconv.FormatInt(rand.Int64N(1<<62), 36)
	if err := os.Rename(budgetInflightFile(c), takeover); err == nil {
		// A sibling may have claimed a fresh marker between our check and the
		// rename; the detached file tells us. If it was fresh, restore it —
		// via link(2), so a claim that landed in between is never clobbered.
		if data, rerr := os.ReadFile(takeover); rerr == nil {
			var e inflightEntry
			if json.Unmarshal(data, &e) == nil && cacheFresh(e.Timestamp, inflightFreshMs) {
				_ = os.Link(takeover, budgetInflightFile(c))
				_ = os.Remove(takeover)
				return "", false
			}
		}
		if nonce, ok := claimMarker(c); ok {
			return nonce, true
		}
		// Someone re-claimed between our rename and claim — defer to them.
	}
	return "", false
}

func claimMarker(c credential) (string, bool) {
	if err := os.MkdirAll(cacheDir(), 0o755); err != nil {
		return "", false
	}
	nonce := strconv.FormatInt(rand.Int64N(1<<62), 36)
	data, err := json.Marshal(inflightEntry{Timestamp: time.Now().UnixMilli(), Owner: nonce})
	if err != nil {
		return "", false
	}
	// Publish atomically via link(2): the marker never exists as an empty or
	// half-written file, so a reader either sees a complete claim or none.
	// EEXIST means the claim was taken — there is nothing to clean up.
	tmp := budgetInflightFile(c) + ".tmp-" + nonce
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		_ = os.Remove(tmp)
		return "", false
	}
	if err := os.Link(tmp, budgetInflightFile(c)); err != nil {
		_ = os.Remove(tmp)
		return "", false
	}
	_ = os.Remove(tmp)
	return nonce, true
}

// clearInflightMarker removes the marker only if this process still owns it.
func clearInflightMarker(c credential, nonce string) {
	if nonce == "" {
		return
	}
	data, err := os.ReadFile(budgetInflightFile(c))
	if err != nil {
		return
	}
	var entry inflightEntry
	if json.Unmarshal(data, &entry) != nil || entry.Owner != nonce {
		return // someone else's (or unreadable) claim — leave it alone
	}
	// A stale-or-older marker is takeover-eligible; removing it here could
	// delete a successor's fresh claim that replaced ours since the read.
	// Only remove a marker that is still fresh — ours can only be, since a
	// live fetch is bounded well below inflightFreshMs.
	if !cacheFresh(entry.Timestamp, inflightFreshMs) {
		return
	}
	_ = os.Remove(budgetInflightFile(c))
}

// clearBudgetFailCache drops the negative-cache record, resetting the backoff
// ladder after a successful fetch.
func clearBudgetFailCache(c credential) {
	_ = os.Remove(budgetFailCacheFile(c))
}

// authoritativeFailRecord reports whether the fail cache holds an unresolved
// auth/budget record — even one past its backoff. Stale data must not mask
// those states; only a successful fetch (or a fresher authoritative error)
// resolves them.
func authoritativeFailRecord(c credential) bool {
	entry, ok := readBudgetFailCacheEntry(c)
	return ok && (entry.Kind == "auth" || entry.Kind == "budget")
}

// updateCacheFile is intentionally NOT namespaced by key: the latest GitHub release
// is identical regardless of which LiteLLM key/URL is in use, and a shared file means
// a single backoff is honored across keys (fewer GitHub calls under rate limits).
func updateCacheFile() string {
	return filepath.Join(cacheDir(), "update.json")
}

// staleTempAge is how old an orphaned "*.tmp-*" file must be before it is swept.
// Claude Code cancels in-flight statusline processes when a new refresh starts, which
// can kill us between CreateTemp and Rename and leave the temp file behind.
const staleTempAge = time.Minute

// sweepStaleTemps removes orphaned temp files left in dir by killed processes. The age
// threshold keeps it from racing a concurrent process that is mid-write.
func sweepStaleTemps(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && time.Since(fi.ModTime()) > staleTempAge {
			_ = os.Remove(m)
		}
	}
}

// writeFileAtomic writes data to path atomically: it writes to a uniquely-named temp
// file in the same directory, then renames it into place. Rename is atomic on the same
// filesystem, so concurrent statusline processes (or goroutines) never observe a torn
// file. The directory must already exist. It never touches other files in the
// directory (callers that own the directory sweep orphans via writeCacheFile).
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
	// Sync before rename: without it a crash/power loss can durably commit the
	// rename while the data blocks are not, leaving a zero-length or garbage
	// target file that silently disables the stale-cache fallback.
	if err := f.Sync(); err != nil {
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
// cacheFresh reports whether a cache entry written at ts (Unix ms) is still
// within ttl. Negative ages (clock stepped back after the write) count as
// expired so a backwards clock adjustment can't extend an entry's life.
func cacheFresh(ts, ttl int64) bool {
	age := time.Now().UnixMilli() - ts
	return age >= 0 && age < ttl
}

// readBudgetCacheEntry reads and parses the cached budget entry regardless of
// age. Returns nil, false when the file is missing or corrupt. A corrupt file
// is removed: nothing rewrites it except a successful fetch, so leaving it in
// place would disable the stale fallback for the whole outage.
func readBudgetCacheEntry(c credential) (*BudgetCacheEntry, bool) {
	data, err := os.ReadFile(budgetCacheFile(c))
	if err != nil {
		return nil, false
	}
	var entry BudgetCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		_ = os.Remove(budgetCacheFile(c))
		return nil, false
	}
	return &entry, true
}

func readBudgetCache(c credential) (*KeyInfo, bool) {
	entry, ok := readBudgetCacheEntry(c)
	if !ok {
		return nil, false
	}
	ttl := int64(CacheTTLMs)
	if entry.FreshTTL > 0 {
		ttl = entry.FreshTTL
	}
	if !cacheFresh(entry.Timestamp, ttl) {
		return nil, false
	}
	return &entry.Info, true
}

// readBudgetCacheStale returns cached budget info of any age within
// StaleCacheMaxAgeMs, ignoring the fresh TTL. Used to degrade to last-known
// data when the proxy is unreachable. It never refreshes the entry — only a
// successful fetch writes a new timestamp via writeBudgetCache.
func readBudgetCacheStale(c credential) (*KeyInfo, bool) {
	entry, ok := readBudgetCacheEntry(c)
	if !ok || !cacheFresh(entry.Timestamp, StaleCacheMaxAgeMs) {
		return nil, false
	}
	return &entry.Info, true
}

// isTransportErr reports whether err is a network-level failure — timeout, dial
// failure, connection refused/reset, truncated transfer — as opposed to an
// HTTP-status or parse failure. Structural checks come first (they survive
// fmt.Errorf %w wrapping and don't depend on error text); the case-insensitive
// substring match is a fallback for replayed negative-cache entries, which
// persist only their message text.
func isTransportErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTransport) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// Any net/http round-trip failure: dial, reset, refused, mid-body EOF.
		return true
	}
	// Authoritative answers are never transport, even when their error text
	// echoes a URL or proxy body that happens to contain the substrings below.
	var hErr *httpStatusError
	if errors.As(err, &hErr) {
		return false
	}
	if errors.Is(err, ErrAuth) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "connection") ||
		strings.Contains(msg, "dial")
}

// writeBudgetCache writes budget info to the filesystem cache.
// Errors are silently ignored — caching is best-effort.
func writeBudgetCache(c credential, info *KeyInfo) {
	if info == nil {
		return
	}
	entry := BudgetCacheEntry{
		Timestamp: time.Now().UnixMilli(),
		Info:      *info,
		// Jitter the fresh window once, at write time: a fleet restarted in
		// sync expires its caches on a spread, not in one wave.
		FreshTTL: CacheTTLMs + rand.Int64N(CacheTTLJitterMs*2) - CacheTTLJitterMs,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	writeCacheFile(budgetCacheFile(c), data)
}

// readBudgetFailCache returns a recent failed-fetch record, if one exists within
// BudgetFailTTLMs. Returns nil, false when absent, corrupt, or expired.
// readBudgetFailCacheEntry reads and parses the fail entry regardless of
// freshness. Returns nil, false when missing or corrupt.
func readBudgetFailCacheEntry(c credential) (*BudgetFailEntry, bool) {
	data, err := os.ReadFile(budgetFailCacheFile(c))
	if err != nil {
		return nil, false
	}
	var entry BudgetFailEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, false
	}
	return &entry, true
}

// readBudgetFailCache returns an active failed-fetch record — one whose
// backoff window (or, for legacy entries, flat TTL) has not expired. Returns
// nil, false when absent, corrupt, or expired.
func readBudgetFailCache(c credential) (*BudgetFailEntry, bool) {
	entry, ok := readBudgetFailCacheEntry(c)
	if !ok {
		return nil, false
	}
	if entry.BackoffUntil > 0 {
		// The backoff never legitimately exceeds cap+jitter; a larger remaining
		// window means the clock stepped backwards after the write — expired.
		remaining := entry.BackoffUntil - time.Now().UnixMilli()
		if remaining > 0 && remaining <= BudgetFailCapMs+BudgetFailJitterMs {
			return entry, true
		}
		return nil, false
	}
	if !cacheFresh(entry.Timestamp, BudgetFailTTLMs) {
		return nil, false
	}
	return entry, true
}

// writeBudgetFailCache records a failed budget fetch so subsequent refreshes back off
// instead of re-blocking on the network. Errors are silently ignored — best-effort.
func writeBudgetFailCache(c credential, fetchErr error) {
	if c.token == "" {
		return
	}
	now := time.Now().UnixMilli()
	entry := BudgetFailEntry{
		Timestamp: now,
		Message:   fetchErr.Error(),
	}
	prev, prevOK := readBudgetFailCacheEntry(c)
	// Escalate on consecutive failures; a long gap since the previous one
	// means the proxy likely recovered in between, so restart the ladder.
	if prevOK && now-prev.Timestamp <= 2*BudgetFailCapMs {
		entry.Attempts = prev.Attempts + 1
	} else {
		entry.Attempts = 1
	}
	shift := entry.Attempts - 1
	if shift > 6 { // 10s<<6 already exceeds the cap; the cap governs beyond this
		shift = 6
	}
	backoff := int64(BudgetFailBaseMs) << uint(shift)
	if backoff > BudgetFailCapMs {
		backoff = BudgetFailCapMs
	}
	entry.BackoffUntil = now + backoff + rand.Int64N(BudgetFailJitterMs)
	var bErr *BudgetExceededError
	switch {
	case errors.As(fetchErr, &bErr):
		entry.Kind = "budget"
		entry.Spend = bErr.Spend
		entry.MaxBudget = bErr.MaxBudget
	case errors.Is(fetchErr, ErrAuth):
		entry.Kind = "auth"
	case isTransportErr(fetchErr):
		entry.Kind = "transport"
	default:
		// HTTP-status and parse failures are authoritative answers, not
		// outages — the replay path must not degrade them to stale cache.
		entry.Kind = "other"
	}
	// An authoritative record (revoked key, exceeded budget) outlives a
	// transport blip: preserve its kind and payload so the replay path keeps
	// surfacing the real state instead of degrading to stale cache, while
	// still restarting the backoff clock from this failure.
	if prevOK && (prev.Kind == "auth" || prev.Kind == "budget") &&
		(entry.Kind == "transport" || entry.Kind == "other") {
		entry.Kind = prev.Kind
		entry.Message = prev.Message
		entry.Spend = prev.Spend
		entry.MaxBudget = prev.MaxBudget
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	writeCacheFile(budgetFailCacheFile(c), data)
}

// errorFromFailEntry rebuilds an error equivalent to the original failed fetch so
// callers (and main()'s error classification) behave identically without a network call.
func errorFromFailEntry(e *BudgetFailEntry) error {
	switch e.Kind {
	case "budget":
		return &BudgetExceededError{Spend: e.Spend, MaxBudget: e.MaxBudget}
	case "auth":
		return &cachedError{msg: e.Message, sentinel: ErrAuth}
	case "transport":
		// ErrTransport keeps replayed transport failures classifying
		// structurally — the message text alone is not load-bearing.
		return &cachedError{msg: e.Message, sentinel: ErrTransport}
	default:
		return &cachedError{msg: e.Message}
	}
}

// readUpdateCache reads the cached latest GitHub release version from disk.
// Returns "", false if the cache is missing, corrupt, or older than UpdateCheckTTLMs.
// readUpdateCacheEntry reads and parses the update cache regardless of
// freshness. Returns nil, false when missing or corrupt.
func readUpdateCacheEntry() (*UpdateCacheEntry, bool) {
	data, err := os.ReadFile(updateCacheFile())
	if err != nil {
		return nil, false
	}
	var entry UpdateCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, false
	}
	return &entry, true
}

func readUpdateCache() (string, bool) {
	entry, ok := readUpdateCacheEntry()
	if !ok {
		return "", false
	}
	if !cacheFresh(entry.Timestamp, UpdateCheckTTLMs) {
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
	writeCacheFile(updateCacheFile(), data)
}

// fetchLatestVersion calls the GitHub releases API to get the latest release tag.
// A var so tests can stub the network call.
var fetchLatestVersion = func() string {
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
// Returns "" when update checks are disabled (dev builds, LITELLM_PLUGIN_UPDATE_CHECK=0).
func getLatestVersion() string {
	if !updateCheckEnabled() {
		return ""
	}
	if version, ok := readUpdateCache(); ok {
		return version
	}
	prior, _ := readUpdateCacheEntry()
	// Claim the slot before going to the network: if this process is killed
	// mid-request (editors time out, Claude Code cancels in-flight runs), the
	// next run still backs off for the full TTL instead of retrying forever.
	writeUpdateCache("")
	latest := fetchLatestVersion()
	if latest == "" {
		// Fetch failed (network/rate limit): the pre-written empty entry keeps
		// the backoff, but re-persist the last known version so a transient
		// failure doesn't erase the update notice for the full hour. Re-read
		// the disk entry first: a concurrent sibling may have fetched a real
		// version while we were failing — never overwrite its success.
		if cur, ok := readUpdateCacheEntry(); ok && cur.LatestVersion != "" && cacheFresh(cur.Timestamp, UpdateCheckTTLMs) {
			return cur.LatestVersion
		}
		if prior != nil && prior.LatestVersion != "" {
			writeUpdateCache(prior.LatestVersion)
			return prior.LatestVersion
		}
		return ""
	}
	writeUpdateCache(latest)
	return latest
}

// updateCheckEnabled reports whether the GitHub latest-release check may run.
func updateCheckEnabled() bool {
	if Version == "dev" {
		return false
	}
	switch strings.ToLower(os.Getenv("LITELLM_PLUGIN_UPDATE_CHECK")) {
	case "0", "false", "off", "no":
		return false
	}
	return true
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

// credential is the auth token for one run plus the proxy base URL it may be
// sent to. Budget fetches and cache namespacing use these fields rather than
// re-resolving ambient env/Keychain state, which can change mid-run.
type credential struct {
	token   string
	baseURL string
	// gateway marks a LiteLLM gateway SSO token (enterpriseGateway.jwt).
	// Gateway tokens authenticate but are not DB key rows, so budgets come
	// from /user/info instead of /key/info.
	gateway bool
	// source names where the token came from (env var name, credentials file,
	// macOS Keychain) for diagnostics. Never the token itself.
	source string
}

// gatewayPathSuffix is the path Claude Code appends to the LiteLLM issuer when
// it stores the gateway URL (e.g. https://litellm.example/claude_code_gateway).
const gatewayPathSuffix = "/claude_code_gateway"

// resolveCredential returns the auth token and the base URL it is bound to.
// Precedence:
//  1. LITELLM_PROXY_API_KEY env var
//  2. ANTHROPIC_AUTH_TOKEN env var
//  3. the LiteLLM gateway SSO token (enterpriseGateway.jwt) from Claude Code's
//     credential store
//
// API-key mode is unchanged — env vars win exactly as before. The credential
// store is only consulted when neither env var is set, and its token is only
// ever sent to the gateway that issued it, over HTTPS (loopback excepted).
// Claude Code's Anthropic OAuth token (claudeAiOauth) is deliberately never
// used: it is a first-party Anthropic credential, not a LiteLLM one.
func resolveCredential() credential {
	c, _ := resolveCredentialWithNotes()
	return c
}

// resolveCredentialWithNotes is resolveCredential plus human-readable, non-secret
// notes explaining what was found and why a stored token was skipped (used by
// `doctor`). The resolution logic and security invariants are identical.
func resolveCredentialWithNotes() (credential, []string) {
	var notes []string
	baseURL := getBaseURL()
	for _, name := range []string{"LITELLM_PROXY_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if tok := os.Getenv(name); tok != "" {
			return credential{token: tok, baseURL: baseURL, source: name}, notes
		}
	}
	if skip := keychainSkipReason(); skip != "" {
		notes = append(notes, skip)
	}
	for _, src := range credentialSources() {
		payload := src.read()
		if len(payload) == 0 {
			continue
		}
		jwt, gatewayURL := extractGatewayToken(payload)
		if jwt == "" {
			notes = append(notes, src.name+": no unexpired LiteLLM gateway token (run /login in Claude Code)")
			continue
		}
		if target, ok := gatewayTarget(baseURL, gatewayURL); ok {
			return credential{token: jwt, baseURL: target, gateway: true, source: src.name}, notes
		}
		notes = append(notes, fmt.Sprintf("%s: gateway token issued by %s was withheld: base URL %q is not the same HTTPS origin",
			src.name, redactURL(gatewayURL), baseURL))
	}
	return credential{baseURL: baseURL}, notes
}

// redactURL keeps scheme, host and path of a URL for diagnostics (drops query/userinfo).
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(unparseable URL)"
	}
	return u.Scheme + "://" + u.Host + u.Path
}

// extractGatewayToken returns the LiteLLM gateway SSO token and the gateway URL
// it was issued for from a credential-store payload. The store shape is
// {"enterpriseGateway":{"jwt":"…","url":"https://…/claude_code_gateway","expiresAt":1790782037250},…}
// with expiresAt in epoch milliseconds. Returns "" when absent, expired,
// unparsable, or missing its URL (an unbound token can't be safely sent).
//
// Expiry comes only from the store's expiresAt: LiteLLM encrypts the token
// (it is not a plain signed JWT), so its own expiry claim can't be read
// client-side. The proxy still rejects a lapsed token with 401.
func extractGatewayToken(payload []byte) (jwt, gatewayURL string) {
	var creds struct {
		EnterpriseGateway *struct {
			JWT       string   `json:"jwt"`
			URL       string   `json:"url"`
			ExpiresAt *float64 `json:"expiresAt"`
		} `json:"enterpriseGateway"`
	}
	if err := json.Unmarshal(payload, &creds); err != nil || creds.EnterpriseGateway == nil {
		return "", ""
	}
	gw := creds.EnterpriseGateway
	if gw.JWT == "" || gw.URL == "" || expired(gw.ExpiresAt) {
		return "", ""
	}
	return gw.JWT, gw.URL
}

// gatewayTarget decides where a gateway token may be sent. The token is bound
// to the gateway that issued it: with a configured base URL, the two must share
// scheme, host, and port; with none, the stored gateway URL (minus the
// /claude_code_gateway suffix) becomes the base URL. Either way the target must
// use HTTPS, or plain HTTP to a loopback host.
func gatewayTarget(baseURL, gatewayURL string) (string, bool) {
	gw, err := url.Parse(gatewayURL)
	if err != nil || gw.Host == "" || !secureTransport(gw) {
		return "", false
	}
	if baseURL == "" {
		return strings.TrimSuffix(strings.TrimSuffix(gw.Scheme+"://"+gw.Host+gw.Path, "/"), gatewayPathSuffix), true
	}
	base, err := url.Parse(baseURL)
	if err != nil || !sameOrigin(base, gw) {
		return "", false
	}
	return baseURL, true
}

// secureTransport reports whether a credential-store token may travel to u:
// HTTPS always, plain HTTP only to a loopback host (local proxies/tests).
func secureTransport(u *url.URL) bool {
	switch strings.ToLower(u.Scheme) {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	return false
}

// sameOrigin compares scheme, host, and effective port (default ports filled in).
func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if strings.EqualFold(u.Scheme, "https") {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		port(a) == port(b)
}

// expired reports whether an epoch-milliseconds timestamp has passed. A nil
// timestamp (unknown expiry) is treated as still valid.
func expired(ms *float64) bool {
	return ms != nil && time.Now().After(time.UnixMilli(int64(*ms)))
}

// defaultClaudeConfigDir is ~/.claude, or "" when the home dir is unknown.
func defaultClaudeConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// claudeConfigDir is Claude Code's config dir: $CLAUDE_CONFIG_DIR when set
// (Claude Code relocates settings.json and .credentials.json there), else ~/.claude.
func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	return defaultClaudeConfigDir()
}

// customClaudeConfigDir reports whether CLAUDE_CONFIG_DIR points away from ~/.claude.
func customClaudeConfigDir() bool {
	d := os.Getenv("CLAUDE_CONFIG_DIR")
	if d == "" {
		return false
	}
	def := defaultClaudeConfigDir()
	return def == "" || filepath.Clean(d) != filepath.Clean(def)
}

// claudeSettingsPath is the user-level Claude Code settings file.
func claudeSettingsPath() string {
	dir := claudeConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "settings.json")
}

// credentialsFilePath returns Claude Code's credentials file.
// LITELLM_PLUGIN_CLAUDE_CREDENTIALS_FILE overrides the default location for
// non-standard installs and tests. Returns "" when no path can be determined.
func credentialsFilePath() string {
	if p := os.Getenv("LITELLM_PLUGIN_CLAUDE_CREDENTIALS_FILE"); p != "" {
		return p
	}
	dir := claudeConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, ".credentials.json")
}

// keychainSkipReason explains why the macOS Keychain is not consulted, or ""
// when it is (or the platform has no Keychain).
func keychainSkipReason() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	if os.Getenv("LITELLM_PLUGIN_CLAUDE_CREDENTIALS_FILE") != "" {
		return "macOS Keychain skipped: LITELLM_PLUGIN_CLAUDE_CREDENTIALS_FILE is set"
	}
	if customClaudeConfigDir() {
		// Claude Code keys a different (undocumented) Keychain entry per config
		// dir; the default entry may belong to another account, so never read it.
		return "macOS Keychain skipped: CLAUDE_CONFIG_DIR is set (its Keychain entry name is undocumented); " +
			"set LITELLM_PROXY_API_KEY or LITELLM_PLUGIN_CLAUDE_CREDENTIALS_FILE instead"
	}
	return ""
}

// credSource is one lazily-read location of Claude Code's credential store.
type credSource struct {
	name string
	read func() []byte
}

// credentialSources returns lazy readers for Claude Code's credential store,
// most-specific first. resolveCredential stops at the first usable token, so
// in the common case at most one Keychain read happens per refresh.
//  1. the credentials file (LITELLM_PLUGIN_CLAUDE_CREDENTIALS_FILE override or
//     ~/.claude/.credentials.json) — when the override is set it is the only
//     source, keeping tests and non-standard installs deterministic
//  2. on macOS, the login Keychain item Claude Code maintains (service
//     "Claude Code-credentials"): the account-anchored item first, then the
//     accountless lookup. Claude Code stores credentials only in the Keychain
//     on macOS, so gateway-SSO users have no credentials file.
//
// With CLAUDE_CONFIG_DIR pointing elsewhere, only <dir>/.credentials.json is read.
func credentialSources() []credSource {
	path := credentialsFilePath()
	readFile := func() []byte {
		if path == "" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		return data
	}
	sources := []credSource{{name: "credentials file " + path, read: readFile}}
	if runtime.GOOS == "darwin" && keychainSkipReason() == "" {
		sources = append(sources,
			credSource{name: "macOS Keychain (" + keychainService + ")", read: func() []byte { return keychainReadWithAccount() }},
			credSource{name: "macOS Keychain (" + keychainService + ")", read: func() []byte { return keychainRead() }},
		)
	}
	return sources
}

// keychainService is the generic-password service name Claude Code uses.
const keychainService = "Claude Code-credentials"

// keychainRead reads the accountless Keychain lookup — the same query Claude
// Code's own reader falls back to. Swapped out in tests. Returns nil when
// nothing usable is found.
var keychainRead = func() []byte {
	return runSecurity("find-generic-password", "-s", keychainService, "-w")
}

// keychainReadWithAccount queries the Keychain item anchored to the current
// user's account attribute — Claude Code writes one credential item per
// account, and the accountless lookup can return a stale item from an older
// login. Swapped out in tests.
var keychainReadWithAccount = func() []byte {
	name := keychainAccountName()
	if name == "" {
		return nil
	}
	return runSecurity("find-generic-password", "-a", name, "-s", keychainService, "-w")
}

// keychainAccountName resolves the account attribute Claude Code uses for its
// Keychain item (observed: the local username).
func keychainAccountName() string {
	if name := os.Getenv("USER"); name != "" {
		return name
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

// securityBin is invoked by absolute path so a binary named "security" earlier
// on PATH can't impersonate it and feed us a token.
const securityBin = "/usr/bin/security"

// runSecurity shells out to /usr/bin/security, the only supported way to read
// the login Keychain from a non-entitled process. The read is fail-fast (1s
// budget — a locked Keychain would otherwise block on an unlock prompt) and
// the token is never logged.
func runSecurity(args ...string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, securityBin, args...).Output()
	if err != nil {
		return nil
	}
	data := []byte(strings.TrimSpace(string(out)))
	if json.Valid(data) {
		return data
	}
	return nil
}

// isShowCostEnabled returns true only when LITELLM_PLUGIN_SHOW_COST is explicitly enabled.
// Default is false — percent-only display, no dollar amounts.
func isShowCostEnabled() bool {
	val := os.Getenv("LITELLM_PLUGIN_SHOW_COST")
	return val == "1" || val == "true"
}

// isShowEffortEnabled returns true only when LITELLM_PLUGIN_SHOW_EFFORT is explicitly enabled.
// Default is true — current effort is not displayed.
func isShowEffortEnabled() bool {
	val := os.Getenv("LITELLM_PLUGIN_SHOW_EFFORT")
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

// budgetInfo fetches budget info for a credential, using a 30-second
// filesystem cache to avoid hitting the API on every statusline refresh. Each
// invocation of this binary is a fresh process, so all state must live on
// disk. Virtual keys go through /key/info; gateway SSO tokens go through
// /user/info — they authenticate but are not DB key rows, so /key/info fails
// on them with a "where.token" lookup error. Either way a /team/info call then
// populates the team budget fields — the only budget the statusline displays.
func budgetInfo(c credential) (*KeyInfo, error) {
	if c.baseURL == "" {
		return nil, ErrNoBaseURL
	}
	if info, ok := readBudgetCache(c); ok {
		return info, nil
	}
	// Recent failure → back off and replay the cached error instead of re-blocking
	// on the network every refresh while the proxy is down / key is bad / over budget.
	// Only transport failures degrade to stale data; auth/budget errors are
	// authoritative states that must surface as-is. Applies on the replay
	// path too, so the whole backoff window stays informative.
	if failed, ok := readBudgetFailCache(c); ok {
		if failed.Kind == "transport" {
			if stale, ok := readBudgetCacheStale(c); ok {
				stale.stale = true
				return stale, nil
			}
		}
		return nil, errorFromFailEntry(failed)
	}
	// Another process may already be fetching this credential. While its
	// marker is fresh (or we lose the exclusive claim), serve the stale cache
	// instead of racing it — this is what turns Claude Code's
	// cancel-and-respawn of a slow refresh from a per-refresh retry storm into
	// a no-op. With no stale data to serve we fetch anyway: a cold start with
	// a concurrently-fetching sibling just races, as before, which beats
	// rendering nothing.
	if inflightActive(c) {
		if stale, ok := readBudgetCacheStale(c); ok && !authoritativeFailRecord(c) {
			stale.stale = true
			return stale, nil
		}
		// fall through: nothing cached to show, so fetch and race the sibling
	} else if nonce, claimed := claimInflight(c); claimed {
		defer clearInflightMarker(c, nonce)
	} else if inflightActive(c) {
		// Lost the exclusive claim to a sibling whose marker landed after our
		// first check — same deferral as above.
		if stale, ok := readBudgetCacheStale(c); ok && !authoritativeFailRecord(c) {
			stale.stale = true
			return stale, nil
		}
	}
	info, err := fetchBudget(c)
	if err != nil {
		writeBudgetFailCache(c, err)
		if isTransportErr(err) {
			// A preserved authoritative record (revoked key, exceeded budget)
			// outranks the blip: surface the real state instead of stale data.
			if failed, ok := readBudgetFailCache(c); ok && (failed.Kind == "auth" || failed.Kind == "budget") {
				return nil, errorFromFailEntry(failed)
			}
			if stale, ok := readBudgetCacheStale(c); ok {
				stale.stale = true
				return stale, nil
			}
		}
		return nil, err
	}
	// Full success — reset the failure ladder so the next outage starts at the
	// base backoff instead of inheriting an escalated window.
	clearBudgetFailCache(c)
	writeBudgetCache(c, info)
	return info, nil
}

// fetchBudget performs the uncached budget fetch (shared by budgetInfo and
// `doctor`), bounded as a whole by BudgetDeadline.
func fetchBudget(c credential) (*KeyInfo, error) {
	if c.baseURL == "" {
		return nil, ErrNoBaseURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), BudgetDeadline)
	defer cancel()
	if !c.gateway {
		return fetchKeyInfoWithTeam(ctx, c)
	}
	info, err := fetchUserInfo(ctx, c)
	if err != nil {
		return nil, err
	}
	if err := enrichTeamBudget(ctx, c, info); err != nil {
		return nil, err
	}
	// Gateway tokens enforce the JWT-scoped team's budget; when that
	// team is unbudgeted, the user-level budget is what gates requests.
	if (info.TeamMaxBudget == nil || *info.TeamMaxBudget <= 0) && info.MaxBudget != nil {
		info.TeamSpend = info.Spend
		info.TeamMaxBudget = info.MaxBudget
		info.TeamBudgetDuration = info.BudgetDuration
		info.TeamBudgetResetAt = info.BudgetResetAt
	}
	return info, nil
}

// fetchKeyInfoWithTeam fetches /key/info and enriches it with the team budget
// (the only budget the statusline displays — key-level spend is never shown).
func fetchKeyInfoWithTeam(ctx context.Context, c credential) (*KeyInfo, error) {
	info, err := fetchKeyInfo(ctx, c)
	if err != nil {
		return nil, err
	}
	if err := enrichTeamBudget(ctx, c, info); err != nil {
		return nil, err
	}
	return info, nil
}

// enrichTeamBudget overlays the team budget onto info via /team/info — the
// only budget the statusline displays; key/user-level spend is never shown.
// Primary source is this member's own litellm_budget_table row; fallbacks are
// team_member_budget_table, then the team's own max_budget. Spend is always
// paired with the budget's own source — never the key's spend.
//
// A key that may not read its team (401/403/404) simply has no team budget.
// Any other failure (timeout, 5xx) is returned so it surfaces as a connection
// error and is negative-cached, instead of being cached as "no budget".
func enrichTeamBudget(ctx context.Context, c credential, info *KeyInfo) error {
	if info.TeamID == nil || *info.TeamID == "" {
		return nil
	}
	teamResp, err := fetchTeamInfo(ctx, c, *info.TeamID)
	if err != nil {
		var hErr *httpStatusError
		if errors.Is(err, ErrAuth) || (errors.As(err, &hErr) && hErr.Status == http.StatusNotFound) {
			return nil
		}
		return err
	}
	ti := teamResp.TeamInfo
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
	return nil
}

// getJSON GETs baseURL+path with the credential and decodes a 200 response into
// out. Shared error mapping: 401/403 → ErrAuth (body omitted — LiteLLM's token
// lookup errors can echo token-derived details), LiteLLM budget_exceeded →
// *BudgetExceededError, other non-200 → *httpStatusError with a truncated body.
func getJSON(ctx context.Context, c credential, path string, out any) error {
	if c.baseURL == "" {
		return ErrNoBaseURL
	}
	endpoint := c.baseURL + path

	client := &http.Client{Timeout: HTTPTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("request creation failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "claude-code-litellm-plugin/"+Version)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connection error: %w [url=%s]", err, endpoint)
	}
	defer func() { _ = resp.Body.Close() }()

	// Decide auth from the status alone, before touching the body: auth
	// errors omit the body entirely (it can echo token-derived details), and
	// a broken transfer on a 401 must not reclassify as transport.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("status=%d url=%s: %w", resp.StatusCode, endpoint, ErrAuth)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// A broken transfer is transport when the payload is the answer
		// (2xx, 5xx); for other statuses the headers already decide.
		if resp.StatusCode >= 500 || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
			return fmt.Errorf("connection error: failed to read response: %w", err)
		}
		return &httpStatusError{Status: resp.StatusCode, URL: endpoint, Body: "(unreadable)"}
	}
	if resp.StatusCode != http.StatusOK {
		var litellmErr liteLLMError
		if json.Unmarshal(body, &litellmErr) == nil && litellmErr.Error.Type == "budget_exceeded" {
			bErr := &BudgetExceededError{}
			_, _ = fmt.Sscanf(litellmErr.Error.Message, "Budget has been exceeded! Current cost: %f, Max budget: %f", &bErr.Spend, &bErr.MaxBudget)
			return bErr
		}
		// 5xx from the proxy or a load balancer is a proxy-side outage, not an
		// authoritative answer — classify as transport so the stale fallback
		// engages instead of rendering/caching an error-shaped truth.
		if resp.StatusCode >= 500 {
			return fmt.Errorf("connection error: proxy HTTP %d [url=%s]", resp.StatusCode, endpoint)
		}
		return &httpStatusError{Status: resp.StatusCode, URL: endpoint, Body: truncateBody(body)}
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("JSON parse error: %w [body=%s]", err, truncateBody(body))
	}
	return nil
}

// UserInfoResponse is the /user/info response LiteLLM serves to the
// authenticated key's user — the budget source for gateway SSO tokens.
type UserInfoResponse struct {
	UserID   string        `json:"user_id"`
	UserInfo UserInfoData  `json:"user_info"`
	Teams    []GatewayTeam `json:"teams"`
}

// UserInfoData carries the user-level budget from /user/info, plus the user
// record's own team list — the source of the gateway token's scoped team.
type UserInfoData struct {
	Teams          []string `json:"teams"`
	Spend          *float64 `json:"spend"`
	MaxBudget      *float64 `json:"max_budget"`
	BudgetDuration *string  `json:"budget_duration"`
	BudgetResetAt  *string  `json:"budget_reset_at"`
}

// GatewayTeam is one entry of /user/info's top-level teams array. That array is
// NOT the user record's team order — for proxy admins it lists every team on
// the proxy — so it is only used to look up the scoped team's fields.
type GatewayTeam struct {
	TeamID         *string  `json:"team_id"`
	TeamAlias      *string  `json:"team_alias"`
	Spend          *float64 `json:"spend"`
	MaxBudget      *float64 `json:"max_budget"`
	BudgetDuration *string  `json:"budget_duration"`
	BudgetResetAt  *string  `json:"budget_reset_at"`
}

// fetchUserInfo fetches budget info via /user/info — the entry point that
// works for LiteLLM gateway SSO tokens. /key/info looks the token up as a DB
// key row and errors on gateway tokens, while /user/info resolves the
// authenticated user. Team budget detail arrives via the shared /team/info
// enrichment; when the scoped team is unbudgeted, the caller falls back to the
// user-level budget.
//
// The team id is an approximation: LiteLLM encrypts the gateway token, so its
// team claim can't be read client-side. LiteLLM scopes the token to "the first
// team on the developer's user record" (user_info.teams[0]); exact for
// single-team users, best-effort for multi-team users, since the proxy picks
// from a DB query without an explicit order.
func fetchUserInfo(ctx context.Context, c credential) (*KeyInfo, error) {
	var response UserInfoResponse
	if err := getJSON(ctx, c, "/user/info", &response); err != nil {
		return nil, err
	}

	info := &KeyInfo{
		Spend:          response.UserInfo.Spend,
		MaxBudget:      response.UserInfo.MaxBudget,
		BudgetDuration: response.UserInfo.BudgetDuration,
		BudgetResetAt:  response.UserInfo.BudgetResetAt,
	}
	if response.UserID != "" {
		id := response.UserID
		info.UserID = &id
	}
	if len(response.UserInfo.Teams) == 0 {
		return info, nil
	}
	teamID := response.UserInfo.Teams[0]
	info.TeamID = &teamID
	for _, t := range response.Teams {
		if t.TeamID != nil && *t.TeamID == teamID {
			info.TeamSpend = t.Spend
			info.TeamMaxBudget = t.MaxBudget
			info.TeamBudgetDuration = t.BudgetDuration
			info.TeamBudgetResetAt = t.BudgetResetAt
			break
		}
	}
	return info, nil
}

// fetchKeyInfo fetches /key/info for a virtual key.
func fetchKeyInfo(ctx context.Context, c credential) (*KeyInfo, error) {
	var response KeyInfoResponse
	if err := getJSON(ctx, c, "/key/info", &response); err != nil {
		return nil, err
	}
	return &response.Info, nil
}

// fetchTeamInfo calls /team/info to get team-level budget data.
func fetchTeamInfo(ctx context.Context, c credential, teamID string) (*TeamInfoAPIResponse, error) {
	var response TeamInfoAPIResponse
	if err := getJSON(ctx, c, "/team/info?team_id="+url.QueryEscape(teamID), &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// maxErrorBody caps how much of a response body is echoed into error text,
// which is persisted to the budget-fail cache.
const maxErrorBody = 200

// truncateBody trims a response body for inclusion in error messages. Auth
// failures omit the body entirely — LiteLLM's token lookup errors can echo
// token-derived details.
func truncateBody(body []byte) string {
	if len(body) <= maxErrorBody {
		return string(body)
	}
	return string(body[:maxErrorBody]) + "…(truncated)"
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
		if info.stale {
			// Last-known data said "no budget"; label it so a slow proxy
			// doesn't look like a fresh, authoritative answer.
			return formatError("No budget configured (cached)", input)
		}
		return formatError("No budget configured", input)
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

	effortStr := ""
	if isShowEffortEnabled() && input.Effort.Level != "" {
		effortStr = fmt.Sprintf(" | effort: %s", input.Effort.Level)
	}

	// Stale-cache fallback flag: data shown after the proxy became unreachable.
	cachedStr := ""
	if info.stale {
		cachedStr = fmt.Sprintf(" %s(cached)%s", ColorGray, ColorReset)
	}

	line := fmt.Sprintf("%s%s%s%s %s%s%s",
		prefix, absColor, circleGlyph(percent), ColorReset, absColor, budgetStr, ColorReset)

	line += resetStr + updateStr + contextStr + effortStr + cachedStr
	return line
}

// formatError formats an error message with red color
func formatError(msg string, input StatusInput) string {
	return fmt.Sprintf("%s%s%s%s", ColorRed, getPrefix(input), msg, ColorReset)
}

// StatusJSON is the structured output emitted with --json, consumed by the VS Code
// extension (and any other caller that prefers data over ANSI text). All fields are
// optional — absent values are omitted so a minimal payload stays minimal.
//
// Text is the fully-rendered status line (ANSI stripped) — editors forward it
// directly instead of re-implementing the format. Percent is kept alongside so
// editors can apply their own background-color theming (which can't be forwarded).
type StatusJSON struct {
	Prefix          string  `json:"prefix,omitempty"`
	Text            string  `json:"text"`
	Percent         float64 `json:"percent"`
	Spend           float64 `json:"spend,omitempty"`
	MaxBudget       float64 `json:"max_budget,omitempty"`
	HasBudget       bool    `json:"has_budget"`
	ResetLabel      string  `json:"reset_label,omitempty"`
	ResetTime       string  `json:"reset_time,omitempty"`
	UpdateAvailable string  `json:"update_available,omitempty"`
	// Version is the binary's own version. Editor plugins show it and treat its
	// absence as "binary older than the plugin".
	Version        string  `json:"version"`
	ContextPercent float64 `json:"context_percent,omitempty"`
	HasContext     bool    `json:"has_context"`
	// Stale is true when the budget was not freshly fetched in this call —
	// the stale-cache fallback after a transport failure, or a deferral while
	// another process holds the in-flight fetch. Data may be up to
	// StaleCacheMaxAgeMs old.
	Stale bool   `json:"stale,omitempty"`
	Error string `json:"error,omitempty"`
}

// stripANSI removes all ANSI escape sequences from s, leaving plain text.
func stripANSI(s string) string {
	for _, c := range []string{ColorRed, ColorYellow, ColorGreen, ColorGray, ColorReset} {
		s = strings.ReplaceAll(s, c, "")
	}
	return s
}

// renderLine produces the fully-rendered status line (with ANSI color) for the
// current state. Both the stdout path (main) and the --json Text field use this,
// so the two output modes can never drift. buildStatusJSON strips the ANSI for JSON.
func renderLine(info *KeyInfo, latestVersion string, input StatusInput, err error) string {
	if err != nil {
		var bErr *BudgetExceededError
		if errors.As(err, &bErr) && bErr.MaxBudget > 0 {
			pct := (bErr.Spend / bErr.MaxBudget) * 100
			return fmt.Sprintf("%s%s$%.2f/$%.2f (%.0f%%) | Budget exceeded%s",
				ColorRed, getPrefix(input), bErr.Spend, bErr.MaxBudget, pct, ColorReset)
		}
		text, _ := classifyError(err)
		return formatError(text, input)
	}
	if info == nil {
		return formatError("Error", input)
	}
	return formatStatusLine(info, latestVersion, input)
}

// classifyError maps a fetch/resolution error to the short status-line text and
// the stable lowercase code used in the --json "error" field. Both output modes
// use it, so they can't drift.
func classifyError(err error) (text, code string) {
	switch {
	case errors.Is(err, ErrBudgetExceeded):
		return "Budget exceeded", "budget exceeded"
	case errors.Is(err, ErrAuth):
		return "Auth error", "auth error"
	case errors.Is(err, ErrNoAPIKey):
		return "No API key", "no api key"
	case errors.Is(err, ErrNoBaseURL):
		return "No proxy URL", "no proxy url"
	case isTransportErr(err):
		return "Connection error", "connection error"
	default:
		return "Error", "error"
	}
}

// buildStatusJSON gathers the same data as the ANSI path but returns a structured
// StatusJSON. info may be nil (fetch failed); err carries the reason. The caller
// decides whether to render the ANSI line or emit this struct.
func buildStatusJSON(info *KeyInfo, latestVersion string, input StatusInput, err error) StatusJSON {
	out := StatusJSON{Prefix: strings.TrimSpace(getPrefix(input)), Version: Version}
	out.Text = stripANSI(renderLine(info, latestVersion, input, err))

	if input.ContextWindow != nil && input.ContextWindow.UsedPercentage != nil {
		pct := *input.ContextWindow.UsedPercentage
		if pct < 0 {
			pct = 0
		} else if pct > 100 {
			pct = 100
		}
		out.HasContext = true
		out.ContextPercent = pct
	}

	if isUpdateAvailable(Version, latestVersion) {
		out.UpdateAvailable = latestVersion
	}

	if err != nil {
		var bErr *BudgetExceededError
		if errors.As(err, &bErr) && bErr.MaxBudget > 0 {
			out.HasBudget = true
			out.Spend = bErr.Spend
			out.MaxBudget = bErr.MaxBudget
			out.Percent = (bErr.Spend / bErr.MaxBudget) * 100
		}
		_, out.Error = classifyError(err)
		return out
	}

	if info == nil {
		out.Error = "error"
		return out
	}

	info = resolveEffectiveBudget(info)
	if info.MaxBudget == nil || *info.MaxBudget <= 0 {
		out.Error = "no budget configured"
		out.Stale = info.stale
		return out
	}

	out.HasBudget = true
	out.MaxBudget = *info.MaxBudget
	if info.Spend != nil {
		out.Spend = *info.Spend
	}
	out.Percent = (out.Spend / out.MaxBudget) * 100
	out.Stale = info.stale

	resetTime, durationLabel := formatTimeUntilReset(info.BudgetResetAt, info.BudgetDuration)
	out.ResetTime = resetTime
	out.ResetLabel = durationLabel

	return out
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

const usage = `claude-code-litellm-plugin: LiteLLM budget for the Claude Code status line.

Usage:
  claude-code-litellm-plugin            render the status line (Claude Code runs this; reads its JSON on stdin)
  claude-code-litellm-plugin --json     emit status as JSON (editor plugins); loads the env block of Claude's settings.json
  claude-code-litellm-plugin setup      add/refresh the statusLine entry in Claude Code's settings.json (see: setup --help)
  claude-code-litellm-plugin doctor     diagnose configuration, credentials and connectivity
  claude-code-litellm-plugin --version  print the version
`

// run is main without the process exit, so tests can drive every mode.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	mode := ""
	if len(args) > 0 {
		mode = args[0]
	}
	switch mode {
	case "--version", "-v", "version":
		_, _ = fmt.Fprintln(stdout, Version)
		return 0
	case "--help", "-h", "help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	case "setup":
		return runSetup(args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "", "--json":
	default:
		_, _ = fmt.Fprintf(stderr, "unknown argument %q\n\n%s", mode, usage)
		return 2
	}
	jsonMode := mode == "--json"

	var input StatusInput
	if isTerminal(stdin) {
		// A human ran us by hand: don't block waiting for Claude Code's JSON.
		_, _ = fmt.Fprintln(stderr, "(no status JSON on stdin: run `claude-code-litellm-plugin doctor` to diagnose, or --help)")
	} else {
		input = readStatusInput(stdin)
	}

	if jsonMode {
		// Editors don't inherit the env Claude Code applies from settings.json, so
		// apply it here — one implementation for every plugin. Never in plain mode:
		// Claude Code already applied it there, including project-level overrides.
		applyClaudeSettingsEnv()
	}

	cred := resolveCredential()
	if cred.token == "" {
		noKey := fmt.Errorf("%w", ErrNoAPIKey)
		if jsonMode {
			emitJSON(stdout, buildStatusJSON(nil, "", input, noKey))
		} else {
			_, _ = fmt.Fprintln(stdout, renderLine(nil, "", input, noKey))
		}
		return 0
	}

	// The update check runs alongside the budget fetch so it adds no latency.
	latestCh := make(chan string, 1)
	go func() { latestCh <- getLatestVersion() }()
	info, err := budgetInfo(cred)
	latestVersion := <-latestCh

	if jsonMode {
		emitJSON(stdout, buildStatusJSON(info, latestVersion, input, err))
	} else {
		_, _ = fmt.Fprintln(stdout, renderLine(info, latestVersion, input, err))
	}
	return 0
}

// isTerminal reports whether r is an interactive terminal (character device).
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// utf8BOM is stripped from settings files: Windows PowerShell 5.1 writes one.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// readClaudeSettingsEnv returns the string values of the "env" block of
// Claude Code's user settings.json. Missing/invalid files yield nil.
func readClaudeSettingsEnv() map[string]string {
	path := claudeSettingsPath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var settings struct {
		Env map[string]any `json:"env"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(data, utf8BOM), &settings); err != nil {
		return nil
	}
	env := make(map[string]string, len(settings.Env))
	for k, v := range settings.Env {
		if s, ok := v.(string); ok && k != "" {
			env[k] = s
		}
	}
	return env
}

// applyClaudeSettingsEnv sets the settings.json env block into this process's
// environment, overriding inherited values exactly as Claude Code does for its
// subprocesses. Returns the keys applied, sorted.
func applyClaudeSettingsEnv() []string {
	env := readClaudeSettingsEnv()
	keys := make([]string, 0, len(env))
	for k, v := range env {
		if os.Setenv(k, v) == nil {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// emitJSON marshals out to stdout. A failure to marshal would indicate a programming
// error (nil pointers on the struct fields can't happen), so it panics — the binary
// should never produce unparseable JSON in --json mode.
func emitJSON(w io.Writer, out StatusJSON) {
	data, err := json.Marshal(out)
	if err != nil {
		panic(err)
	}
	_, _ = fmt.Fprintln(w, string(data))
}
