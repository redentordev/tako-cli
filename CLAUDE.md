# Claude Code Instructions

## Git Commits

- **Author**: All commits must be authored by `redentordev <redenvalerio2@gmail.com>`
- **No Co-Authored-By**: Do NOT add `Co-Authored-By: Claude` or any Claude attribution to commit messages
- **Commit style**: Use conventional commit messages (e.g., "Add feature X", "Fix bug Y")

## Project Overview

Tako CLI is a deployment automation tool that brings PaaS-like simplicity to your own servers. It deploys Docker containers to VPS servers with automatic HTTPS, health checks, and zero-downtime deployments.

## Key Directories

- `cmd/` - CLI commands
- `pkg/` - Core packages
- `pkg/provisioner/` - Existing server setup over SSH
- `examples/` - Example projects
- `docs/` - Documentation

## Security

- Never commit `.env` files or `.tako/` directories
- Never hardcode API tokens or credentials
- Use `${ENV_VAR}` syntax for credentials in tako.yaml
- All examples should use `.env.example` with placeholder values

## Testing

```bash
go build ./...   # Build all packages
go test ./...    # Run tests
```

## Deployment Scope

Tako deploys to existing VPS hosts declared in `servers`. It does not create cloud provider infrastructure.
