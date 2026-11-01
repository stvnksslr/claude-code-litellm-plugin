package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testCmd = "~/.local/bin/claude-code-litellm-plugin"

// runSetupOn runs `setup --command testCmd <extra>` against a settings file
// with the given initial content (nil = file absent).
func runSetupOn(t *testing.T, initial []byte, extra ...string) (path string, code int, stdout, stderr string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "settings.json")
	if initial != nil {
		if err := os.WriteFile(path, initial, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	args := append([]string{"--settings", path, "--command", testCmd}, extra...)
	code = runSetup(args, &out, &errOut)
	return path, code, out.String(), errOut.String()
}

func readJSONMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(data, utf8BOM) {
		t.Fatal("settings written with a BOM")
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("settings not valid JSON: %v\n%s", err, data)
	}
	return m
}

func wantOurStatusLine(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	sl, ok := m["statusLine"].(map[string]any)
	if !ok {
		t.Fatalf("statusLine missing: %v", m)
	}
	if sl["type"] != "command" || sl["command"] != testCmd {
		t.Errorf("statusLine = %v", sl)
	}
	return sl
}

func TestSetupMergesSettings(t *testing.T) {
	inputs := map[string][]byte{
		"missing file":      nil,
		"empty file":        []byte(""),
		"empty object":      []byte("{}"),
		"minified":          []byte(`{"model":"opus","env":{"FOO":"bar"}}`),
		"nested last":       []byte("{\n  \"model\": \"opus\",\n  \"env\": {\n    \"FOO\": \"bar\"\n  }\n}\n"),
		"BOM (install.ps1)": append(append([]byte{}, utf8BOM...), []byte(`{"env":{"FOO":"bar"}}`)...),
	}
	for name, in := range inputs {
		t.Run(name, func(t *testing.T) {
			path, code, _, stderr := runSetupOn(t, in)
			if code != setupOK {
				t.Fatalf("code = %d, stderr = %s", code, stderr)
			}
			m := readJSONMap(t, path)
			sl := wantOurStatusLine(t, m)
			if sl["refreshInterval"] != float64(defaultRefreshInterval) {
				t.Errorf("refreshInterval = %v", sl["refreshInterval"])
			}
			if env, ok := m["env"].(map[string]any); ok {
				if _, polluted := env["statusLine"]; polluted || env["FOO"] != "bar" {
					t.Errorf("env block damaged: %v", env)
				}
			}
		})
	}
}

func TestSetupPreservesKeyOrderAndIsIdempotent(t *testing.T) {
	in := []byte(`{"zeta":1,"alpha":{"b":2,"a":1},"model":"opus"}`)
	path, code, _, _ := runSetupOn(t, in)
	if code != setupOK {
		t.Fatal(code)
	}
	first, _ := os.ReadFile(path)
	s := string(first)
	order := []string{`"zeta"`, `"alpha"`, `"b"`, `"a"`, `"model"`, `"statusLine"`}
	inOrder := true
	for i := 1; i < len(order); i++ {
		inOrder = inOrder && strings.Index(s, order[i-1]) < strings.Index(s, order[i])
	}
	if !inOrder {
		t.Errorf("key order not preserved:\n%s", s)
	}
	var out bytes.Buffer
	if code := runSetup([]string{"--settings", path, "--command", testCmd}, &out, &out); code != setupOK {
		t.Fatal(code)
	}
	second, _ := os.ReadFile(path)
	if !bytes.Equal(first, second) || !strings.Contains(out.String(), "already up to date") {
		t.Errorf("second run changed the file or didn't report up-to-date: %q", out.String())
	}
}

func TestSetupUpdatesOurStatusLineKeepingUserFields(t *testing.T) {
	in := []byte(`{"statusLine":{"type":"command","command":"/old/path/claude-code-litellm-plugin","padding":2,"refreshInterval":10,"hideVimModeIndicator":true}}`)
	path, code, _, _ := runSetupOn(t, in)
	if code != setupOK {
		t.Fatal(code)
	}
	sl := wantOurStatusLine(t, readJSONMap(t, path))
	if sl["padding"] != float64(2) || sl["refreshInterval"] != float64(10) || sl["hideVimModeIndicator"] != true {
		t.Errorf("user statusLine fields lost: %v", sl)
	}
}

func TestSetupForeignStatusLine(t *testing.T) {
	in := []byte(`{"statusLine":{"type":"command","command":"~/my-statusline.sh"}}`)

	path, code, _, stderr := runSetupOn(t, in)
	if code != setupForeign || !strings.Contains(stderr, "--force") {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, in) {
		t.Error("foreign statusLine must be left untouched")
	}

	path, code, _, _ = runSetupOn(t, in, "--force")
	if code != setupOK {
		t.Fatal(code)
	}
	wantOurStatusLine(t, readJSONMap(t, path))
	if bak, _ := os.ReadFile(path + ".bak"); !bytes.Equal(bak, in) {
		t.Errorf("backup = %q", bak)
	}

	_, code, _, _ = runSetupOn(t, in, "--remove")
	if code != setupForeign {
		t.Errorf("--remove must not delete a foreign statusLine, code = %d", code)
	}
}

func TestSetupRefusesInvalidJSON(t *testing.T) {
	in := []byte(`{"model": "opus",`)
	path, code, _, stderr := runSetupOn(t, in)
	if code != setupFailed || !strings.Contains(stderr, "not valid JSON") {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, in) {
		t.Error("invalid settings must never be overwritten")
	}
}

func TestSetupRemove(t *testing.T) {
	in := []byte(`{"model":"opus","statusLine":{"type":"command","command":"claude-code-litellm-plugin"}}`)
	path, code, _, _ := runSetupOn(t, in, "--remove")
	if code != setupOK {
		t.Fatal(code)
	}
	m := readJSONMap(t, path)
	if _, ok := m["statusLine"]; ok || m["model"] != "opus" {
		t.Errorf("after --remove: %v", m)
	}
}

func TestSetupDryRunDoesNotWrite(t *testing.T) {
	in := []byte(`{}`)
	path, code, stdout, _ := runSetupOn(t, in, "--dry-run")
	if code != setupOK || !strings.Contains(stdout, `"statusLine"`) {
		t.Fatalf("code = %d, stdout = %q", code, stdout)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, in) {
		t.Error("--dry-run wrote the file")
	}
}

func TestSetupKeepsSymlinkAndMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles-settings.json")
	if err := os.WriteFile(real, []byte(`{}`), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := runSetup([]string{"--settings", link, "--command", testCmd}, &out, &out); code != setupOK {
		t.Fatal(out.String())
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink was replaced by a regular file")
	}
	if fi, _ := os.Stat(real); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", fi.Mode().Perm())
	}
	wantOurStatusLine(t, readJSONMap(t, real))
}

func TestSetupNewFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	path, code, _, _ := runSetupOn(t, nil)
	if code != setupOK {
		t.Fatal(code)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestIsOurCommand(t *testing.T) {
	yes := []string{
		"claude-code-litellm-plugin",
		"~/.local/bin/claude-code-litellm-plugin",
		"/opt/homebrew/bin/claude-code-litellm-plugin --flag",
		`"C:/Program Files/x/claude-code-litellm-plugin.exe"`,
		`C:\Users\me\AppData\Local\Programs\claude-code-litellm-plugin\Claude-Code-LiteLLM-Plugin.EXE`,
	}
	no := []string{"", "~/statusline.sh", "claude-code-litellm-plugin-wrapper", "bash -c claude-code-litellm-plugin"}
	for _, c := range yes {
		if !isOurCommand(c) {
			t.Errorf("isOurCommand(%q) = false", c)
		}
	}
	for _, c := range no {
		if isOurCommand(c) {
			t.Errorf("isOurCommand(%q) = true", c)
		}
	}
}

func TestShellPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got := shellPath(filepath.Join(home, ".local", "bin", "x")); got != "~/.local/bin/x" {
		t.Errorf("shellPath(home) = %q", got)
	}
	if got := shellPath("/opt/my tools/x"); got != `"/opt/my tools/x"` && runtime.GOOS != "windows" {
		t.Errorf("shellPath(space) = %q", got)
	}
	if !versionedPackagePath("/opt/homebrew/Caskroom/claude-code-litellm-plugin/0.5.0/claude-code-litellm-plugin") {
		t.Error("Caskroom path should be flagged as versioned")
	}
}

func TestChooseCommandPrefersPathEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows writes the bare name")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	link := filepath.Join(bin, binaryName)
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	cmd, warn := chooseCommand()
	if cmd != link || warn != "" {
		t.Errorf("chooseCommand() = (%q, %q), want the PATH symlink %q", cmd, warn, link)
	}
}

func TestInstallEditors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI is a shell script")
	}
	origVersion, origBase, origApps := Version, releaseDownloadBase, editorAppCLIs
	t.Cleanup(func() { Version, releaseDownloadBase, editorAppCLIs = origVersion, origBase, origApps })
	Version = "9.9.9"
	editorAppCLIs = func() []string { return nil }

	vsix := []byte("fake-vsix")
	sum := sha256.Sum256(vsix)
	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), vsixAssetName("9.9.9"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/9.9.9/" + checksumsAssetName("9.9.9"):
			_, _ = w.Write([]byte(checksums))
		case "/9.9.9/" + vsixAssetName("9.9.9"):
			_, _ = w.Write(vsix)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	releaseDownloadBase = srv.URL

	bin := t.TempDir()
	record := filepath.Join(bin, "calls.txt")
	script := "#!/bin/sh\n/bin/cp \"$2\" " + filepath.Join(bin, "installed.vsix") + "\necho \"$@\" >> " + record + "\n"
	if err := os.WriteFile(filepath.Join(bin, "cursor"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	var out, errOut bytes.Buffer
	if code := installEditors("", &out, &errOut); code != setupOK {
		t.Fatalf("code = %d, stderr = %s", code, errOut.String())
	}
	calls, _ := os.ReadFile(record)
	if !strings.Contains(string(calls), "--install-extension") || !strings.Contains(string(calls), "--force") {
		t.Errorf("cursor called with %q", calls)
	}
	if got, _ := os.ReadFile(filepath.Join(bin, "installed.vsix")); !bytes.Equal(got, vsix) {
		t.Error("installed vsix content differs from the release asset")
	}
	if !strings.Contains(out.String(), "IntelliJ") {
		t.Error("missing IntelliJ instructions")
	}

	t.Run("checksum mismatch aborts", func(t *testing.T) {
		vsix = []byte("tampered")
		var out, errOut bytes.Buffer
		if code := installEditors("", &out, &errOut); code != setupFailed || !strings.Contains(errOut.String(), "checksum mismatch") {
			t.Errorf("code = %d, stderr = %q", code, errOut.String())
		}
	})
}
