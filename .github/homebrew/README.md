# Tako CLI Homebrew Tap

This directory contains the Homebrew formula for Tako CLI.

## Setup Instructions

### 1. Create a Homebrew Tap Repository

Create a new GitHub repository named `homebrew-tako` under your organization:
```bash
https://github.com/redentordev/homebrew-tako
```

### 2. Initialize the Tap Repository

```bash
mkdir homebrew-tako
cd homebrew-tako
git init
mkdir Formula
cp /path/to/tako-cli/.github/homebrew/tako.rb Formula/
git add Formula/tako.rb
git commit -m "Add Tako CLI formula"
git remote add origin https://github.com/redentordev/homebrew-tako.git
git push -u origin main
```

### 3. Update Formula with Checksums

After each release, update the `sha256` values in the formula:

```bash
# Download binaries
curl -LO https://github.com/redentordev/tako-cli/releases/download/v0.2.0/tako-darwin-arm64
curl -LO https://github.com/redentordev/tako-cli/releases/download/v0.2.0/tako-darwin-amd64
curl -LO https://github.com/redentordev/tako-cli/releases/download/v0.2.0/tako-linux-arm64
curl -LO https://github.com/redentordev/tako-cli/releases/download/v0.2.0/tako-linux-amd64

# Calculate checksums
shasum -a 256 tako-darwin-arm64
shasum -a 256 tako-darwin-amd64
shasum -a 256 tako-linux-arm64
shasum -a 256 tako-linux-amd64
```

Update the `sha256` values in `Formula/tako.rb` with the calculated checksums.

### 4. Usage

Once the tap is set up, users can install Tako CLI with:

```bash
# Add the tap
brew tap redentordev/tako

# Install Tako CLI
brew install tako

# Verify installation
tako --version

# Update to latest version
brew upgrade tako

# Uninstall
brew uninstall tako
```

## Automated Release Workflow

To automatically update the Homebrew formula on each release, add a GitHub Action workflow to the main `tako-cli` repository. Create `.github/workflows/homebrew.yml`:

```yaml
name: Update Homebrew Formula

on:
  release:
    types: [published]

jobs:
  update-formula:
    runs-on: ubuntu-latest
    steps:
      - name: Update Homebrew formula
        env:
          HOMEBREW_TAP_TOKEN: ${{ secrets.HOMEBREW_TAP_TOKEN }}
        run: |
          # This will be implemented in the future
          echo "Homebrew formula update automation coming soon"
```

## Benefits of Homebrew Installation

- ✅ Automatic PATH configuration
- ✅ Easy updates with `brew upgrade`
- ✅ Managed dependencies
- ✅ Trusted by developers
- ✅ Works on macOS and Linux
- ✅ Easy uninstallation

## Alternative: Homebrew Core

For wider distribution, you can submit Tako CLI to Homebrew Core after it reaches stability:
https://docs.brew.sh/Adding-Software-to-Homebrew
