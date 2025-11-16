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
INSTALL_DIR="${TAKO_INSTALL_DIR:-/usr/local/bin}"
REPO="redentordev/tako-cli"
VERSION="${TAKO_VERSION:-latest}"

# Print with color
print_info() {
    echo -e "${CYAN}ℹ${NC} $1"
}

print_success() {
    echo -e "${GREEN}✓${NC} $1"
}

print_error() {
    echo -e "${RED}✗${NC} $1"
}

print_warning() {
    echo -e "${YELLOW}⚠${NC} $1"
}

print_header() {
    echo -e "${PURPLE}"
    echo "  ╔═══════════════════════════════════════╗"
    echo "  ║     🐙 Tako CLI Installer             ║"
    echo "  ║  Deploy to any VPS with zero config  ║"
    echo "  ╚═══════════════════════════════════════╝"
    echo -e "${NC}"
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

# Verify installation
verify_installation() {
    if ! command -v tako &> /dev/null; then
        print_warning "Tako CLI installed but not found in PATH"
        print_info "Add ${INSTALL_DIR} to your PATH or run: export PATH=\"${INSTALL_DIR}:\$PATH\""
        return
    fi
    
    local installed_version
    installed_version=$(tako --version 2>&1 | head -n1 || echo "unknown")
    
    print_success "Installation verified!"
    print_info "Version: ${installed_version}"
}

# Show next steps
show_next_steps() {
    echo ""
    echo -e "${GREEN}╔════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${GREEN}║  🎉 Tako CLI installed successfully!                      ║${NC}"
    echo -e "${GREEN}╚════════════════════════════════════════════════════════════╝${NC}"
    echo ""
    print_info "Get started:"
    echo ""
    echo "  1. Initialize a new project:"
    echo -e "     ${CYAN}tako init my-app${NC}"
    echo ""
    echo "  2. Configure your server in tako.yaml"
    echo ""
    echo "  3. Setup your server:"
    echo -e "     ${CYAN}tako setup -e production${NC}"
    echo ""
    echo "  4. Deploy your application:"
    echo -e "     ${CYAN}tako deploy -e production${NC}"
    echo ""
    print_info "Documentation: https://github.com/${REPO}"
    print_info "Built by @redentor_dev"
    echo ""
}

# Cleanup on error
cleanup() {
    if [ -n "$tmp_file" ] && [ -f "$tmp_file" ]; then
        rm -f "$tmp_file"
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
    
    # Install binary
    install_binary "$tmp_file"
    
    # Verify installation
    verify_installation
    
    # Show next steps
    show_next_steps
}

# Run main function
main
