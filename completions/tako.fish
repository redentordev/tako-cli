# fish completion for tako

# Main commands
complete -c tako -f -n '__fish_use_subcommand' -a 'access' -d 'Stream access logs from Traefik'
complete -c tako -f -n '__fish_use_subcommand' -a 'cleanup' -d 'Clean up old Docker resources'
complete -c tako -f -n '__fish_use_subcommand' -a 'deploy' -d 'Deploy application to environment'
complete -c tako -f -n '__fish_use_subcommand' -a 'destroy' -d 'Remove all services from server'
complete -c tako -f -n '__fish_use_subcommand' -a 'dev' -d 'Run production environment locally'
complete -c tako -f -n '__fish_use_subcommand' -a 'downgrade' -d 'Downgrade from Docker Swarm'
complete -c tako -f -n '__fish_use_subcommand' -a 'exec' -d 'Execute commands on remote server(s)'
complete -c tako -f -n '__fish_use_subcommand' -a 'history' -d 'View deployment history'
complete -c tako -f -n '__fish_use_subcommand' -a 'init' -d 'Initialize new project'
complete -c tako -f -n '__fish_use_subcommand' -a 'live' -d 'Live development mode'
complete -c tako -f -n '__fish_use_subcommand' -a 'logs' -d 'Stream container logs'
complete -c tako -f -n '__fish_use_subcommand' -a 'metrics' -d 'View system metrics'
complete -c tako -f -n '__fish_use_subcommand' -a 'monitor' -d 'Monitor deployed services'
complete -c tako -f -n '__fish_use_subcommand' -a 'ps' -d 'List running services'
complete -c tako -f -n '__fish_use_subcommand' -a 'rollback' -d 'Rollback to previous deployment'
complete -c tako -f -n '__fish_use_subcommand' -a 'scale' -d 'Scale service replicas'
complete -c tako -f -n '__fish_use_subcommand' -a 'secrets' -d 'Manage secrets'
complete -c tako -f -n '__fish_use_subcommand' -a 'setup' -d 'Provision server'
complete -c tako -f -n '__fish_use_subcommand' -a 'start' -d 'Start stopped services'
complete -c tako -f -n '__fish_use_subcommand' -a 'stop' -d 'Stop running services'
complete -c tako -f -n '__fish_use_subcommand' -a 'upgrade' -d 'Upgrade Tako CLI'
complete -c tako -f -n '__fish_use_subcommand' -a 'version' -d 'Show version'

# Global flags
complete -c tako -s h -l help -d 'Show help'
complete -c tako -s v -l version -d 'Show version'

# Common flags for most commands
complete -c tako -n '__fish_seen_subcommand_from deploy setup destroy logs ps start stop rollback history metrics monitor' -s e -l env -d 'Target environment' -r
complete -c tako -n '__fish_seen_subcommand_from deploy setup destroy logs ps start stop rollback history metrics monitor' -s s -l server -d 'Target server' -r
complete -c tako -n '__fish_seen_subcommand_from deploy setup destroy logs ps start stop rollback history metrics monitor' -l service -d 'Target service' -r
complete -c tako -n '__fish_seen_subcommand_from deploy setup destroy logs ps start stop rollback history metrics monitor' -l config -d 'Config file' -r -F
complete -c tako -n '__fish_seen_subcommand_from deploy setup destroy logs ps start stop rollback history metrics monitor' -l verbose -d 'Verbose output'

# Init command flags
complete -c tako -n '__fish_seen_subcommand_from init' -s t -l template -d 'Template' -r

# Upgrade command flags
complete -c tako -n '__fish_seen_subcommand_from upgrade' -s c -l check -d 'Check only'
complete -c tako -n '__fish_seen_subcommand_from upgrade' -s f -l force -d 'Force upgrade'

# Secrets subcommands
complete -c tako -n '__fish_seen_subcommand_from secrets' -f -a 'init' -d 'Initialize secrets'
complete -c tako -n '__fish_seen_subcommand_from secrets' -f -a 'set' -d 'Set a secret'
complete -c tako -n '__fish_seen_subcommand_from secrets' -f -a 'list' -d 'List secrets'
complete -c tako -n '__fish_seen_subcommand_from secrets' -f -a 'delete' -d 'Delete secret'
complete -c tako -n '__fish_seen_subcommand_from secrets' -f -a 'validate' -d 'Validate secrets'

# Exec command flags
complete -c tako -n '__fish_seen_subcommand_from exec' -s e -l env -d 'Target environment' -r
complete -c tako -n '__fish_seen_subcommand_from exec' -s s -l server -d 'Target server' -r
