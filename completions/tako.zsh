#compdef tako

# zsh completion for tako

_tako() {
    local -a commands

    _arguments -C \
        '(-h --help)'{-h,--help}'[Show help information]' \
        '(-v --version)'{-v,--version}'[Show version information]' \
        '1: :->cmds' \
        '*:: :->args'

    case $state in
        cmds)
            commands=(
                'access:Stream access logs from Traefik'
                'cleanup:Clean up old Docker resources'
                'deploy:Deploy application to environment'
                'destroy:Remove all services from server'
                'dev:Run production environment locally'
                'downgrade:Downgrade from Docker Swarm to single-server mode'
                'exec:Execute commands on remote server(s)'
                'history:View deployment history'
                'init:Initialize new project with template config'
                'live:Live development mode with hot reload'
                'logs:Stream container logs'
                'metrics:View system metrics from servers'
                'monitor:Continuously monitor deployed services'
                'ps:List running services and their status'
                'rollback:Rollback to previous/specific deployment'
                'scale:Scale service replicas'
                'secrets:Manage secrets for your project'
                'setup:Provision server (Docker, Traefik, security)'
                'start:Start stopped services'
                'stop:Stop running services'
                'upgrade:Upgrade Tako CLI to the latest version'
                'version:Show version information'
            )
            _describe -t commands 'tako commands' commands
            ;;
        args)
            case $words[1] in
                deploy|setup|destroy|logs|ps|start|stop|rollback|history|metrics|monitor|secrets)
                    _arguments \
                        '(-e --env)'{-e,--env}'[Target specific environment]:environment:' \
                        '(-s --server)'{-s,--server}'[Target specific server]:server:' \
                        '(--service)--service[Target specific service]:service:' \
                        '(--config)--config[Use custom config file]:file:_files' \
                        '(-v --verbose)'{-v,--verbose}'[Show detailed output]'
                    ;;
                init)
                    _arguments \
                        '1:project name:' \
                        '(-t --template)'{-t,--template}'[Use specific template]:template:'
                    ;;
                upgrade)
                    _arguments \
                        '(-c --check)'{-c,--check}'[Only check for updates]' \
                        '(-f --force)'{-f,--force}'[Force upgrade]'
                    ;;
                exec)
                    _arguments \
                        '(-e --env)'{-e,--env}'[Target specific environment]:environment:' \
                        '(-s --server)'{-s,--server}'[Target specific server]:server:' \
                        '1:command:'
                    ;;
                secrets)
                    local -a secret_commands
                    secret_commands=(
                        'init:Initialize secrets storage'
                        'set:Set a secret value'
                        'list:List all secrets'
                        'delete:Delete a secret'
                        'validate:Validate required secrets'
                    )
                    _describe -t commands 'secrets commands' secret_commands
                    ;;
            esac
            ;;
    esac
}

_tako "$@"
