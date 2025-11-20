#!/bin/bash
# 🐙 Tako CLI Uninstall Script
# Usage: curl -fsSL https://raw.githubusercontent.com/redentordev/tako-cli/master/uninstall.sh | bash

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
PURPLE='\033[0;35m'
NC='\033[0m' # No Color

# Print with color (output to stderr)
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
    echo "  ║     🐙 Tako CLI Uninstaller           ║" >&2
    echo "  ╚═══════════════════════════════════════╝" >&2
    echo -e "${NC}" >&2
}

# Find Tako binary
find_tako_binary() {
    if command -v tako &> /dev/null; then
        which tako
        return 0
    fi

    # Check common installation locations
    local locations=(
        "/usr/local/bin/tako"
        "/usr/bin/tako"
        "$HOME/.local/bin/tako"
    )

    for location in "${locations[@]}"; do
        if [ -f "$location" ]; then
            echo "$location"
            return 0
        fi
    done

    return 1
}

# Remove Tako binary
remove_binary() {
    local binary_path=$1

    if [ ! -f "$binary_path" ]; then
        print_error "Tako binary not found at ${binary_path}"
        return 1
    fi

    local install_dir
    install_dir=$(dirname "$binary_path")

    print_info "Removing Tako CLI from ${binary_path}..."

    # Check if we need sudo
    if [ ! -w "$install_dir" ]; then
        if ! sudo rm -f "$binary_path"; then
            print_error "Failed to remove ${binary_path}"
            return 1
        fi
    else
        if ! rm -f "$binary_path"; then
            print_error "Failed to remove ${binary_path}"
            return 1
        fi
    fi

    print_success "Tako CLI binary removed"
    return 0
}

# Clean PATH entries from shell config files
clean_path_entries() {
    print_info "Cleaning PATH entries from shell configuration files..."

    local config_files=(
        "$HOME/.bashrc"
        "$HOME/.bash_profile"
        "$HOME/.zshrc"
        "$HOME/.profile"
        "$HOME/.config/fish/config.fish"
    )

    local removed=0

    for config_file in "${config_files[@]}"; do
        if [ -f "$config_file" ]; then
            # Check if file contains Tako CLI installer comments
            if grep -q "# Added by Tako CLI installer" "$config_file" 2>/dev/null; then
                # Create a backup
                cp "$config_file" "${config_file}.bak.tako"

                # Remove Tako CLI entries (comment line and export line)
                sed -i.tmp '/# Added by Tako CLI installer/,+1d' "$config_file" 2>/dev/null || \
                    sed -i '' '/# Added by Tako CLI installer/,+1d' "$config_file" 2>/dev/null || true

                # Remove the .tmp file if it was created
                rm -f "${config_file}.tmp"

                print_success "Cleaned PATH entry from ${config_file}"
                print_info "Backup saved as ${config_file}.bak.tako"
                removed=1
            fi
        fi
    done

    if [ $removed -eq 0 ]; then
        print_info "No PATH entries found to clean"
    fi
}

# Main uninstall function
main() {
    print_header

    # Find Tako binary
    print_info "Looking for Tako CLI installation..."

    local tako_path
    if tako_path=$(find_tako_binary); then
        print_info "Found Tako CLI at: ${tako_path}"

        # Remove binary
        if remove_binary "$tako_path"; then
            # Clean PATH entries
            clean_path_entries

            echo "" >&2
            echo -e "${GREEN}╔════════════════════════════════════════════════════════════╗${NC}" >&2
            echo -e "${GREEN}║  Tako CLI uninstalled successfully                        ║${NC}" >&2
            echo -e "${GREEN}╚════════════════════════════════════════════════════════════╝${NC}" >&2
            echo "" >&2
            print_warning "Please restart your shell to apply PATH changes"
            echo "" >&2
        else
            print_error "Failed to remove Tako CLI binary"
            exit 1
        fi
    else
        print_warning "Tako CLI not found on this system"
        print_info "If you know the installation path, you can manually remove it:"
        print_info "  sudo rm /path/to/tako"
        exit 1
    fi
}

# Run main function
main
