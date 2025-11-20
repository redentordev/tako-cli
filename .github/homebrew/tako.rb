# Homebrew Formula for Tako CLI
# To use this tap:
#   brew tap redentordev/tako
#   brew install tako

class Tako < Formula
  desc "Deploy to any VPS with zero config & zero downtime"
  homepage "https://github.com/redentordev/tako-cli"
  version "0.0.1"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/redentordev/tako-cli/releases/download/v0.0.1/tako-darwin-arm64"
      sha256 "" # Will be filled by release automation
    else
      url "https://github.com/redentordev/tako-cli/releases/download/v0.0.1/tako-darwin-amd64"
      sha256 "" # Will be filled by release automation
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/redentordev/tako-cli/releases/download/v0.0.1/tako-linux-arm64"
      sha256 "" # Will be filled by release automation
    else
      url "https://github.com/redentordev/tako-cli/releases/download/v0.0.1/tako-linux-amd64"
      sha256 "" # Will be filled by release automation
    end
  end

  def install
    bin.install "tako-darwin-arm64" => "tako" if OS.mac? && Hardware::CPU.arm?
    bin.install "tako-darwin-amd64" => "tako" if OS.mac? && Hardware::CPU.intel?
    bin.install "tako-linux-arm64" => "tako" if OS.linux? && Hardware::CPU.arm?
    bin.install "tako-linux-amd64" => "tako" if OS.linux? && Hardware::CPU.intel?
  end

  test do
    system "#{bin}/tako", "--version"
  end
end
