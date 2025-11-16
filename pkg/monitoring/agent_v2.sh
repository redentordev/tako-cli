#!/bin/bash
# Tako CLI Enhanced Monitoring Agent v2
# Secure, real-time system metrics collection with health checks
# Features: CPU, RAM, Disk, Network, Docker stats, Health checks, Event streaming

set -euo pipefail

# Configuration
VERSION="2.0.0"
INTERVAL=${MONITOR_INTERVAL:-30}  # Collection interval in seconds
STATE_DIR="/var/lib/tako/metrics"
METRICS_FILE="$STATE_DIR/current.json"
EVENTS_FILE="$STATE_DIR/events.log"
HEALTH_FILE="$STATE_DIR/health.json"
PID_FILE="$STATE_DIR/agent.pid"
LOG_FILE="/var/log/tako/agent.log"

# Ensure directories exist
mkdir -p "$STATE_DIR"
mkdir -p "$(dirname "$LOG_FILE")"

# Logging function
log() {
    local level="$1"
    shift
    local message="$*"
    local timestamp=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    echo "[$timestamp] [$level] $message" | tee -a "$LOG_FILE"
}

# PID file management
create_pid_file() {
    echo $$ > "$PID_FILE"
}

remove_pid_file() {
    rm -f "$PID_FILE"
}

check_running() {
    if [ -f "$PID_FILE" ]; then
        local pid=$(cat "$PID_FILE")
        if kill -0 "$pid" 2>/dev/null; then
            return 0
        fi
    fi
    return 1
}

# Enhanced CPU usage with per-core metrics
get_cpu_usage() {
    local cpu_line=$(grep '^cpu ' /proc/stat)
    local cpu_times=($cpu_line)
    
    local user=${cpu_times[1]}
    local nice=${cpu_times[2]}
    local system=${cpu_times[3]}
    local idle=${cpu_times[4]}
    local iowait=${cpu_times[5]}
    local irq=${cpu_times[6]}
    local softirq=${cpu_times[7]}
    local steal=${cpu_times[8]:-0}
    
    local total=$((user + nice + system + idle + iowait + irq + softirq + steal))
    local busy=$((total - idle - iowait))
    
    # Calculate CPU percentage
    local prev_file="$STATE_DIR/cpu_prev"
    if [ -f "$prev_file" ]; then
        local prev_values=$(cat "$prev_file")
        local prev_total=$(echo "$prev_values" | cut -d' ' -f1)
        local prev_busy=$(echo "$prev_values" | cut -d' ' -f2)
        
        local total_delta=$((total - prev_total))
        local busy_delta=$((busy - prev_busy))
        
        if [ $total_delta -gt 0 ]; then
            local cpu_pct=$((busy_delta * 10000 / total_delta))
            echo "scale=2; $cpu_pct / 100" | bc
        else
            echo "0.00"
        fi
    else
        echo "0.00"
    fi
    
    echo "$total $busy" > "$prev_file"
}

# Get CPU count
get_cpu_count() {
    nproc
}

# Enhanced memory metrics with detailed breakdown
get_memory_usage() {
    local mem_total=$(grep '^MemTotal:' /proc/meminfo | awk '{print $2}')
    local mem_free=$(grep '^MemFree:' /proc/meminfo | awk '{print $2}')
    local mem_available=$(grep '^MemAvailable:' /proc/meminfo | awk '{print $2}')
    local mem_buffers=$(grep '^Buffers:' /proc/meminfo | awk '{print $2}')
    local mem_cached=$(grep '^Cached:' /proc/meminfo | awk '{print $2}')
    local swap_total=$(grep '^SwapTotal:' /proc/meminfo | awk '{print $2}')
    local swap_free=$(grep '^SwapFree:' /proc/meminfo | awk '{print $2}')
    
    local mem_used=$((mem_total - mem_available))
    
    # Convert to MB
    local mem_total_mb=$((mem_total / 1024))
    local mem_used_mb=$((mem_used / 1024))
    local mem_available_mb=$((mem_available / 1024))
    local mem_buffers_mb=$((mem_buffers / 1024))
    local mem_cached_mb=$((mem_cached / 1024))
    local swap_total_mb=$((swap_total / 1024))
    local swap_used_mb=$(((swap_total - swap_free) / 1024))
    
    local mem_pct=$(echo "scale=2; $mem_used * 100 / $mem_total" | bc)
    local swap_pct="0.00"
    if [ $swap_total -gt 0 ]; then
        swap_pct=$(echo "scale=2; ($swap_total - $swap_free) * 100 / $swap_total" | bc)
    fi
    
    echo "{\"total_mb\":$mem_total_mb,\"used_mb\":$mem_used_mb,\"available_mb\":$mem_available_mb,\"buffers_mb\":$mem_buffers_mb,\"cached_mb\":$mem_cached_mb,\"percent\":\"$mem_pct\",\"swap_total_mb\":$swap_total_mb,\"swap_used_mb\":$swap_used_mb,\"swap_percent\":\"$swap_pct\"}"
}

# Disk usage for all mounted filesystems
get_disk_usage() {
    local disk_json="["
    local first=true
    
    while IFS= read -r line; do
        if [[ "$line" =~ ^/ ]]; then
            local filesystem=$(echo "$line" | awk '{print $1}')
            local total=$(echo "$line" | awk '{print $2}' | sed 's/M//')
            local used=$(echo "$line" | awk '{print $3}' | sed 's/M//')
            local available=$(echo "$line" | awk '{print $4}' | sed 's/M//')
            local percent=$(echo "$line" | awk '{print $5}' | sed 's/%//')
            local mountpoint=$(echo "$line" | awk '{print $6}')
            
            if [ "$first" = true ]; then
                first=false
            else
                disk_json+=","
            fi
            
            disk_json+="{\"filesystem\":\"$filesystem\",\"mountpoint\":\"$mountpoint\",\"total_mb\":$total,\"used_mb\":$used,\"available_mb\":$available,\"percent\":\"$percent\"}"
        fi
    done < <(df -BM | tail -n +2)
    
    disk_json+="]"
    echo "$disk_json"
}

# Network metrics with rate calculation
get_network_usage() {
    local total_rx=0
    local total_tx=0
    local interfaces=()
    
    while IFS= read -r line; do
        if [[ ! "$line" =~ ^lo: ]] && [[ "$line" =~ ^[[:space:]]*[^:]+: ]]; then
            local iface=$(echo "$line" | awk '{print $1}' | sed 's/://')
            local rx=$(echo "$line" | awk '{print $2}')
            local tx=$(echo "$line" | awk '{print $10}')
            total_rx=$((total_rx + rx))
            total_tx=$((total_tx + tx))
            interfaces+=("$iface")
        fi
    done < <(tail -n +3 /proc/net/dev)
    
    local prev_file="$STATE_DIR/net_prev"
    local prev_time_file="$STATE_DIR/net_prev_time"
    local current_time=$(date +%s)
    
    if [ -f "$prev_file" ] && [ -f "$prev_time_file" ]; then
        local prev_values=$(cat "$prev_file")
        local prev_rx=$(echo "$prev_values" | cut -d' ' -f1)
        local prev_tx=$(echo "$prev_values" | cut -d' ' -f2)
        local prev_time=$(cat "$prev_time_file")
        
        local time_delta=$((current_time - prev_time))
        if [ $time_delta -gt 0 ]; then
            local rx_delta=$((total_rx - prev_rx))
            local tx_delta=$((total_tx - prev_tx))
            
            # Calculate rates in KB/s
            local rx_rate=$(echo "scale=2; $rx_delta / $time_delta / 1024" | bc)
            local tx_rate=$(echo "scale=2; $tx_delta / $time_delta / 1024" | bc)
            
            # Convert total to MB
            local rx_mb=$(echo "scale=2; $rx_delta / 1048576" | bc)
            local tx_mb=$(echo "scale=2; $tx_delta / 1048576" | bc)
            
            echo "{\"rx_mb\":$rx_mb,\"tx_mb\":$tx_mb,\"rx_rate_kbps\":$rx_rate,\"tx_rate_kbps\":$tx_rate,\"rx_bytes\":$rx_delta,\"tx_bytes\":$tx_delta,\"interfaces\":[\"${interfaces[@]}\"]}"
        else
            echo "{\"rx_mb\":0,\"tx_mb\":0,\"rx_rate_kbps\":0,\"tx_rate_kbps\":0,\"rx_bytes\":0,\"tx_bytes\":0,\"interfaces\":[\"${interfaces[@]}\"]}"
        fi
    else
        echo "{\"rx_mb\":0,\"tx_mb\":0,\"rx_rate_kbps\":0,\"tx_rate_kbps\":0,\"rx_bytes\":0,\"tx_bytes\":0,\"interfaces\":[\"${interfaces[@]}\"]}"
    fi
    
    echo "$total_rx $total_tx" > "$prev_file"
    echo "$current_time" > "$prev_time_file"
}

# Docker container metrics
get_docker_metrics() {
    if ! command -v docker &> /dev/null; then
        echo "null"
        return
    fi
    
    local container_count=$(docker ps -q 2>/dev/null | wc -l)
    local running_count=$(docker ps -q --filter "status=running" 2>/dev/null | wc -l)
    local stopped_count=$(docker ps -aq --filter "status=exited" 2>/dev/null | wc -l)
    
    echo "{\"total\":$container_count,\"running\":$running_count,\"stopped\":$stopped_count}"
}

# System health checks
check_system_health() {
    local status="healthy"
    local issues=()
    
    # Check CPU usage
    local cpu=$(get_cpu_usage)
    if (( $(echo "$cpu > 90" | bc -l) )); then
        status="warning"
        issues+=("\"High CPU usage: ${cpu}%\"")
    fi
    
    # Check memory usage
    local mem_info=$(get_memory_usage)
    local mem_pct=$(echo "$mem_info" | jq -r '.percent')
    if (( $(echo "$mem_pct > 90" | bc -l) )); then
        status="critical"
        issues+=("\"High memory usage: ${mem_pct}%\"")
    fi
    
    # Check disk usage
    local disk_info=$(get_disk_usage)
    local root_pct=$(echo "$disk_info" | jq -r '.[0].percent')
    if (( $(echo "$root_pct > 90" | bc -l) )); then
        status="critical"
        issues+=("\"High disk usage: ${root_pct}%\"")
    fi
    
    # Check Docker daemon
    if ! docker info &>/dev/null; then
        status="warning"
        issues+=("\"Docker daemon not responding\"")
    fi
    
    local timestamp=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    local issues_json=$(IFS=,; echo "[${issues[*]}]")
    
    cat > "$HEALTH_FILE" <<EOF
{
  "timestamp": "$timestamp",
  "status": "$status",
  "issues": $issues_json
}
EOF
    
    echo "$status"
}

# Event logging for important changes
log_event() {
    local event_type="$1"
    local message="$2"
    local timestamp=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    
    echo "{\"timestamp\":\"$timestamp\",\"type\":\"$event_type\",\"message\":\"$message\"}" >> "$EVENTS_FILE"
    
    # Keep only last 1000 events
    tail -n 1000 "$EVENTS_FILE" > "$EVENTS_FILE.tmp" && mv "$EVENTS_FILE.tmp" "$EVENTS_FILE"
}

# Collect all metrics and output JSON
collect_metrics() {
    local timestamp=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    local hostname=$(hostname)
    local cpu=$(get_cpu_usage)
    local cpu_count=$(get_cpu_count)
    local memory=$(get_memory_usage)
    local disk=$(get_disk_usage)
    local network=$(get_network_usage)
    local uptime=$(cat /proc/uptime | awk '{print int($1)}')
    local load=$(cat /proc/loadavg)
    local load1=$(echo "$load" | awk '{print $1}')
    local load5=$(echo "$load" | awk '{print $2}')
    local load15=$(echo "$load" | awk '{print $3}')
    local docker=$(get_docker_metrics)
    local health=$(check_system_health)
    
    cat > "$METRICS_FILE" <<EOF
{
  "version": "$VERSION",
  "timestamp": "$timestamp",
  "hostname": "$hostname",
  "cpu": {
    "percent": "$cpu",
    "cores": $cpu_count
  },
  "memory": $memory,
  "disk": $disk,
  "network": $network,
  "uptime_seconds": $uptime,
  "load_average": {
    "1min": "$load1",
    "5min": "$load5",
    "15min": "$load15"
  },
  "docker": $docker,
  "health_status": "$health"
}
EOF
    
    # Output to stdout if requested
    if [ "${OUTPUT_STDOUT:-0}" = "1" ]; then
        cat "$METRICS_FILE"
    fi
}

# Start the monitoring agent
start_agent() {
    if check_running; then
        log "INFO" "Agent already running (PID: $(cat $PID_FILE))"
        exit 0
    fi
    
    create_pid_file
    log "INFO" "Tako CLI Monitoring Agent v$VERSION started (interval: ${INTERVAL}s)"
    log "INFO" "PID: $$"
    log "INFO" "Metrics file: $METRICS_FILE"
    log_event "agent_start" "Monitoring agent started"
    
    while true; do
        collect_metrics
        sleep "$INTERVAL"
    done
}

# Stop the monitoring agent
stop_agent() {
    if [ -f "$PID_FILE" ]; then
        local pid=$(cat "$PID_FILE")
        if kill -0 "$pid" 2>/dev/null; then
            log "INFO" "Stopping agent (PID: $pid)"
            kill -TERM "$pid"
            log_event "agent_stop" "Monitoring agent stopped"
        fi
        remove_pid_file
    else
        log "INFO" "Agent not running"
    fi
}

# Get agent status
status_agent() {
    if check_running; then
        local pid=$(cat "$PID_FILE")
        log "INFO" "Agent is running (PID: $pid)"
        if [ -f "$METRICS_FILE" ]; then
            log "INFO" "Last metrics collection: $(jq -r '.timestamp' < $METRICS_FILE)"
        fi
        exit 0
    else
        log "INFO" "Agent is not running"
        exit 1
    fi
}

# Handle signals for graceful shutdown
cleanup() {
    log "INFO" "Shutting down monitoring agent..."
    log_event "agent_shutdown" "Monitoring agent received shutdown signal"
    remove_pid_file
    exit 0
}

trap cleanup SIGTERM SIGINT

# Command line interface
case "${1:-start}" in
    start)
        start_agent
        ;;
    stop)
        stop_agent
        ;;
    restart)
        stop_agent
        sleep 2
        start_agent
        ;;
    status)
        status_agent
        ;;
    once)
        collect_metrics
        ;;
    health)
        check_system_health
        cat "$HEALTH_FILE"
        ;;
    version)
        echo "Tako CLI Monitoring Agent v$VERSION"
        ;;
    *)
        echo "Usage: $0 {start|stop|restart|status|once|health|version}"
        exit 1
        ;;
esac
