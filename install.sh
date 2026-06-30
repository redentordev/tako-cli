#!/bin/bash
# 🐙 Tako CLI Installation Script
# Usage: curl -fsSL https://raw.githubusercontent.com/redentordev/tako-cli/master/install.sh | bash

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
PURPLE='\033[0;35m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

# Default values
REPO="redentordev/tako-cli"
VERSION="${TAKO_VERSION:-latest}"
VERIFY_CHECKSUM="${TAKO_VERIFY_CHECKSUM:-true}"
INSTALL_MANPAGES="${TAKO_INSTALL_MANPAGES:-true}"
MAN_DIR="${TAKO_MAN_DIR:-}"
tmp_file=""
tmp_man_archive=""
tmp_man_dir=""

# Determine best install directory
if [ -n "$TAKO_INSTALL_DIR" ]; then
    INSTALL_DIR="$TAKO_INSTALL_DIR"
elif [ -w "/usr/local/bin" ]; then
    INSTALL_DIR="/usr/local/bin"
elif [ -w "$HOME/.local/bin" ]; then
    INSTALL_DIR="$HOME/.local/bin"
else
    INSTALL_DIR="$HOME/.local/bin"
    mkdir -p "$INSTALL_DIR"
fi

# Print with color (output to stderr to avoid interfering with function returns)
print_info() {
    echo -e "${CYAN}ℹ${NC} $1" >&2
}

print_success() {
    echo -e "${GREEN}✓${NC} $1" >&2
}

print_error() {
    echo -e "${RED}✗${NC} $1" >&2
}

print_warning() {
    echo -e "${YELLOW}⚠${NC} $1" >&2
}

print_header() {
    echo -e "${PURPLE}" >&2
    echo "  ╔═══════════════════════════════════════╗" >&2
    echo "  ║     🐙 Tako CLI Installer             ║" >&2
    echo "  ║  Deploy to any VPS with zero config  ║" >&2
    echo "  ╚═══════════════════════════════════════╝" >&2
    echo -e "${NC}" >&2
}

# Detect OS and architecture
detect_platform() {
    local os
    local arch
    
    # Detect OS
    case "$(uname -s)" in
        Linux*)     os="linux" ;;
        Darwin*)    os="darwin" ;;
        MINGW*|MSYS*|CYGWIN*)
            print_error "Windows is not supported by this script."
            print_info "Please download the binary manually from:"
            print_info "https://github.com/${REPO}/releases/latest"
            exit 1
            ;;
        *)
            print_error "Unsupported operating system: $(uname -s)"
            exit 1
            ;;
    esac
    
    # Detect architecture
    case "$(uname -m)" in
        x86_64|amd64)
            arch="amd64"
            ;;
        aarch64|arm64)
            arch="arm64"
            ;;
        armv7l)
            print_error "ARM v7 is not supported. Only ARM64 is available."
            exit 1
            ;;
        *)
            print_error "Unsupported architecture: $(uname -m)"
            exit 1
            ;;
    esac
    
    echo "${os}-${arch}"
}

# Get latest release version
get_latest_version() {
    local latest_url="https://api.github.com/repos/${REPO}/releases/latest"
    
    if command -v curl &> /dev/null; then
        VERSION=$(curl -fsSL "$latest_url" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
    elif command -v wget &> /dev/null; then
        VERSION=$(wget -qO- "$latest_url" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
    else
        print_error "Neither curl nor wget is available. Please install one of them."
        exit 1
    fi
    
    if [ -z "$VERSION" ]; then
        print_error "Failed to fetch latest version"
        exit 1
    fi
}

# Download binary
download_binary() {
    local platform=$1
    local binary_name="tako-${platform}"
    local download_url="https://github.com/${REPO}/releases/download/${VERSION}/${binary_name}"
    local tmp_file="/tmp/tako-${platform}-$$"

    print_info "Downloading Tako CLI ${VERSION} for ${platform}..."

    if command -v curl &> /dev/null; then
        if ! curl -fsSL "$download_url" -o "$tmp_file"; then
            print_error "Failed to download binary from $download_url"
            exit 1
        fi
    elif command -v wget &> /dev/null; then
        if ! wget -q "$download_url" -O "$tmp_file"; then
            print_error "Failed to download binary from $download_url"
            exit 1
        fi
    fi

    echo "$tmp_file"
}

# Download and verify checksum
download_checksums() {
    local checksums_url="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"
    local tmp_checksums="/tmp/tako-checksums-$$"

    if command -v curl &> /dev/null; then
        if ! curl -fsSL "$checksums_url" -o "$tmp_checksums" 2>/dev/null; then
            return 1
        fi
    elif command -v wget &> /dev/null; then
        if ! wget -q "$checksums_url" -O "$tmp_checksums" 2>/dev/null; then
            return 1
        fi
    fi

    echo "$tmp_checksums"
}

# Verify a release asset checksum
verify_asset_checksum() {
    local asset_file=$1
    local asset_name=$2

    if [ "$VERIFY_CHECKSUM" != "true" ]; then
        return 0
    fi

    print_info "Verifying checksum..."

    local checksums_file
    if ! checksums_file=$(download_checksums); then
        print_error "Failed to download checksums.txt"
        return 1
    fi
    if [ -z "$checksums_file" ] || [ ! -f "$checksums_file" ]; then
        print_error "Checksum file not found"
        return 1
    fi

    # Check if sha256sum or shasum is available
    local sha_cmd
    if command -v sha256sum &> /dev/null; then
        sha_cmd="sha256sum"
    elif command -v shasum &> /dev/null; then
        sha_cmd="shasum -a 256"
    else
        print_error "Neither sha256sum nor shasum found; cannot verify download"
        rm -f "$checksums_file"
        return 1
    fi

    # Calculate checksum of downloaded asset
    local calculated_checksum
    calculated_checksum=$($sha_cmd "$asset_file" | awk '{print $1}')

    # Get expected checksum from checksums file
    local expected_checksum
    expected_checksum=$(awk -v name="$asset_name" '$2 == name || $2 == "*" name { print $1; found = 1; exit } END { if (!found) exit 1 }' "$checksums_file" || true)

    rm -f "$checksums_file"

    if [ -z "$expected_checksum" ]; then
        print_error "Checksum for ${asset_name} not found in checksums.txt"
        return 1
    fi

    if ! echo "$expected_checksum" | grep -Eq '^[0-9a-fA-F]{64}$'; then
        print_error "Checksum for ${asset_name} is not a valid SHA-256 digest"
        return 1
    fi

    if [ "$calculated_checksum" != "$expected_checksum" ]; then
        print_error "Checksum verification failed!"
        print_error "Expected: $expected_checksum"
        print_error "Got:      $calculated_checksum"
        return 1
    fi

    print_success "Checksum verified"
    return 0
}

# Verify binary checksum
verify_checksum() {
    local binary_file=$1
    local platform=$2
    local binary_name="tako-${platform}"

    verify_asset_checksum "$binary_file" "$binary_name"
}

# Install binary
install_binary() {
    local tmp_file=$1
    local target_file="${INSTALL_DIR}/tako"
    
    # Make binary executable
    chmod +x "$tmp_file"
    
    # Check if we need sudo
    if [ ! -w "$INSTALL_DIR" ]; then
        print_info "Installing to ${INSTALL_DIR} (requires sudo)..."
        if ! sudo mv "$tmp_file" "$target_file"; then
            print_error "Failed to install binary to ${target_file}"
            print_info "You can manually copy it: sudo mv $tmp_file $target_file"
            exit 1
        fi
        sudo chmod +x "$target_file"
    else
        print_info "Installing to ${INSTALL_DIR}..."
        if ! mv "$tmp_file" "$target_file"; then
            print_error "Failed to install binary to ${target_file}"
            exit 1
        fi
        chmod +x "$target_file"
    fi
    
    print_success "Tako CLI installed to ${target_file}"
}

# Resolve manpage install directory
resolve_man_dir() {
    if [ -n "$MAN_DIR" ]; then
        echo "$MAN_DIR"
        return 0
    fi

    if [ -d "/usr/local/share/man/man1" ] && [ -w "/usr/local/share/man/man1" ]; then
        echo "/usr/local/share/man/man1"
        return 0
    fi
    if [ -d "/usr/local/share/man" ] && [ -w "/usr/local/share/man" ]; then
        echo "/usr/local/share/man/man1"
        return 0
    fi
    if [ ! -e "/usr/local/share/man" ] && [ -w "/usr/local/share" ]; then
        echo "/usr/local/share/man/man1"
        return 0
    fi

    echo "$HOME/.local/share/man/man1"
}

# Download and install man pages
install_man_pages() {
    if [ "$INSTALL_MANPAGES" != "true" ]; then
        return 0
    fi
    if ! command -v tar &> /dev/null; then
        print_warning "tar not found; skipping manual page installation"
        return 0
    fi

    local asset_name="tako-manpages.tar.gz"
    local download_url="https://github.com/${REPO}/releases/download/${VERSION}/${asset_name}"
    tmp_man_archive="/tmp/tako-manpages-$$.tar.gz"

    print_info "Downloading manual pages..."
    if command -v curl &> /dev/null; then
        if ! curl -fsSL "$download_url" -o "$tmp_man_archive"; then
            print_warning "Manual pages are not available for ${VERSION}; skipping"
            rm -f "$tmp_man_archive"
            tmp_man_archive=""
            return 0
        fi
    elif command -v wget &> /dev/null; then
        if ! wget -q "$download_url" -O "$tmp_man_archive"; then
            print_warning "Manual pages are not available for ${VERSION}; skipping"
            rm -f "$tmp_man_archive"
            tmp_man_archive=""
            return 0
        fi
    else
        print_warning "Neither curl nor wget is available; skipping manual page installation"
        return 0
    fi

    if ! verify_asset_checksum "$tmp_man_archive" "$asset_name"; then
        print_warning "Manual page checksum verification failed; skipping manual page installation"
        rm -f "$tmp_man_archive"
        tmp_man_archive=""
        return 0
    fi

    if ! tmp_man_dir=$(mktemp -d "${TMPDIR:-/tmp}/tako-manpages.XXXXXX"); then
        print_warning "Failed to create temporary directory; skipping manual page installation"
        return 0
    fi
    if ! tar -xzf "$tmp_man_archive" -C "$tmp_man_dir"; then
        print_warning "Failed to extract manual pages; skipping"
        return 0
    fi

    local target_dir
    target_dir=$(resolve_man_dir)
    if [ ! -d "$target_dir" ]; then
        if ! mkdir -p "$target_dir" 2>/dev/null; then
            if ! sudo mkdir -p "$target_dir"; then
                print_warning "Could not create ${target_dir}; skipping manual page installation"
                return 0
            fi
        fi
    fi

    local found=false
    local page
    for page in "$tmp_man_dir"/*.1; do
        [ -e "$page" ] || continue
        found=true
        if [ -w "$target_dir" ]; then
            if ! install -m 0644 "$page" "$target_dir/"; then
                print_warning "Failed to install manual pages to ${target_dir}; skipping"
                return 0
            fi
        else
            if ! sudo install -m 0644 "$page" "$target_dir/"; then
                print_warning "Failed to install manual pages to ${target_dir}; skipping"
                return 0
            fi
        fi
    done
    if [ "$found" != "true" ]; then
        print_warning "Manual page archive did not contain .1 files; skipping"
        return 0
    fi

    print_success "Manual pages installed to ${target_dir}"
}

# Detect user's shell
detect_shell() {
    if [ -n "$BASH_VERSION" ]; then
        echo "bash"
    elif [ -n "$ZSH_VERSION" ]; then
        echo "zsh"
    elif [ -n "$FISH_VERSION" ]; then
        echo "fish"
    else
        # Fallback to $SHELL environment variable
        basename "$SHELL"
    fi
}

# Get shell config file
get_shell_config() {
    local shell_name=$1
    case "$shell_name" in
        bash)
            if [ -f "$HOME/.bashrc" ]; then
                echo "$HOME/.bashrc"
            elif [ -f "$HOME/.bash_profile" ]; then
                echo "$HOME/.bash_profile"
            else
                echo "$HOME/.profile"
            fi
            ;;
        zsh)
            echo "$HOME/.zshrc"
            ;;
        fish)
            echo "$HOME/.config/fish/config.fish"
            ;;
        *)
            echo "$HOME/.profile"
            ;;
    esac
}

# Configure PATH automatically
configure_path() {
    local dir=$1

    # Check if directory is already in PATH
    if echo "$PATH" | grep -q "$dir"; then
        return 0
    fi

    local shell_name
    shell_name=$(detect_shell)

    local config_file
    config_file=$(get_shell_config "$shell_name")

    # Check if PATH export already exists in config file
    if [ -f "$config_file" ] && grep -q "export PATH.*${dir}" "$config_file" 2>/dev/null; then
        return 0
    fi

    print_info "Adding ${dir} to PATH in ${config_file}..."

    # Create config file if it doesn't exist
    if [ ! -f "$config_file" ]; then
        mkdir -p "$(dirname "$config_file")"
        touch "$config_file"
    fi

    # Add PATH export to config file
    if [ "$shell_name" = "fish" ]; then
        echo "" >> "$config_file"
        echo "# Added by Tako CLI installer" >> "$config_file"
        echo "set -gx PATH $dir \$PATH" >> "$config_file"
    else
        echo "" >> "$config_file"
        echo "# Added by Tako CLI installer" >> "$config_file"
        echo "export PATH=\"${dir}:\$PATH\"" >> "$config_file"
    fi

    print_success "PATH configured in ${config_file}"
    print_warning "Please restart your shell or run: source ${config_file}"
}

# Verify installation
verify_installation() {
    if ! command -v tako &> /dev/null; then
        print_warning "Tako CLI installed but not found in PATH"

        # Try to configure PATH automatically if not in standard location
        if [ "$INSTALL_DIR" != "/usr/local/bin" ] && [ "$INSTALL_DIR" != "/usr/bin" ]; then
            configure_path "$INSTALL_DIR"
        else
            print_info "Add ${INSTALL_DIR} to your PATH or run: export PATH=\"${INSTALL_DIR}:\$PATH\""
        fi
        return
    fi

    local installed_version
    installed_version=$(tako --version 2>&1 | head -n1 || echo "unknown")

    print_success "Installation verified!"
    print_info "Version: ${installed_version}"
}

# Show next steps
show_next_steps() {
    echo "" >&2
    echo -e "${GREEN}╔════════════════════════════════════════════════════════════╗${NC}" >&2
    echo -e "${GREEN}║  🎉 Tako CLI installed successfully!                      ║${NC}" >&2
    echo -e "${GREEN}╚════════════════════════════════════════════════════════════╝${NC}" >&2
    echo "" >&2
    print_info "Get started:"
    echo "" >&2
    echo "  1. Initialize a new project:" >&2
    echo -e "     ${CYAN}tako init my-app${NC}" >&2
    echo "" >&2
    echo "  2. Configure your server in tako.yaml" >&2
    echo "" >&2
    echo "  3. Setup your server:" >&2
    echo -e "     ${CYAN}tako setup -e production${NC}" >&2
    echo "" >&2
    echo "  4. Deploy your application:" >&2
    echo -e "     ${CYAN}tako deploy -e production${NC}" >&2
    echo "" >&2
    print_info "Documentation: https://github.com/${REPO}"
    print_info "Built by @redentor_dev"
    echo "" >&2
}

# Cleanup on error
cleanup() {
    if [ -n "$tmp_file" ] && [ -f "$tmp_file" ]; then
        rm -f "$tmp_file"
    fi
    if [ -n "$tmp_man_archive" ] && [ -f "$tmp_man_archive" ]; then
        rm -f "$tmp_man_archive"
    fi
    if [ -n "$tmp_man_dir" ] && [ -d "$tmp_man_dir" ]; then
        rm -rf "$tmp_man_dir"
    fi
}

trap cleanup EXIT

# Main installation flow
main() {
    print_header
    
    # Detect platform
    platform=$(detect_platform)
    print_info "Detected platform: ${platform}"
    
    # Get latest version if not specified
    if [ "$VERSION" = "latest" ]; then
        print_info "Fetching latest version..."
        get_latest_version
    fi
    
    print_info "Installing version: ${VERSION}"
    
    # Download binary
    tmp_file=$(download_binary "$platform")

    # Verify checksum
    if ! verify_checksum "$tmp_file" "$platform"; then
        print_error "Installation aborted due to checksum verification failure"
        exit 1
    fi

    # Install binary
    install_binary "$tmp_file"

    # Install manual pages when the release provides them
    install_man_pages
    
    # Verify installation
    verify_installation
    
    # Show next steps
    show_next_steps
}

# Run main function
main
