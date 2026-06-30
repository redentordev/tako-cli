# Tako CLI Homebrew Tap

The production Homebrew tap lives in:

https://github.com/redentordev/homebrew-tako

This directory only documents how the tap is maintained from `tako-cli`
releases. Do not keep a second formula template here; the tap repository is the
source of truth for `Formula/tako.rb`.

## User Install

```bash
brew install redentordev/tako/tako
```

Or tap first:

```bash
brew tap redentordev/tako
brew install tako
```

## Release Update Checklist

1. Push a `vX.Y.Z` tag in this repository and wait for the release workflow to
   verify and publish all binary assets plus `checksums.txt`.
2. Download the release checksums:

   ```bash
   gh release download vX.Y.Z \
     --repo redentordev/tako-cli \
     --pattern checksums.txt \
     --dir /tmp/tako-release
   ```

3. Update `Formula/tako.rb` in `redentordev/homebrew-tako` with the new
   version, binary URLs, SHA256 values, and `tako-manpages.tar.gz` asset.
   The formula should install the manual pages into `man1`.
4. Commit and push the tap update.
5. Verify the published tap:

   ```bash
   brew update
   brew upgrade redentordev/tako/tako
   tako --version
   brew test redentordev/tako/tako
   ```
