# Tako CLI Shell Completions

Shell completion files for Tako CLI that provide tab completion for commands, flags, and arguments.

## Installation

### Bash

**Linux:**
```bash
# System-wide (requires root)
sudo cp tako.bash /etc/bash_completion.d/tako

# User-specific
mkdir -p ~/.local/share/bash-completion/completions
cp tako.bash ~/.local/share/bash-completion/completions/tako
```

**macOS (with Homebrew bash-completion):**
```bash
# Install bash-completion if not already installed
brew install bash-completion@2

# Copy completion file
cp tako.bash $(brew --prefix)/etc/bash_completion.d/tako
```

Then restart your shell or run:
```bash
source ~/.bashrc  # or ~/.bash_profile on macOS
```

### Zsh

```bash
# For Oh My Zsh
mkdir -p ~/.oh-my-zsh/completions
cp tako.zsh ~/.oh-my-zsh/completions/_tako

# For standard zsh
mkdir -p ~/.zsh/completion
cp tako.zsh ~/.zsh/completion/_tako

# Add to ~/.zshrc if not already present:
fpath=(~/.zsh/completion $fpath)
autoload -Uz compinit && compinit
```

Restart your shell or run:
```bash
source ~/.zshrc
```

### Fish

```bash
# Copy to fish completions directory
mkdir -p ~/.config/fish/completions
cp tako.fish ~/.config/fish/completions/tako.fish
```

Fish will automatically load the completions. No restart needed!

## Usage

Once installed, you can use tab completion with Tako CLI:

```bash
# Complete commands
tako <TAB>
# Shows: access, cleanup, deploy, destroy, dev, ...

# Complete flags
tako deploy --<TAB>
# Shows: --env, --server, --service, --config, --verbose

# Complete subcommands
tako secrets <TAB>
# Shows: init, set, list, delete, validate
```

## Automatic Installation

The install script can automatically install completions. During installation, completions will be placed in the appropriate directory for your shell.

## Benefits

- **Faster workflow**: No need to remember all commands and flags
- **Fewer typos**: Tab completion prevents command errors
- **Discovery**: See available options without checking documentation
- **Context-aware**: Completions adapt based on the command you're typing

## Supported Shells

- ✅ **Bash** (4.0+)
- ✅ **Zsh** (5.0+)
- ✅ **Fish** (3.0+)

## Troubleshooting

### Bash completions not working

1. Ensure bash-completion is installed:
   ```bash
   # Ubuntu/Debian
   sudo apt install bash-completion

   # macOS
   brew install bash-completion@2
   ```

2. Check if completions are loading:
   ```bash
   complete -p tako
   ```

### Zsh completions not working

1. Ensure compinit is called in your ~/.zshrc:
   ```zsh
   autoload -Uz compinit && compinit
   ```

2. Clear the completion cache:
   ```bash
   rm -f ~/.zcompdump
   compinit
   ```

### Fish completions not working

1. Check if the file is in the right location:
   ```bash
   ls ~/.config/fish/completions/tako.fish
   ```

2. Reload completions:
   ```bash
   fish_update_completions
   ```

## Contributing

To update completions when new commands are added:

1. Edit the appropriate completion file(s)
2. Add new commands and their descriptions
3. Update flags and arguments
4. Test the completions
5. Submit a pull request

For more information about shell completions:
- [Bash Completion](https://github.com/scop/bash-completion)
- [Zsh Completion](https://github.com/zsh-users/zsh-completions)
- [Fish Completion](https://fishshell.com/docs/current/completions.html)
