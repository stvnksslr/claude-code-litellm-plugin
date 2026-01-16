#!/bin/bash
set -e

REPO="stvnksslr/claude-code-litellm-plugin"
BINARY_NAME="claude-code-litellm-plugin"
INSTALL_DIR="${HOME}/.local/bin"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m' # No Color

info() { echo -e "${GREEN}[INFO]${NC} $1"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
error() { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }

# Detect OS
detect_os() {
    case "$(uname -s)" in
        Linux*)  echo "Linux" ;;
        Darwin*) echo "Darwin" ;;
        *)       error "Unsupported operating system: $(uname -s)" ;;
    esac
}

# Detect architecture
detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)  echo "x86_64" ;;
        arm64|aarch64) echo "arm64" ;;
        i386|i686)     echo "i386" ;;
        *)             error "Unsupported architecture: $(uname -m)" ;;
    esac
}

# Get latest release tag from GitHub
get_latest_version() {
    local latest
    latest=$(curl -sL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
    if [ -z "$latest" ]; then
        error "Failed to fetch latest release version"
    fi
    echo "$latest"
}

# Download and install binary
install_binary() {
    local os=$1
    local arch=$2
    local version=$3

    local archive_name="${BINARY_NAME}_${os}_${arch}.tar.gz"
    local download_url="https://github.com/${REPO}/releases/download/${version}/${archive_name}"

    info "Downloading ${archive_name}..."

    local tmp_dir
    tmp_dir=$(mktemp -d)
    trap "rm -rf ${tmp_dir}" EXIT

    if ! curl -sL "${download_url}" -o "${tmp_dir}/${archive_name}"; then
        error "Failed to download ${download_url}"
    fi

    info "Extracting archive..."
    tar -xzf "${tmp_dir}/${archive_name}" -C "${tmp_dir}"

    # Create install directory if it doesn't exist
    mkdir -p "${INSTALL_DIR}"

    # Install binary
    mv "${tmp_dir}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
    chmod +x "${INSTALL_DIR}/${BINARY_NAME}"

    info "Installed ${BINARY_NAME} to ${INSTALL_DIR}"
}

# Add to PATH if needed
ensure_path() {
    if [[ ":$PATH:" != *":${INSTALL_DIR}:"* ]]; then
        warn "${INSTALL_DIR} is not in your PATH"

        local shell_rc=""
        case "${SHELL}" in
            */bash) shell_rc="${HOME}/.bashrc" ;;
            */zsh)  shell_rc="${HOME}/.zshrc" ;;
            *)      shell_rc="${HOME}/.profile" ;;
        esac

        echo "" >> "${shell_rc}"
        echo "# Added by claude-code-litellm-plugin installer" >> "${shell_rc}"
        echo "export PATH=\"\${PATH}:${INSTALL_DIR}\"" >> "${shell_rc}"

        info "Added ${INSTALL_DIR} to PATH in ${shell_rc}"
        warn "Run 'source ${shell_rc}' or restart your terminal to update PATH"
    fi
}

# Configure Claude settings
configure_claude() {
    local claude_dir="${HOME}/.claude"
    local settings_file="${claude_dir}/settings.json"

    mkdir -p "${claude_dir}"

    if [ -f "${settings_file}" ]; then
        # Check if jq is available
        if command -v jq &> /dev/null; then
            # Use jq to merge settings
            local tmp_file
            tmp_file=$(mktemp)
            jq '.statusLine = {"type": "command", "command": "claude-code-litellm-plugin"}' "${settings_file}" > "${tmp_file}"
            mv "${tmp_file}" "${settings_file}"
            info "Updated Claude settings in ${settings_file}"
        else
            # Manual check and update without jq
            if grep -q '"statusLine"' "${settings_file}"; then
                warn "statusLine already exists in ${settings_file}. Please update manually if needed."
            else
                # Remove trailing } and add statusLine
                sed -i.bak 's/}$/,\n  "statusLine": {\n    "type": "command",\n    "command": "claude-code-litellm-plugin"\n  }\n}/' "${settings_file}"
                rm -f "${settings_file}.bak"
                info "Updated Claude settings in ${settings_file}"
            fi
        fi
    else
        # Create new settings file
        cat > "${settings_file}" << 'EOF'
{
  "statusLine": {
    "type": "command",
    "command": "claude-code-litellm-plugin"
  }
}
EOF
        info "Created Claude settings at ${settings_file}"
    fi
}

main() {
    info "Installing ${BINARY_NAME}..."

    local os arch version
    os=$(detect_os)
    arch=$(detect_arch)
    version=$(get_latest_version)

    info "Detected: ${os} ${arch}"
    info "Latest version: ${version}"

    install_binary "${os}" "${arch}" "${version}"
    ensure_path
    configure_claude

    echo ""
    info "Installation complete!"

    # Check if required env vars are set
    local missing_vars=()
    if [ -z "${ANTHROPIC_BASE_URL:-}" ] && [ -z "${LITELLM_PROXY_URL:-}" ]; then
        missing_vars+=("ANTHROPIC_BASE_URL or LITELLM_PROXY_URL")
    fi
    if [ -z "${ANTHROPIC_AUTH_TOKEN:-}" ] && [ -z "${LITELLM_PROXY_API_KEY:-}" ]; then
        missing_vars+=("ANTHROPIC_AUTH_TOKEN or LITELLM_PROXY_API_KEY")
    fi

    if [ ${#missing_vars[@]} -gt 0 ]; then
        warn "Missing environment variables:"
        for var in "${missing_vars[@]}"; do
            warn "  - $var"
        done
    fi
}

main "$@"
