# Claude Code LiteLLM Plugin

A statusline plugin for Claude Code that displays LiteLLM budget information including current spend, max budget, usage percentage, and time until reset.

## Installation

### Quick Install (Recommended)

**macOS / Linux:**

```bash
curl -fsSL https://raw.githubusercontent.com/stvnksslr/claude-code-litellm-plugin/main/install.sh | bash
```

**Windows (PowerShell):**

```powershell
irm https://raw.githubusercontent.com/stvnksslr/claude-code-litellm-plugin/main/install.ps1 | iex
```

The installer will:
- Download the latest release for your OS and architecture
- Install the binary to your PATH
- Configure Claude Code to use the plugin

### Manual Installation

1. Download the latest release from the [releases page](https://github.com/stvnksslr/claude-code-litellm-plugin/releases)

2. Extract and move the binary to a location in your PATH:

```bash
# Example: move to ~/.local/bin
mv claude-code-litellm-plugin ~/.local/bin/
chmod +x ~/.local/bin/claude-code-litellm-plugin
```

3. Add the statusline configuration to `~/.claude/settings.json`:

```json
{
  "statusLine": {
    "type": "command",
    "command": "claude-code-litellm-plugin"
  }
}
```

## Configuration

### Environment Variables

Set the following environment variables (typically in your shell profile like `~/.zshrc` or `~/.bashrc`):

```bash
# LiteLLM Proxy URL (required)
export ANTHROPIC_BASE_URL="https://your-litellm-instance.com"
# or
export LITELLM_PROXY_URL="https://your-litellm-instance.com"

# LiteLLM API Key (required)
export ANTHROPIC_AUTH_TOKEN="your-api-key"
# or
export LITELLM_PROXY_API_KEY="your-api-key"
```

### Claude Code Settings

Add the statusline configuration to your Claude Code settings file:

**For global settings** (`~/.claude/settings.json`):

```json
{
  "statusLine": {
    "type": "command",
    "command": "claude-code-litellm-plugin"
  }
}
```

**For project-specific settings** (`.claude/settings.local.json` in your project):

```json
{
  "statusLine": {
    "type": "command",
    "command": "claude-code-litellm-plugin"
  }
}
```

## Output

The plugin displays the active model, budget usage, and context-window pressure:

```
Opus 4.7: ○ 12% weekly reset: 3d1h | 📖 ◑ 45%
```

- **Prefix** is the model display name from Claude Code's stdin (falls back to `LiteLLM:` when stdin is unavailable). Override with `LITELLM_PLUGIN_PREFIX`.
- **Circle gauge** fills clockwise as usage grows: `○` (empty) · `◔` (<30%) · `◑` (<60%) · `◕` (<85%) · `●` (full).
- **Color** thresholds for the budget circle: green `< 75%`, yellow `75–89%`, red `90%+`.
- **Reset countdown** shows time until the budget rolls over.
- **Context segment (`📖 ●`)** reports the current context-window usage from Claude Code. Color thresholds: green `< 70%`, yellow `70–84%`, red `85%+`. Warn and critical bands append `— consider /compact` and `— run /compact or /clear` respectively. The segment is hidden when stdin doesn't include context data (e.g. before the first API call in a session).

![Status line examples](examples.svg)

### Restoring dollar amounts

Dollar amounts are hidden by default. To show `$spend/$budget (pct%)` instead of just the percentage:

```bash
export LITELLM_PLUGIN_SHOW_COST=1
```

## Environment Variable Priority

The plugin checks environment variables in the following order:

**Base URL:**

1. `LITELLM_PROXY_URL`
2. `ANTHROPIC_BASE_URL`

**API Key:**

1. `LITELLM_PROXY_API_KEY`
2. `ANTHROPIC_AUTH_TOKEN`

## Troubleshooting

If the statusline shows an error:

- `No API key` - Set either `ANTHROPIC_AUTH_TOKEN` or `LITELLM_PROXY_API_KEY`
- `Auth error` - Check your API key is valid
- `Connection error` - Check your base URL and network connection
- `Error` - Generic error, check logs for details

## Development

Run tests:

```bash
go test -v
```

Run locally:

```bash
go run main.go
```
