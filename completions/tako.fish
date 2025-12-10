# fish completion for tako

# Main commands
complete -c tako -f -n '__fish_use_subcommand' -a 'access' -d 'Stream access logs from Traefik'
complete -c tako -f -n '__fish_use_subcommand' -a 'backup' -d 'Backup and restore Docker volumes'
complete -c tako -f -n '__fish_use_subcommand' -a 'cleanup' -d 'Clean up old Docker resources'
complete -c tako -f -n '__fish_use_subcommand' -a 'deploy' -d 'Deploy application to environment'
complete -c tako -f -n '__fish_use_subcommand' -a 'destroy' -d 'Remove all services from server'
complete -c tako -f -n '__fish_use_subcommand' -a 'dev' -d 'Run production environment locally'
complete -c tako -f -n '__fish_use_subcommand' -a 'downgrade' -d 'Downgrade from Docker Swarm'
complete -c tako -f -n '__fish_use_subcommand' -a 'drift' -d 'Detect configuration drift'
complete -c tako -f -n '__fish_use_subcommand' -a 'exec' -d 'Execute commands on remote server(s)'
complete -c tako -f -n '__fish_use_subcommand' -a 'history' -d 'View deployment history'
complete -c tako -f -n '__fish_use_subcommand' -a 'init' -d 'Initialize new project'
complete -c tako -f -n '__fish_use_subcommand' -a 'live' -d 'Live development mode'
complete -c tako -f -n '__fish_use_subcommand' -a 'logs' -d 'Stream container logs'
complete -c tako -f -n '__fish_use_subcommand' -a 'maintenance' -d 'Enable/disable maintenance mode'
complete -c tako -f -n '__fish_use_subcommand' -a 'metrics' -d 'View system metrics'
complete -c tako -f -n '__fish_use_subcommand' -a 'monitor' -d 'Monitor deployed services'
complete -c tako -f -n '__fish_use_subcommand' -a 'prometheus' -d 'Deploy Prometheus monitoring stack'
complete -c tako -f -n '__fish_use_subcommand' -a 'ps' -d 'List running services'
complete -c tako -f -n '__fish_use_subcommand' -a 'remove' -d 'Remove a service'
complete -c tako -f -n '__fish_use_subcommand' -a 'rollback' -d 'Rollback to previous deployment'
complete -c tako -f -n '__fish_use_subcommand' -a 'scale' -d 'Scale service replicas'
complete -c tako -f -n '__fish_use_subcommand' -a 'secrets' -d 'Manage secrets'
complete -c tako -f -n '__fish_use_subcommand' -a 'setup' -d 'Provision server'
complete -c tako -f -n '__fish_use_subcommand' -a 'start' -d 'Start stopped services'
complete -c tako -f -n '__fish_use_subcommand' -a 'stats' -d 'Show real-time container stats'
complete -c tako -f -n '__fish_use_subcommand' -a 'stop' -d 'Stop running services'
complete -c tako -f -n '__fish_use_subcommand' -a 'storage' -d 'Manage shared storage (NFS)'
complete -c tako -f -n '__fish_use_subcommand' -a 'upgrade' -d 'Upgrade Tako CLI'
complete -c tako -f -n '__fish_use_subcommand' -a 'version' -d 'Show version'

# Global flags
complete -c tako -s h -l help -d 'Show help'
complete -c tako -s v -l version -d 'Show version'

# Common flags for most commands
complete -c tako -n '__fish_seen_subcommand_from deploy setup destroy logs ps start stop rollback history metrics monitor backup drift remove stats access' -s e -l env -d 'Target environment' -r
complete -c tako -n '__fish_seen_subcommand_from deploy setup destroy logs ps start stop rollback history metrics monitor backup drift remove stats access' -s s -l server -d 'Target server' -r
complete -c tako -n '__fish_seen_subcommand_from deploy setup destroy logs ps start stop rollback history metrics monitor backup drift remove stats access' -l service -d 'Target service' -r
complete -c tako -n '__fish_seen_subcommand_from deploy setup destroy logs ps start stop rollback history metrics monitor backup drift remove stats access' -l config -d 'Config file' -r -F
complete -c tako -n '__fish_seen_subcommand_from deploy setup destroy logs ps start stop rollback history metrics monitor backup drift remove stats access' -l verbose -d 'Verbose output'

# Deploy command flags
complete -c tako -n '__fish_seen_subcommand_from deploy' -s y -l yes -d 'Skip confirmation prompts'
complete -c tako -n '__fish_seen_subcommand_from deploy' -s m -l message -d 'Deployment message' -r
complete -c tako -n '__fish_seen_subcommand_from deploy' -l force -d 'Force deploy'
complete -c tako -n '__fish_seen_subcommand_from deploy' -l no-cache -d 'Build without cache'

# Init command flags
complete -c tako -n '__fish_seen_subcommand_from init' -s t -l template -d 'Template' -r

# Upgrade command flags
complete -c tako -n '__fish_seen_subcommand_from upgrade' -s c -l check -d 'Check only'
complete -c tako -n '__fish_seen_subcommand_from upgrade' -s f -l force -d 'Force upgrade'

# Backup command flags
complete -c tako -n '__fish_seen_subcommand_from backup' -l volume -d 'Volume to backup' -r
complete -c tako -n '__fish_seen_subcommand_from backup' -l restore -d 'Restore from backup file' -r -F
complete -c tako -n '__fish_seen_subcommand_from backup' -l list -d 'List available backups'
complete -c tako -n '__fish_seen_subcommand_from backup' -l cleanup -d 'Remove old backups'
complete -c tako -n '__fish_seen_subcommand_from backup' -l keep -d 'Number of backups to keep' -r

# Drift command flags
complete -c tako -n '__fish_seen_subcommand_from drift' -l watch -d 'Watch for drift continuously'
complete -c tako -n '__fish_seen_subcommand_from drift' -l interval -d 'Watch interval in seconds' -r

# Scale command flags
complete -c tako -n '__fish_seen_subcommand_from scale' -l replicas -d 'Number of replicas' -r

# Logs command flags
complete -c tako -n '__fish_seen_subcommand_from logs' -s f -l follow -d 'Follow log output'
complete -c tako -n '__fish_seen_subcommand_from logs' -s n -l tail -d 'Number of lines' -r
complete -c tako -n '__fish_seen_subcommand_from logs' -l since -d 'Show logs since timestamp' -r

# History command flags
complete -c tako -n '__fish_seen_subcommand_from history' -s n -l limit -d 'Number of entries' -r

# Cleanup command flags
complete -c tako -n '__fish_seen_subcommand_from cleanup' -l all -d 'Remove all unused resources'
complete -c tako -n '__fish_seen_subcommand_from cleanup' -l images -d 'Remove unused images'
complete -c tako -n '__fish_seen_subcommand_from cleanup' -l volumes -d 'Remove unused volumes'

# Secrets subcommands
complete -c tako -n '__fish_seen_subcommand_from secrets' -f -a 'init' -d 'Initialize secrets'
complete -c tako -n '__fish_seen_subcommand_from secrets' -f -a 'set' -d 'Set a secret'
complete -c tako -n '__fish_seen_subcommand_from secrets' -f -a 'list' -d 'List secrets'
complete -c tako -n '__fish_seen_subcommand_from secrets' -f -a 'delete' -d 'Delete secret'
complete -c tako -n '__fish_seen_subcommand_from secrets' -f -a 'validate' -d 'Validate secrets'

# Storage subcommands
complete -c tako -n '__fish_seen_subcommand_from storage' -f -a 'status' -d 'Show NFS storage status'
complete -c tako -n '__fish_seen_subcommand_from storage' -f -a 'remount' -d 'Remount NFS exports'

# Exec command flags
complete -c tako -n '__fish_seen_subcommand_from exec' -s e -l env -d 'Target environment' -r
complete -c tako -n '__fish_seen_subcommand_from exec' -s s -l server -d 'Target server' -r

# Maintenance command flags
complete -c tako -n '__fish_seen_subcommand_from maintenance' -l on -d 'Enable maintenance mode'
complete -c tako -n '__fish_seen_subcommand_from maintenance' -l off -d 'Disable maintenance mode'

# Remove command flags
complete -c tako -n '__fish_seen_subcommand_from remove' -l force -d 'Force removal without confirmation'

# Prometheus command flags
complete -c tako -n '__fish_seen_subcommand_from prometheus' -l remove -d 'Remove Prometheus stack'

# Stats command flags
complete -c tako -n '__fish_seen_subcommand_from stats' -l no-stream -d 'Show single snapshot'

# Access command flags
complete -c tako -n '__fish_seen_subcommand_from access' -s f -l follow -d 'Follow log output'
complete -c tako -n '__fish_seen_subcommand_from access' -s n -l tail -d 'Number of lines' -r
complete -c tako -n '__fish_seen_subcommand_from access' -l format -d 'Output format (json, text)' -r
