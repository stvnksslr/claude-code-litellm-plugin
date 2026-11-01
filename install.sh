#!/usr/bin/env bash
# Installs claude-code-litellm-plugin and wires it into Claude Code.
#
#   curl -fsSL https://raw.githubusercontent.com/stvnksslr/claude-code-litellm-plugin/main/install.sh | bash
#   curl -fsSL .../install.sh | bash -s -- --editors     # also install the VS Code extension
#
# Options (flags or environment variables):
#   --editors                 LITELLM_PLUGIN_EDITORS=1         install the extension into code/cursor/windsurf/codium
#   --version X.Y.Z           LITELLM_PLUGIN_VERSION=X.Y.Z     install a specific release (default: latest)
#   --install-dir DIR         LITELLM_PLUGIN_INSTALL_DIR=DIR   default: ~/.local/bin
#   --no-modify-path          LITELLM_PLUGIN_NO_MODIFY_PATH=1  don't add the install dir to your shell profile
set -euo pipefail

REPO="stvnksslr/claude-code-litellm-plugin"
BINARY_NAME="claude-code-litellm-plugin"

EDITORS="${LITELLM_PLUGIN_EDITORS:-}"
VERSION="${LITELLM_PLUGIN_VERSION:-}"
INSTALL_DIR="${LITELLM_PLUGIN_INSTALL_DIR:-${HOME}/.local/bin}"
NO_MODIFY_PATH="${LITELLM_PLUGIN_NO_MODIFY_PATH:-}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

info() { printf "${GREEN}[INFO]${NC} %s\n" "$1"; }
warn() { printf "${YELLOW}[WARN]${NC} %s\n" "$1"; }
error() { printf "${RED}[ERROR]${NC} %s\n" "$1" >&2; exit 1; }

while [ $# -gt 0 ]; do
    case "$1" in
        --editors) EDITORS=1 ;;
        --version) VERSION="${2:?--version needs a value}"; shift ;;
        --version=*) VERSION="${1#*=}" ;;
        --install-dir) INSTALL_DIR="${2:?--install-dir needs a value}"; shift ;;
        --install-dir=*) INSTALL_DIR="${1#*=}" ;;
        --no-modify-path) NO_MODIFY_PATH=1 ;;
        -h|--help) sed -n '2,12p' "$0" 2>/dev/null || true; exit 0 ;;
        *) error "Unknown option: $1" ;;
    esac
    shift
done

detect_os() {
    case "$(uname -s)" in
        Linux*)  echo "Linux" ;;
        Darwin*) echo "Darwin" ;;
        MINGW*|MSYS*|CYGWIN*) error "On Windows, use install.ps1: irm https://raw.githubusercontent.com/${REPO}/main/install.ps1 | iex" ;;
        *)       error "Unsupported operating system: $(uname -s)" ;;
    esac
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)  echo "x86_64" ;;
        arm64|aarch64) echo "arm64" ;;
        i386|i686)     echo "i386" ;;
        *)             error "Unsupported architecture: $(uname -m)" ;;
    esac
}

# Resolves "latest" via the release redirect: no GitHub API call, no rate limit.
latest_version() {
    local url
    url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest") \
        || error "Could not reach GitHub to find the latest release"
    case "$url" in
        */releases/tag/*) echo "${url##*/}" ;;
        *) error "Could not determine the latest release (got ${url})" ;;
    esac
}

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

install_binary() {
    local os=$1 arch=$2 version=$3
    local archive="${BINARY_NAME}_${os}_${arch}.tar.gz"
    local checksums="${BINARY_NAME}_${version}_checksums.txt"
    local base="https://github.com/${REPO}/releases/download/${version}"

    TMP_DIR=$(mktemp -d)
    trap 'rm -rf "$TMP_DIR"' EXIT

    info "Downloading ${archive} (${version})..."
    curl -fsSL "${base}/${archive}" -o "${TMP_DIR}/${archive}" \
        || error "Download failed: ${base}/${archive}"
    curl -fsSL "${base}/${checksums}" -o "${TMP_DIR}/${checksums}" \
        || error "Download failed: ${base}/${checksums}"

    local want got
    want=$(awk -v f="$archive" '$2 == f || $2 == "*"f {print $1}' "${TMP_DIR}/${checksums}")
    [ -n "$want" ] || error "${archive} is not listed in ${checksums}"
    got=$(sha256_of "${TMP_DIR}/${archive}")
    [ "$want" = "$got" ] || error "Checksum mismatch for ${archive} (expected ${want}, got ${got})"
    info "Checksum verified"

    tar -xzf "${TMP_DIR}/${archive}" -C "${TMP_DIR}" "${BINARY_NAME}"
    mkdir -p "${INSTALL_DIR}"
    # Install via a temp name + rename so a running statusline never sees a partial file.
    cp "${TMP_DIR}/${BINARY_NAME}" "${INSTALL_DIR}/.${BINARY_NAME}.new"
    chmod +x "${INSTALL_DIR}/.${BINARY_NAME}.new"
    mv -f "${INSTALL_DIR}/.${BINARY_NAME}.new" "${INSTALL_DIR}/${BINARY_NAME}"
    info "Installed ${INSTALL_DIR}/${BINARY_NAME}"
}

# Adds INSTALL_DIR to PATH for future shells (idempotent). Only convenience:
# the Claude Code statusLine uses the binary's full path either way.
ensure_path() {
    case ":${PATH}:" in *":${INSTALL_DIR}:"*) return ;; esac
    [ -n "$NO_MODIFY_PATH" ] && { warn "${INSTALL_DIR} is not on your PATH"; return; }

    local marker="# Added by claude-code-litellm-plugin installer"
    case "${SHELL:-}" in
        */fish)
            local conf="${XDG_CONFIG_HOME:-$HOME/.config}/fish/conf.d/claude-code-litellm-plugin.fish"
            mkdir -p "$(dirname "$conf")"
            printf '%s\nfish_add_path -g "%s"\n' "$marker" "$INSTALL_DIR" > "$conf"
            info "Added ${INSTALL_DIR} to PATH in ${conf}"
            return ;;
        */zsh)  rc="${HOME}/.zshrc" ;;
        */bash) rc="${HOME}/.bashrc" ;;
        *)      rc="${HOME}/.profile" ;;
    esac
    if [ -f "$rc" ] && grep -qF "$marker" "$rc"; then
        return
    fi
    # shellcheck disable=SC2016 # $PATH must stay literal: it expands when the rc file is sourced
    printf '\n%s\nexport PATH="$PATH:%s"\n' "$marker" "$INSTALL_DIR" >> "$rc"
    info "Added ${INSTALL_DIR} to PATH in ${rc} (open a new terminal to use it)"
}

main() {
    local os arch bin
    os=$(detect_os)
    arch=$(detect_arch)
    [ -n "$VERSION" ] || VERSION=$(latest_version)

    info "Installing ${BINARY_NAME} ${VERSION} for ${os} ${arch}"
    install_binary "$os" "$arch" "$VERSION"
    ensure_path
    bin="${INSTALL_DIR}/${BINARY_NAME}"

    echo ""
    # Child processes get </dev/null: under `curl | bash` stdin is the script itself.
    if ! "$bin" --help </dev/null 2>/dev/null | grep -q ' setup '; then
        warn "${VERSION} predates the 'setup' command; add the statusLine to ~/.claude/settings.json manually (see README)"
        return
    fi
    local setup_args=()
    [ -n "$EDITORS" ] && setup_args+=(--editors)
    set +e
    "$bin" setup ${setup_args[@]+"${setup_args[@]}"} </dev/null
    local rc=$?
    set -e
    case $rc in
        0) ;;
        2) warn "Your existing statusLine was kept. To switch to this plugin: ${bin} setup --force" ;;
        *) warn "setup did not complete; fix the issue above and run: ${bin} setup" ;;
    esac

    echo ""
    info "Checking your configuration (${bin} doctor):"
    "$bin" doctor </dev/null || true
}

main
