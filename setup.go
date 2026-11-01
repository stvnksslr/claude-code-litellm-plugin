package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// binaryName is the installed name of this program (plus ".exe" on Windows).
const binaryName = "claude-code-litellm-plugin"

// defaultRefreshInterval is the statusLine refreshInterval setup writes: Claude
// Code otherwise only re-runs the status line on events, and the budget cache
// lives for 30s anyway.
const defaultRefreshInterval = 30

// Exit codes for `setup`.
const (
	setupOK      = 0
	setupFailed  = 1
	setupForeign = 2 // a statusLine for another tool exists and was left untouched
)

const setupUsage = `Usage: claude-code-litellm-plugin setup [flags]

Adds (or refreshes) the statusLine entry in Claude Code's settings.json so it
runs this binary. Other settings are preserved; an existing statusLine that
belongs to another tool is left alone unless --force is given.

Flags:
`

type setupOptions struct {
	settings string
	command  string
	vsix     string
	force    bool
	remove   bool
	dryRun   bool
	editors  bool
}

func runSetup(args []string, stdout, stderr io.Writer) int {
	var opts setupOptions
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, setupUsage)
		fs.PrintDefaults()
	}
	fs.StringVar(&opts.settings, "settings", "", "settings file to edit (default: $CLAUDE_CONFIG_DIR/settings.json or ~/.claude/settings.json)")
	fs.StringVar(&opts.command, "command", "", "statusLine command to write (default: path of this binary)")
	fs.BoolVar(&opts.force, "force", false, "replace a statusLine that belongs to another tool (the old file is saved as settings.json.bak)")
	fs.BoolVar(&opts.remove, "remove", false, "remove this plugin's statusLine instead of adding it")
	fs.BoolVar(&opts.dryRun, "dry-run", false, "print the resulting settings.json instead of writing it")
	fs.BoolVar(&opts.editors, "editors", false, "also install the VS Code extension into detected VS Code-family editors")
	fs.StringVar(&opts.vsix, "vsix", "", "with --editors: install this local .vsix instead of downloading the release one")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return setupOK
		}
		return setupFailed
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "setup: unexpected argument %q\n", fs.Arg(0))
		return setupFailed
	}

	settingsPath := opts.settings
	if settingsPath == "" {
		settingsPath = claudeSettingsPath()
	}
	if settingsPath == "" {
		_, _ = fmt.Fprintln(stderr, "setup: cannot locate Claude Code's settings.json (no home directory); pass --settings")
		return setupFailed
	}

	code := configureStatusLine(settingsPath, opts, stdout, stderr)
	if opts.editors && !opts.remove {
		if ec := installEditors(opts.vsix, stdout, stderr); ec != setupOK && code == setupOK {
			code = ec
		}
	}
	return code
}

// configureStatusLine adds/refreshes/removes our statusLine entry in path.
func configureStatusLine(settingsPath string, opts setupOptions, stdout, stderr io.Writer) int {
	target := settingsPath
	if resolved, err := filepath.EvalSymlinks(settingsPath); err == nil {
		target = resolved // keep dotfile-manager symlinks intact: write the real file
	}

	original, err := os.ReadFile(target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		_, _ = fmt.Fprintf(stderr, "setup: cannot read %s: %v\n", settingsPath, err)
		return setupFailed
	}
	exists := err == nil

	settings, err := parseOrderedObject(original)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "setup: %s is not valid JSON (%v).\nFix the file (or move it aside) and run setup again; it was not modified.\n", settingsPath, err)
		return setupFailed
	}

	existing, hasStatusLine := settings.get("statusLine")
	if hasStatusLine && string(existing) == "null" {
		hasStatusLine = false
	}
	var current orderedObject
	ours := false
	if hasStatusLine {
		if obj, err := parseOrderedObject(existing); err == nil {
			current = obj
			if raw, ok := obj.get("command"); ok {
				var cmd string
				if json.Unmarshal(raw, &cmd) == nil {
					ours = isOurCommand(cmd)
				}
			}
		}
	}

	if opts.remove {
		switch {
		case !hasStatusLine:
			_, _ = fmt.Fprintf(stdout, "No statusLine in %s; nothing to remove.\n", settingsPath)
			return setupOK
		case !ours:
			_, _ = fmt.Fprintf(stderr, "setup: the statusLine in %s belongs to another tool (%s); left untouched.\n", settingsPath, compactJSON(existing))
			return setupForeign
		}
		settings.del("statusLine")
	} else {
		if hasStatusLine && !ours && !opts.force {
			_, _ = fmt.Fprintf(stderr, "setup: %s already has a statusLine for another tool:\n  %s\nLeft untouched. Re-run with --force to replace it (the current file is saved as %s.bak).\n",
				settingsPath, compactJSON(existing), filepath.Base(settingsPath))
			return setupForeign
		}
		command := opts.command
		if command == "" {
			var warn string
			command, warn = chooseCommand()
			if warn != "" {
				_, _ = fmt.Fprintln(stderr, "warning: "+warn)
			}
		}
		statusLine := orderedObject{}
		if ours {
			statusLine = current // keep padding, hideVimModeIndicator, custom refreshInterval, unknown fields
		}
		statusLine.set("type", mustJSON("command"))
		statusLine.set("command", mustJSON(command))
		if _, ok := statusLine.get("refreshInterval"); !ok {
			statusLine.set("refreshInterval", mustJSON(defaultRefreshInterval))
		}
		raw, err := statusLine.marshal()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "setup: %v\n", err)
			return setupFailed
		}
		settings.set("statusLine", raw)
	}

	out, err := settings.marshalIndent()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "setup: %v\n", err)
		return setupFailed
	}

	if opts.dryRun {
		_, _ = stdout.Write(out)
		return setupOK
	}
	if exists && bytes.Equal(out, original) {
		_, _ = fmt.Fprintf(stdout, "✓ %s is already up to date.\n", settingsPath)
		return setupOK
	}

	if hasStatusLine && !ours && opts.force {
		if err := os.WriteFile(target+".bak", original, 0o600); err != nil {
			_, _ = fmt.Fprintf(stderr, "setup: cannot write backup %s.bak: %v\n", target, err)
			return setupFailed
		}
		_, _ = fmt.Fprintf(stdout, "Replaced statusLine %s (previous file saved as %s.bak).\n", compactJSON(existing), target)
	}

	perm := os.FileMode(0o600) // new file: the env block may hold API keys
	if fi, err := os.Stat(target); err == nil {
		perm = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		_, _ = fmt.Fprintf(stderr, "setup: %v\n", err)
		return setupFailed
	}
	if err := writeFileAtomic(target, out, perm); err != nil {
		_, _ = fmt.Fprintf(stderr, "setup: cannot write %s: %v\n", settingsPath, err)
		return setupFailed
	}

	if opts.remove {
		_, _ = fmt.Fprintf(stdout, "✓ Removed the statusLine from %s.\n", settingsPath)
	} else {
		_, _ = fmt.Fprintf(stdout, "✓ Configured the Claude Code status line in %s.\n  Claude Code picks this up live (in trusted workspaces). Run `%s doctor` to check the setup.\n", settingsPath, binaryName)
	}
	return setupOK
}

// isOurCommand reports whether a statusLine command runs this plugin (by the
// basename of its first word), regardless of install location.
func isOurCommand(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	var first string
	if cmd != "" && (cmd[0] == '"' || cmd[0] == '\'') {
		if end := strings.IndexByte(cmd[1:], cmd[0]); end >= 0 {
			first = cmd[1 : end+1]
		}
	} else if fields := strings.Fields(cmd); len(fields) > 0 {
		first = fields[0]
	}
	base := path.Base(strings.ReplaceAll(first, `\`, "/"))
	base = strings.TrimSuffix(strings.ToLower(base), ".exe")
	return base == binaryName
}

// chooseCommand picks the statusLine command for this binary. Claude Code runs
// it through a shell (bash/zsh; Git Bash or PowerShell on Windows):
//   - Unix: the PATH entry if it is this binary (keeps a stable Homebrew symlink
//     instead of a versioned Cellar/Caskroom path), else this binary's path;
//   - Windows: the bare name when PATH resolves it (avoids quoting rules that
//     differ between Git Bash and PowerShell), else this binary's path;
//
// Paths are written with forward slashes, relative to ~ when possible.
func chooseCommand() (command, warning string) {
	exe, err := os.Executable()
	if err != nil {
		return binaryName, "cannot determine this binary's path; writing the bare command name (must be on PATH)"
	}
	if lp, err := exec.LookPath(binaryName); err == nil {
		if runtime.GOOS == "windows" {
			return binaryName, ""
		}
		if abs, err := filepath.Abs(lp); err == nil && sameFile(abs, exe) {
			return shellPath(abs), ""
		}
	}
	if versionedPackagePath(exe) {
		warning = fmt.Sprintf("%s is inside a versioned package directory and may disappear on upgrade; put %s on PATH or pass --command", exe, binaryName)
	}
	return shellPath(exe), warning
}

func sameFile(a, b string) bool {
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(fa, fb)
}

// versionedPackagePath flags paths a package manager replaces on upgrade.
func versionedPackagePath(p string) bool {
	p = filepath.ToSlash(p)
	return strings.Contains(p, "/Cellar/") || strings.Contains(p, "/Caskroom/") ||
		(strings.Contains(strings.ToLower(p), "/scoop/apps/") && !strings.Contains(strings.ToLower(p), "/current/"))
}

// shellPath renders p for a shell command line: ~/-relative with forward slashes
// when under the home dir and space-free, else quoted if it contains spaces.
func shellPath(p string) string {
	slashed := filepath.ToSlash(p)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		h := filepath.ToSlash(home)
		if rest, ok := strings.CutPrefix(slashed, h+"/"); ok && !strings.ContainsAny(rest, " \t'\"") {
			return "~/" + rest
		}
	}
	if strings.ContainsAny(slashed, " \t") {
		return `"` + slashed + `"`
	}
	return slashed
}

// ---- Order-preserving JSON object ------------------------------------------

type jsonMember struct {
	Key   string
	Value json.RawMessage
}

// orderedObject is a JSON object that keeps its members' order, so editing one
// key of a user's settings.json doesn't reshuffle the rest.
type orderedObject []jsonMember

// parseOrderedObject parses a JSON object. Empty input (or only whitespace / a
// UTF-8 BOM) is an empty object.
func parseOrderedObject(data []byte) (orderedObject, error) {
	data = bytes.TrimSpace(bytes.TrimPrefix(data, utf8BOM))
	if len(data) == 0 {
		return orderedObject{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("top level is not a JSON object")
	}
	obj := orderedObject{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, errors.New("invalid object key")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		obj.set(key, raw)
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after the top-level object")
	}
	return obj, nil
}

func (o orderedObject) get(key string) (json.RawMessage, bool) {
	for _, m := range o {
		if m.Key == key {
			return m.Value, true
		}
	}
	return nil, false
}

// set replaces key in place (keeping its position) or appends it.
func (o *orderedObject) set(key string, value json.RawMessage) {
	for i := range *o {
		if (*o)[i].Key == key {
			(*o)[i].Value = value
			return
		}
	}
	*o = append(*o, jsonMember{Key: key, Value: value})
}

func (o *orderedObject) del(key string) {
	for i := range *o {
		if (*o)[i].Key == key {
			*o = append((*o)[:i], (*o)[i+1:]...)
			return
		}
	}
}

func (o orderedObject) marshal() (json.RawMessage, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := json.Marshal(m.Key)
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(m.Value)
	}
	buf.WriteByte('}')
	var compact bytes.Buffer
	if err := json.Compact(&compact, buf.Bytes()); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}

// marshalIndent renders the object 2-space indented with a trailing newline,
// no BOM — the format Claude Code itself writes.
func (o orderedObject) marshalIndent() ([]byte, error) {
	raw, err := o.marshal()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}

// ---- Editor extensions -----------------------------------------------------

// releaseDownloadBase is where release assets live (a var so tests can serve them).
var releaseDownloadBase = "https://github.com/" + GitHubRepo + "/releases/download"

// editorCLINames are the VS Code-family command-line launchers we install into.
var editorCLINames = []string{"code", "code-insiders", "cursor", "windsurf", "codium"}

// editorAppCLIs are macOS app-bundle launchers, for editors whose shell command
// was never added to PATH. A var so tests can disable it.
var editorAppCLIs = func() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return []string{
		"/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code",
		"/Applications/Visual Studio Code - Insiders.app/Contents/Resources/app/bin/code-insiders",
		"/Applications/Cursor.app/Contents/Resources/app/bin/cursor",
		"/Applications/Windsurf.app/Contents/Resources/app/bin/windsurf",
		"/Applications/VSCodium.app/Contents/Resources/app/bin/codium",
	}
}

// findEditorCLIs returns the distinct VS Code-family launchers on this machine.
func findEditorCLIs() []string {
	var found []string
	seen := map[string]bool{}
	add := func(p string) {
		key := p
		if r, err := filepath.EvalSymlinks(p); err == nil {
			key = r
		}
		if !seen[key] {
			seen[key] = true
			found = append(found, p)
		}
	}
	for _, name := range editorCLINames {
		if p, err := exec.LookPath(name); err == nil {
			add(p)
		}
	}
	for _, p := range editorAppCLIs() {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			add(p)
		}
	}
	return found
}

func vsixAssetName(version string) string { return "claude-code-litellm-" + version + ".vsix" }
func intellijAssetName(version string) string {
	return "claude-code-litellm-intellij-" + version + ".zip"
}
func checksumsAssetName(version string) string {
	return binaryName + "_" + version + "_checksums.txt"
}

// installEditors installs the VS Code extension matching this binary's version
// into every detected VS Code-family editor, and prints IntelliJ instructions.
func installEditors(localVSIX string, stdout, stderr io.Writer) int {
	defer printIntelliJHint(stdout)

	clis := findEditorCLIs()
	if len(clis) == 0 {
		_, _ = fmt.Fprintf(stdout, "No VS Code-family editor CLI found (looked for %s).\n", strings.Join(editorCLINames, ", "))
		return setupOK
	}

	vsix := localVSIX
	if vsix == "" {
		if Version == "dev" {
			_, _ = fmt.Fprintln(stderr, "setup --editors: this is a dev build with no matching release; build the extension (mise run vscode:build) and pass --vsix <file>")
			return setupFailed
		}
		dir, err := os.MkdirTemp("", "claude-code-litellm-vsix-")
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "setup --editors: %v\n", err)
			return setupFailed
		}
		defer func() { _ = os.RemoveAll(dir) }()
		vsix, err = downloadVerifiedAsset(Version, vsixAssetName(Version), dir)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "setup --editors: %v\n", err)
			return setupFailed
		}
	}

	code := setupOK
	for _, cli := range clis {
		cmd := exec.Command(cli, "--install-extension", vsix, "--force")
		if out, err := cmd.CombinedOutput(); err != nil {
			_, _ = fmt.Fprintf(stderr, "✗ %s --install-extension failed: %v\n%s", cli, err, out)
			code = setupFailed
			continue
		}
		_, _ = fmt.Fprintf(stdout, "✓ Installed the VS Code extension via %s (reload the editor window).\n", cli)
	}
	return code
}

func printIntelliJHint(w io.Writer) {
	v := Version
	if v == "dev" {
		v = "<version>"
	}
	_, _ = fmt.Fprintf(w, "IntelliJ: download %s/%s/%s, then Settings → Plugins → ⚙ → Install Plugin from Disk…\n",
		releaseDownloadBase, v, intellijAssetName(v))
}

// downloadVerifiedAsset downloads a release asset into dir and verifies its
// SHA-256 against the release's checksums file. Returns the file path.
func downloadVerifiedAsset(version, name, dir string) (string, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	base := releaseDownloadBase + "/" + version + "/"

	sums, err := httpGetBytes(client, base+checksumsAssetName(version))
	if err != nil {
		return "", fmt.Errorf("downloading checksums: %w", err)
	}
	want := ""
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			want = strings.ToLower(fields[0])
		}
	}
	if want == "" {
		return "", fmt.Errorf("%s is not listed in %s", name, checksumsAssetName(version))
	}

	data, err := httpGetBytes(client, base+name)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", name, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s", name, got, want)
	}
	dest := filepath.Join(dir, name)
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		return "", err
	}
	return dest, nil
}

func httpGetBytes(client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "claude-code-litellm-plugin/"+Version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
