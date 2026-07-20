import * as vscode from "vscode";
import { spawn } from "child_process";
import * as path from "path";
import * as os from "os";
import * as fs from "fs";

/** Fields emitted by `claude-code-litellm-plugin --json`. All optional. */
interface StatusJSON {
  prefix?: string;
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
}

/**
 * Read the `env` block from Claude Code's settings file (~/.claude/settings.json).
 * This is where users configure ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN etc. for
 * the terminal CLI. The VS Code extension host never sees these — GUI apps inherit
 * env from launchd, not the shell profile — so we read the file directly and pass
 * the values to the spawned binary. Returns {} on missing/unreadable/invalid.
 */
function readClaudeSettingsEnv(): Record<string, string> {
  const settingsPath = path.join(os.homedir(), ".claude", "settings.json");
  try {
    const raw = fs.readFileSync(settingsPath, "utf8");
    const parsed = JSON.parse(raw);
    if (parsed && typeof parsed.env === "object" && parsed.env !== null) {
      return parsed.env as Record<string, string>;
    }
  } catch {
    // Missing/unreadable/invalid — not fatal, extension settings or process env may suffice.
  }
  return {};
}

/**
 * Resolve the binary path: explicit setting → PATH → common install dirs.
 * Returns the bare binary name when no explicit path is set so spawn() can
 * resolve it via PATH; returns an absolute path only when falling back to
 * a known install location.
 */
function resolveBinary(): string {
  const cfg = vscode.workspace.getConfiguration("claudeCodeLitellm");
  const explicit = cfg.get<string>("binaryPath");
  if (explicit) return explicit;

  // Fall back to common install locations so the extension works even when the
  // binary isn't on PATH (e.g. installed via the repo's install.sh to ~/.local/bin).
  const candidates = [
    path.join(os.homedir(), ".local", "bin", "claude-code-litellm-plugin"),
    "/usr/local/bin/claude-code-litellm-plugin",
  ];
  for (const c of candidates) {
    if (fs.existsSync(c)) return c;
  }
  // Last resort: let the shell resolve it via PATH (spawn will fail visibly if absent).
  return "claude-code-litellm-plugin";
}

/**
 * Build the env for the spawned binary. Precedence (highest wins):
 *   1. VS Code settings (explicit overrides)
 *   2. ~/.claude/settings.json `env` block (single source of truth for both surfaces)
 *   3. process.env (the extension host's environment — usually empty for GUI apps)
 */
function buildEnv(): NodeJS.ProcessEnv {
  const cfg = vscode.workspace.getConfiguration("claudeCodeLitellm");
  const env: NodeJS.ProcessEnv = { ...process.env, ...readClaudeSettingsEnv() };

  const proxyUrl = cfg.get<string>("proxyUrl");
  if (proxyUrl) env.LITELLM_PROXY_URL = proxyUrl;

  const apiKey = cfg.get<string>("apiKey");
  if (apiKey) env.LITELLM_PROXY_API_KEY = apiKey;

  if (cfg.get<boolean>("showCost")) env.LITELLM_PLUGIN_SHOW_COST = "1";

  const prefix = cfg.get<string>("prefix");
  if (prefix) env.LITELLM_PLUGIN_PREFIX = prefix;

  return env;
}

/**
 * Map a budget percentage to a VS Code codicon that approximates the Go binary's
 * circle-gauge glyphs. VS Code has no partial-fill circle codicon, so we use
 * three states: empty, half-ish, full.
 */
function budgetIcon(percent: number): string {
  if (percent >= 85) return "$(circle-filled)";
  if (percent >= 30) return "$(circle-large-outline)";
  return "$(circle-outline)";
}

/**
 * Pick the statusbar background color. VS Code only allows two themed backgrounds
 * (error + warning) — we use red for ≥90%, yellow for 75–89%, and default (no
 * background) below that.
 */
function backgroundColor(
  percent: number,
  hasBudget: boolean
): vscode.ThemeColor | undefined {
  if (!hasBudget) return new vscode.ThemeColor("statusBarItem.errorBackground");
  if (percent >= 90) return new vscode.ThemeColor("statusBarItem.errorBackground");
  if (percent >= 75) return new vscode.ThemeColor("statusBarItem.warningBackground");
  return undefined;
}

/** Format the status bar text from the parsed JSON. */
function formatText(s: StatusJSON): string {
  // s.prefix already carries its own trailing colon (e.g. "LiteLLM:"), so never append another.
  const prefix = s.prefix ?? "LiteLLM:";

  if (s.error) {
    const icon = s.error === "budget exceeded" ? "$(circle-filled)" : "$(error)";
    return `${prefix} ${icon} ${s.error}`;
  }

  // Mirror the Go statusline: "<prefix> <glyph> <pct>% <reset> | 📖 <ctx>%".
  let text = `${prefix} ${budgetIcon(s.percent ?? 0)} ${Math.round(s.percent ?? 0)}%`;

  if (s.reset_time) {
    const label = s.reset_label ? `${s.reset_label} ` : "";
    text += ` ${label}reset: ${s.reset_time}`;
  }

  if (s.has_context && s.context_percent != null) {
    text += ` | 📖 ${Math.round(s.context_percent)}%`;
  }

  if (s.update_available) {
    text += ` | update: ${s.update_available}`;
  }

  return text;
}

/** Build the hover tooltip (plain text, no ANSI). */
function formatTooltip(s: StatusJSON): string {
  const lines: string[] = [];

  if (s.error) {
    lines.push(`Status: ${s.error}`);
    if (s.has_budget && s.spend != null && s.max_budget != null) {
      lines.push(`Spend: $${s.spend.toFixed(2)} / $${s.max_budget.toFixed(2)}`);
    }
    return lines.join("\n");
  }

  if (s.has_budget && s.spend != null && s.max_budget != null) {
    lines.push(`Spend: $${s.spend.toFixed(2)} / $${s.max_budget.toFixed(2)}`);
    lines.push(`Usage: ${s.percent?.toFixed(1)}%`);
  }
  if (s.reset_time) {
    const label = s.reset_label ? ` (${s.reset_label})` : "";
    lines.push(`Reset${label}: ${s.reset_time}`);
  }
  if (s.has_context && s.context_percent != null) {
    lines.push(`Context window: ${s.context_percent.toFixed(0)}%`);
  }
  if (s.update_available) {
    lines.push(`Update available: ${s.update_available}`);
  }
  return lines.join("\n");
}

export function activate(ctx: vscode.ExtensionContext): void {
  const item = vscode.window.createStatusBarItem(
    vscode.StatusBarAlignment.Right,
    100
  );
  item.name = "Claude Code LiteLLM";
  item.command = "claudeCodeLitellm.refresh";
  ctx.subscriptions.push(item);

  let timer: NodeJS.Timeout | undefined;

  const refresh = () => {
    const cfg = vscode.workspace.getConfiguration("claudeCodeLitellm");
    const binary = resolveBinary();
    const env = buildEnv();

    const proc = spawn(binary, ["--json"], {
      env,
      timeout: 5000,
      stdio: ["pipe", "pipe", "pipe"],
    });

    // The binary reads Claude Code's stdin JSON; in VS Code there is no such
    // payload, so send a minimal `{}` to avoid a blocking read.
    proc.stdin.end("{}");

    let stdout = "";
    proc.stdout.on("data", (d) => (stdout += d.toString()));

    proc.on("error", () => {
      item.text = "$(error) LiteLLM: binary not found";
      item.tooltip = "Install claude-code-litellm-plugin and set claudeCodeLitellm.binaryPath";
      item.backgroundColor = new vscode.ThemeColor("statusBarItem.errorBackground");
      item.show();
    });

    proc.on("close", () => {
      let parsed: StatusJSON = { error: "error" };
      try {
        parsed = JSON.parse(stdout.trim());
      } catch {
        parsed = { error: "error" };
      }
      item.text = formatText(parsed);
      item.tooltip = formatTooltip(parsed);
      item.backgroundColor = backgroundColor(parsed.percent ?? 0, parsed.has_budget ?? false);
      item.show();
    });
  };

  // Manual refresh command.
  ctx.subscriptions.push(
    vscode.commands.registerCommand("claudeCodeLitellm.refresh", refresh)
  );

  // Initial refresh + interval.
  refresh();
  const scheduleRefresh = () => {
    const cfg = vscode.workspace.getConfiguration("claudeCodeLitellm");
    const seconds = Math.max(5, cfg.get<number>("refreshSeconds") ?? 30);
    if (timer) clearInterval(timer);
    timer = setInterval(refresh, seconds * 1000);
  };
  scheduleRefresh();

  // Re-schedule when settings change.
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
