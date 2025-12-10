#compdef tako

# zsh completion for tako
# Install: cp tako.zsh ~/.zsh/completion/_tako && autoload -Uz compinit && compinit

_tako() {
    local -a commands

    _arguments -C \
        '(-h --help)'{-h,--help}'[Show help information]' \
        '(--version)--version[Show version information]' \
        '(-v --verbose)'{-v,--verbose}'[Show detailed output]' \
        '(-e --env)'{-e,--env}'[Target specific environment]:environment:' \
        '(--config)--config[Use custom config file]:file:_files' \
        '1: :->cmds' \
        '*:: :->args'

    case $state in
        cmds)
            commands=(
                'access:Stream access logs from Traefik (HTTP requests)'
                'backup:Backup and restore Docker volumes'
                'cleanup:Clean up old Docker resources'
                'deploy:Deploy application to environment'
                'destroy:Remove all services from server'
                'dev:Run production environment locally'
                'downgrade:Downgrade from Docker Swarm to single-server mode'
                'drift:Detect configuration drift between config and running services'
                'exec:Execute commands on remote server(s) or in containers'
                'history:View deployment history'
                'init:Initialize new project with template config'
                'live:Live development mode with hot reload'
                'logs:Stream container logs'
                'maintenance:Enable/disable maintenance mode'
                'metrics:View system metrics from servers'
                'monitor:Continuously monitor deployed services'
                'prometheus:Start Prometheus metrics endpoint'
                'ps:List running services and their status'
                'remove:Remove a specific service'
                'rollback:Rollback to previous/specific deployment'
                'scale:Scale service replicas'
                'secrets:Manage secrets for your project'
                'setup:Provision server (Docker, Traefik, security)'
                'start:Start stopped services'
                'stats:Show container resource usage'
                'stop:Stop running services'
                'upgrade:Upgrade Tako CLI to the latest version'
            )
            _describe -t commands 'tako commands' commands
            ;;
        args)
            case $words[1] in
                deploy)
                    _arguments \
                        '(-e --env)'{-e,--env}'[Target specific environment]:environment:' \
                        '(-s --server)'{-s,--server}'[Target specific server]:server:' \
                        '(--service)--service[Deploy specific service only]:service:' \
                        '(--config)--config[Use custom config file]:file:_files' \
                        '(-v --verbose)'{-v,--verbose}'[Show detailed output]' \
                        '(-y --yes)'{-y,--yes}'[Skip confirmation prompts]' \
                        '(-m --message)'{-m,--message}'[Commit message for uncommitted changes]:message:' \
                        '(--skip-build)--skip-build[Skip building Docker image]' \
                        '(--skip-hooks)--skip-hooks[Skip pre/post deploy hooks]'
                    ;;
                setup|destroy)
                    _arguments \
                        '(-e --env)'{-e,--env}'[Target specific environment]:environment:' \
                        '(-s --server)'{-s,--server}'[Target specific server]:server:' \
                        '(--config)--config[Use custom config file]:file:_files' \
                        '(-v --verbose)'{-v,--verbose}'[Show detailed output]' \
                        '(-f --force)'{-f,--force}'[Force operation without confirmation]'
                    ;;
                logs)
                    _arguments \
                        '(-e --env)'{-e,--env}'[Target specific environment]:environment:' \
                        '(-s --server)'{-s,--server}'[Target specific server]:server:' \
                        '(--service)--service[Target specific service]:service:' \
                        '(--config)--config[Use custom config file]:file:_files' \
                        '(-v --verbose)'{-v,--verbose}'[Show detailed output]' \
                        '(-f --follow)'{-f,--follow}'[Follow log output]' \
                        '(-n --tail)'{-n,--tail}'[Number of lines to show]:lines:' \
                        '(--since)--since[Show logs since timestamp]:timestamp:'
                    ;;
                access)
                    _arguments \
                        '(-e --env)'{-e,--env}'[Target specific environment]:environment:' \
                        '(-s --server)'{-s,--server}'[Target specific server]:server:' \
                        '(--config)--config[Use custom config file]:file:_files' \
                        '(-v --verbose)'{-v,--verbose}'[Show detailed output]' \
                        '(-f --follow)'{-f,--follow}'[Follow log output]' \
                        '(-n --tail)'{-n,--tail}'[Number of lines to show]:lines:' \
                        '(--filter)--filter[Filter by path or status]:filter:'
                    ;;
                ps|stats)
                    _arguments \
                        '(-e --env)'{-e,--env}'[Target specific environment]:environment:' \
                        '(-s --server)'{-s,--server}'[Target specific server]:server:' \
                        '(--config)--config[Use custom config file]:file:_files' \
                        '(-v --verbose)'{-v,--verbose}'[Show detailed output]'
                    ;;
                start|stop)
                    _arguments \
                        '(-e --env)'{-e,--env}'[Target specific environment]:environment:' \
                        '(-s --server)'{-s,--server}'[Target specific server]:server:' \
                        '(--service)--service[Target specific service]:service:' \
                        '(--config)--config[Use custom config file]:file:_files' \
                        '(-v --verbose)'{-v,--verbose}'[Show detailed output]'
                    ;;
                rollback)
                    _arguments \
                        '(-e --env)'{-e,--env}'[Target specific environment]:environment:' \
                        '(-s --server)'{-s,--server}'[Target specific server]:server:' \
                        '(--service)--service[Target specific service]:service:' \
                        '(--config)--config[Use custom config file]:file:_files' \
                        '(-v --verbose)'{-v,--verbose}'[Show detailed output]' \
                        '1:deployment ID:'
                    ;;
                history)
                    _arguments \
                        '(-e --env)'{-e,--env}'[Target specific environment]:environment:' \
                        '(-s --server)'{-s,--server}'[Target specific server]:server:' \
                        '(--config)--config[Use custom config file]:file:_files' \
                        '(-v --verbose)'{-v,--verbose}'[Show detailed output]' \
                        '(-n --limit)'{-n,--limit}'[Number of deployments to show]:limit:' \
                        '(--status)--status[Filter by status (success, failed, rolled_back)]:status:(success failed rolled_back)'
                    ;;
                metrics)
                    _arguments \
                        '(-e --env)'{-e,--env}'[Target specific environment]:environment:' \
                        '(-s --server)'{-s,--server}'[Target specific server]:server:' \
                        '(--config)--config[Use custom config file]:file:_files' \
                        '(-v --verbose)'{-v,--verbose}'[Show detailed output]' \
                        '(--once)--once[Run once and exit]' \
                        '(-i --interval)'{-i,--interval}'[Refresh interval]:interval:'
                    ;;
                monitor)
                    _arguments \
                        '(-e --env)'{-e,--env}'[Target specific environment]:environment:' \
                        '(-s --server)'{-s,--server}'[Target specific server]:server:' \
                        '(--service)--service[Monitor specific service]:service:' \
                        '(--config)--config[Use custom config file]:file:_files' \
                        '(-v --verbose)'{-v,--verbose}'[Show detailed output]' \
                        '(--once)--once[Run single check and exit]'
                    ;;
                scale)
                    _arguments \
                        '(-e --env)'{-e,--env}'[Target specific environment]:environment:' \
                        '(-s --server)'{-s,--server}'[Target specific server]:server:' \
                        '(--config)--config[Use custom config file]:file:_files' \
                        '(-v --verbose)'{-v,--verbose}'[Show detailed output]' \
                        '*:SERVICE=REPLICAS:'
                    ;;
                backup)
                    _arguments \
                        '(-e --env)'{-e,--env}'[Target specific environment]:environment:' \
                        '(-s --server)'{-s,--server}'[Target specific server]:server:' \
                        '(--config)--config[Use custom config file]:file:_files' \
                        '(-v --verbose)'{-v,--verbose}'[Show detailed output]' \
                        '(--volume)--volume[Volume to backup]:volume:' \
                        '(--list)--list[List available backups]' \
                        '(--restore)--restore[Restore from backup ID]:backup_id:' \
                        '(--cleanup)--cleanup[Delete backups older than N days]:days:'
                    ;;
                drift)
                    _arguments \
                        '(-e --env)'{-e,--env}'[Target specific environment]:environment:' \
                        '(-s --server)'{-s,--server}'[Target specific server]:server:' \
                        '(--config)--config[Use custom config file]:file:_files' \
                        '(-v --verbose)'{-v,--verbose}'[Show detailed output]' \
                        '(-w --watch)'{-w,--watch}'[Continuously monitor for drift]' \
                        '(--webhook)--webhook[Webhook URL for notifications]:url:'
                    ;;
                init)
                    _arguments \
                        '1:project name:'
                    ;;
                upgrade)
                    _arguments \
                        '(-c --check)'{-c,--check}'[Only check for updates, do not install]' \
                        '(-f --force)'{-f,--force}'[Force upgrade even if on latest]'
                    ;;
                exec)
                    _arguments \
                        '(-e --env)'{-e,--env}'[Target specific environment]:environment:' \
                        '(-s --server)'{-s,--server}'[Target specific server]:server:' \
                        '(--service)--service[Execute in specific service container]:service:' \
                        '(--config)--config[Use custom config file]:file:_files' \
                        '(-v --verbose)'{-v,--verbose}'[Show detailed output]' \
                        '*:command:'
                    ;;
                cleanup)
                    _arguments \
                        '(-e --env)'{-e,--env}'[Target specific environment]:environment:' \
                        '(-s --server)'{-s,--server}'[Target specific server]:server:' \
                        '(--config)--config[Use custom config file]:file:_files' \
                        '(-v --verbose)'{-v,--verbose}'[Show detailed output]' \
                        '(--all)--all[Clean all resources]' \
                        '(--images)--images[Clean old images]' \
                        '(--containers)--containers[Clean stopped containers]' \
                        '(--volumes)--volumes[Clean unused volumes]'
                    ;;
                secrets)
                    local -a secret_commands
                    secret_commands=(
                        'init:Initialize secrets storage for project'
                        'set:Set a secret value (KEY=value)'
                        'list:List all secrets (redacted)'
                        'delete:Delete a secret'
                        'validate:Validate all required secrets are set'
                    )
                    _describe -t commands 'secrets commands' secret_commands
                    ;;
            esac
            ;;
    esac
}

_tako "$@"
