import * as vscode from "vscode";
import { spawn } from "child_process";
import * as path from "path";
import * as os from "os";
import * as fs from "fs";

/** Fields emitted by `claude-code-litellm-plugin --json`. All optional. */
interface StatusJSON {
  prefix?: string;
  text?: string;
  percent?: number;
  spend?: number;
  max_budget?: number;
  has_budget?: boolean;
  reset_label?: string;
  reset_time?: string;
  update_available?: string;
  context_percent?: number;
  has_context?: boolean;
  error?: string;
  /** Binary version; absent on binaries older than this extension. */
  version?: string;
}

const BINARY = "claude-code-litellm-plugin";
/** Same as the IntelliJ plugin. The binary bounds its own network work to ~4s. */
const TIMEOUT_MS = 10_000;
const INSTALL_HINT =
  "Install it with the one-line installer (see the claude-code-litellm-plugin README), " +
  "or set claudeCodeLitellm.binaryPath.";

/** Expand a leading `~` the way users expect in settings. */
function expandHome(p: string): string {
  if (p === "~") return os.homedir();
  if (p.startsWith("~/") || p.startsWith("~\\")) return path.join(os.homedir(), p.slice(2));
  return p;
}

/**
 * Resolve the binary: explicit setting → known install locations (installer,
 * Homebrew, Scoop) → bare name for PATH lookup. The IntelliJ plugin uses the
 * same list. Settings-env merging lives in the binary (`--json` applies the
 * `env` block of Claude Code's settings.json), so nothing else is needed here.
 */
function resolveBinary(): string {
  const explicit = vscode.workspace.getConfiguration("claudeCodeLitellm").get<string>("binaryPath");
  if (explicit) return expandHome(explicit);

  const home = os.homedir();
  const candidates: string[] = [];
  if (process.platform === "win32") {
    const localAppData = process.env.LOCALAPPDATA ?? path.join(home, "AppData", "Local");
    candidates.push(path.join(localAppData, "Programs", BINARY, `${BINARY}.exe`));
    candidates.push(path.join(home, "scoop", "shims", `${BINARY}.exe`));
  } else {
    candidates.push(path.join(home, ".local", "bin", BINARY));
    candidates.push(`/opt/homebrew/bin/${BINARY}`);
    candidates.push(`/usr/local/bin/${BINARY}`);
    candidates.push(`/home/linuxbrew/.linuxbrew/bin/${BINARY}`);
  }
  return candidates.find((c) => fs.existsSync(c)) ?? BINARY;
}

/**
 * Status bar background: VS Code only offers themed error/warning backgrounds,
 * so ≥90% → error, 75–89% → warning, otherwise none. Errors use error.
 */
function backgroundColor(s: StatusJSON): vscode.ThemeColor | undefined {
  const percent = s.percent ?? 0;
  if (!s.has_budget || percent >= 90) return new vscode.ThemeColor("statusBarItem.errorBackground");
  if (percent >= 75) return new vscode.ThemeColor("statusBarItem.warningBackground");
  return undefined;
}

/** Hover tooltip (plain text). Kept in sync with the IntelliJ widget. */
function formatTooltip(s: StatusJSON, binary: string): string {
  const lines: string[] = [];
  if (s.error) lines.push(`Status: ${s.error}`);
  if (s.has_budget && s.spend != null && s.max_budget != null) {
    lines.push(`Spend: $${s.spend.toFixed(2)} / $${s.max_budget.toFixed(2)}`);
    if (!s.error) lines.push(`Usage: ${(s.percent ?? 0).toFixed(1)}%`);
  }
  if (!s.error && s.reset_time) {
    lines.push(`Reset${s.reset_label ? ` (${s.reset_label})` : ""}: ${s.reset_time}`);
  }
  if (s.has_context && s.context_percent != null) {
    lines.push(`Context window: ${s.context_percent.toFixed(0)}%`);
  }
  if (s.update_available) {
    lines.push(`Update available: ${s.update_available} (re-run the installer, or brew/scoop upgrade)`);
  }
  if (s.error) lines.push(`Diagnose: run \`${BINARY} doctor\` in a terminal`);
  lines.push(`Binary: ${binary}${s.version ? ` (${s.version})` : ""}`);
  return lines.join("\n");
}

export function activate(ctx: vscode.ExtensionContext): void {
  const item = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Right, 100);
  item.name = "Claude Code LiteLLM";
  item.command = "claudeCodeLitellm.refresh";
  item.text = "LiteLLM: …";
  item.show();
  ctx.subscriptions.push(item);

  let timer: NodeJS.Timeout | undefined;
  let inFlight = false;

  const showProblem = (text: string, tooltip: string) => {
    item.text = `$(warning) ${text}`;
    item.tooltip = tooltip;
    item.backgroundColor = new vscode.ThemeColor("statusBarItem.errorBackground");
  };

  const refresh = () => {
    if (inFlight) return; // never stack processes when a refresh is slow
    inFlight = true;

    const binary = resolveBinary();
    let spawnError: NodeJS.ErrnoException | undefined;
    let stdout = "";

    const proc = spawn(binary, ["--json"], {
      env: process.env,
      timeout: TIMEOUT_MS,
      stdio: ["pipe", "pipe", "ignore"],
    });
    proc.on("error", (err: NodeJS.ErrnoException) => {
      spawnError = err;
    });
    proc.stdin?.on("error", () => {
      /* EPIPE when the binary is missing or exits early; reported via 'error'/'close'. */
    });
    // The binary reads Claude Code's stdin JSON; editors have none, so send `{}`.
    proc.stdin?.end("{}");
    proc.stdout?.on("data", (d) => (stdout += d.toString()));

    // Settle exactly once. Node emits 'close' after 'error' too (ENOENT), and a
    // killed process may never 'close' if a grandchild still holds stdout.
    let settled = false;
    const settle = (fn: () => void) => {
      if (settled) return;
      settled = true;
      inFlight = false;
      fn();
    };
    proc.on("exit", (_code, signal) => {
      if (signal) {
        settle(() =>
          showProblem("LiteLLM: timeout", `${binary} did not answer within ${TIMEOUT_MS / 1000}s. Run \`${BINARY} doctor\` in a terminal.`)
        );
      }
    });
    proc.on("close", () => settle(() => {
      if (spawnError) {
        showProblem(
          "LiteLLM: binary not found",
          spawnError.code === "ENOENT"
            ? `${BINARY} was not found (${binary}). ${INSTALL_HINT}`
            : `Could not run ${binary}: ${spawnError.message}`
        );
        return;
      }
      let parsed: StatusJSON;
      try {
        parsed = JSON.parse(stdout.trim());
      } catch {
        showProblem("LiteLLM: error", `${binary} returned unexpected output. Run \`${BINARY} doctor\` in a terminal.`);
        return;
      }
      if (!parsed.version) {
        // Binaries before the --json settings-env loading don't report a version.
        showProblem(
          "LiteLLM: update binary",
          `${binary} is older than this extension. Re-run the installer (or brew/scoop upgrade) to update it.`
        );
        return;
      }
      item.text = parsed.text || "LiteLLM: error";
      item.tooltip = formatTooltip(parsed, binary);
      item.backgroundColor = backgroundColor(parsed);
    }));
  };

  ctx.subscriptions.push(vscode.commands.registerCommand("claudeCodeLitellm.refresh", refresh));

  const scheduleRefresh = () => {
    const seconds = Math.max(5, vscode.workspace.getConfiguration("claudeCodeLitellm").get<number>("refreshSeconds") ?? 30);
    if (timer) clearInterval(timer);
    timer = setInterval(refresh, seconds * 1000);
  };
  refresh();
  scheduleRefresh();

  ctx.subscriptions.push(
    vscode.workspace.onDidChangeConfiguration((e) => {
      if (e.affectsConfiguration("claudeCodeLitellm")) {
        refresh();
        scheduleRefresh();
      }
    })
  );
  ctx.subscriptions.push({ dispose: () => timer && clearInterval(timer) });
}

export function deactivate(): void {
  // Cleanup handled by extension subscriptions.
}
