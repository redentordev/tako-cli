#!/bin/bash
# Tako CLI Monitoring Agent
# Ultra-lightweight system metrics collection
# Collects: CPU, RAM, Disk usage
# Output: JSON format for easy parsing

set -euo pipefail

# Configuration
INTERVAL=${MONITOR_INTERVAL:-60}  # Collection interval in seconds
STATE_DIR="/var/lib/tako/metrics"
METRICS_FILE="$STATE_DIR/current.json"

# Ensure state directory exists
mkdir -p "$STATE_DIR"

# Get CPU usage (percentage)
get_cpu_usage() {
    # Read /proc/stat for CPU times
    # Format: cpu user nice system idle iowait irq softirq steal guest guest_nice
    local cpu_line=$(grep '^cpu ' /proc/stat)
    local cpu_times=($cpu_line)

    # Calculate total time
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

    # Store current values for delta calculation
    local prev_file="$STATE_DIR/cpu_prev"
    if [ -f "$prev_file" ]; then
        local prev_values=$(cat "$prev_file")
        local prev_total=$(echo "$prev_values" | cut -d' ' -f1)
        local prev_busy=$(echo "$prev_values" | cut -d' ' -f2)

        local total_delta=$((total - prev_total))
        local busy_delta=$((busy - prev_busy))

        if [ $total_delta -gt 0 ]; then
            # Calculate percentage (multiply by 100 for precision)
            local cpu_pct=$((busy_delta * 10000 / total_delta))
            # Convert to float with 2 decimals
            echo "scale=2; $cpu_pct / 100" | bc
        else
            echo "0.00"
        fi
    else
        echo "0.00"
    fi

    # Save current values
    echo "$total $busy" > "$prev_file"
}

# Get memory usage (MB and percentage)
get_memory_usage() {
    # Read /proc/meminfo
    local mem_total=$(grep '^MemTotal:' /proc/meminfo | awk '{print $2}')
    local mem_free=$(grep '^MemFree:' /proc/meminfo | awk '{print $2}')
    local mem_available=$(grep '^MemAvailable:' /proc/meminfo | awk '{print $2}')
    local mem_buffers=$(grep '^Buffers:' /proc/meminfo | awk '{print $2}')
    local mem_cached=$(grep '^Cached:' /proc/meminfo | awk '{print $2}')
    local swap_total=$(grep '^SwapTotal:' /proc/meminfo | awk '{print $2}')
    local swap_free=$(grep '^SwapFree:' /proc/meminfo | awk '{print $2}')

    # Calculate used memory (in KB)
    local mem_used=$((mem_total - mem_available))

    # Convert to MB
    local mem_total_mb=$((mem_total / 1024))
    local mem_used_mb=$((mem_used / 1024))
    local mem_available_mb=$((mem_available / 1024))
    local swap_total_mb=$((swap_total / 1024))
    local swap_used_mb=$(((swap_total - swap_free) / 1024))

    # Calculate percentage
    local mem_pct=$(echo "scale=2; $mem_used * 100 / $mem_total" | bc)

    echo "{\"total_mb\":$mem_total_mb,\"used_mb\":$mem_used_mb,\"available_mb\":$mem_available_mb,\"percent\":\"$mem_pct\",\"swap_total_mb\":$swap_total_mb,\"swap_used_mb\":$swap_used_mb}"
}

# Get disk usage for root filesystem
get_disk_usage() {
    # Get root filesystem usage
    local disk_info=$(df -BM / | tail -1)
    local total=$(echo "$disk_info" | awk '{print $2}' | sed 's/M//')
    local used=$(echo "$disk_info" | awk '{print $3}' | sed 's/M//')
    local available=$(echo "$disk_info" | awk '{print $4}' | sed 's/M//')
    local percent=$(echo "$disk_info" | awk '{print $5}' | sed 's/%//')

    echo "{\"total_mb\":$total,\"used_mb\":$used,\"available_mb\":$available,\"percent\":\"$percent\"}"
}

# Get network usage (bytes sent/received)
get_network_usage() {
    local total_rx=0
    local total_tx=0

    # Sum all network interfaces except loopback
    while IFS= read -r line; do
        if [[ ! "$line" =~ ^lo: ]] && [[ "$line" =~ ^[[:space:]]*[^:]+: ]]; then
            local iface=$(echo "$line" | awk '{print $1}' | sed 's/://')
            local rx=$(echo "$line" | awk '{print $2}')
            local tx=$(echo "$line" | awk '{print $10}')
            total_rx=$((total_rx + rx))
            total_tx=$((total_tx + tx))
        fi
    done < <(tail -n +3 /proc/net/dev)

    # Calculate delta if previous values exist
    local prev_file="$STATE_DIR/net_prev"
    if [ -f "$prev_file" ]; then
        local prev_values=$(cat "$prev_file")
        local prev_rx=$(echo "$prev_values" | cut -d' ' -f1)
        local prev_tx=$(echo "$prev_values" | cut -d' ' -f2)

        local rx_delta=$((total_rx - prev_rx))
        local tx_delta=$((total_tx - prev_tx))

        # Convert to MB with proper decimal format
        local rx_mb=$(printf "%.2f" $(echo "scale=2; $rx_delta / 1048576" | bc))
        local tx_mb=$(printf "%.2f" $(echo "scale=2; $tx_delta / 1048576" | bc))

        echo "{\"rx_mb\":\"$rx_mb\",\"tx_mb\":\"$tx_mb\",\"rx_bytes\":$rx_delta,\"tx_bytes\":$tx_delta}"
    else
        echo "{\"rx_mb\":\"0.00\",\"tx_mb\":\"0.00\",\"rx_bytes\":0,\"tx_bytes\":0}"
    fi

    # Save current values
    echo "$total_rx $total_tx" > "$prev_file"
}

# Get disk I/O statistics
get_disk_io() {
    local total_read=0
    local total_write=0

    # Read disk stats from /proc/diskstats
    # Format: major minor name reads ... sectors_read ... writes ... sectors_written ...
    while IFS= read -r line; do
        # Only process physical disks (sd*, nvme*, vd*, hd*)
        if [[ "$line" =~ [[:space:]]+(sd[a-z]+|nvme[0-9]+n[0-9]+|vd[a-z]+|hd[a-z]+)[[:space:]] ]]; then
            local fields=($line)
            local sectors_read=${fields[5]:-0}
            local sectors_written=${fields[9]:-0}
            total_read=$((total_read + sectors_read))
            total_write=$((total_write + sectors_written))
        fi
    done < /proc/diskstats

    # Calculate delta if previous values exist
    # Note: sectors are typically 512 bytes
    local prev_file="$STATE_DIR/disk_io_prev"
    if [ -f "$prev_file" ]; then
        local prev_values=$(cat "$prev_file")
        local prev_read=$(echo "$prev_values" | cut -d' ' -f1)
        local prev_write=$(echo "$prev_values" | cut -d' ' -f2)

        local read_delta=$((total_read - prev_read))
        local write_delta=$((total_write - prev_write))

        # Convert sectors to MB (512 bytes per sector)
        # Use printf to ensure proper decimal format (0.00 instead of .00)
        local read_mb=$(printf "%.2f" $(echo "scale=2; $read_delta * 512 / 1048576" | bc))
        local write_mb=$(printf "%.2f" $(echo "scale=2; $write_delta * 512 / 1048576" | bc))

        echo "{\"read_mb\":\"$read_mb\",\"write_mb\":\"$write_mb\",\"read_sectors\":$read_delta,\"write_sectors\":$write_delta}"
    else
        echo "{\"read_mb\":\"0.00\",\"write_mb\":\"0.00\",\"read_sectors\":0,\"write_sectors\":0}"
    fi

    # Save current values
    echo "$total_read $total_write" > "$prev_file"
}

# Get system uptime
get_uptime() {
    local uptime_sec=$(cat /proc/uptime | awk '{print int($1)}')
    echo "$uptime_sec"
}

# Get load average
get_load_average() {
    local loadavg=$(cat /proc/loadavg)
    local load1=$(echo "$loadavg" | awk '{print $1}')
    local load5=$(echo "$loadavg" | awk '{print $2}')
    local load15=$(echo "$loadavg" | awk '{print $3}')

    echo "{\"1min\":\"$load1\",\"5min\":\"$load5\",\"15min\":\"$load15\"}"
}

# Collect all metrics and output JSON
collect_metrics() {
    local timestamp=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    local cpu=$(get_cpu_usage)
    local memory=$(get_memory_usage)
    local disk=$(get_disk_usage)
    local network=$(get_network_usage)
    local disk_io=$(get_disk_io)
    local uptime=$(get_uptime)
    local load=$(get_load_average)

    cat > "$METRICS_FILE" <<EOF
{
  "timestamp": "$timestamp",
  "cpu_percent": "$cpu",
  "memory": $memory,
  "disk": $disk,
  "network": $network,
  "disk_io": $disk_io,
  "uptime_seconds": $uptime,
  "load_average": $load
}
EOF

    # Also output to stdout if requested
    if [ "${OUTPUT_STDOUT:-0}" = "1" ]; then
        cat "$METRICS_FILE"
    fi
}

# Main loop - collect metrics at interval
main() {
    echo "Tako CLI Monitoring Agent started (interval: ${INTERVAL}s)"
    echo "Metrics saved to: $METRICS_FILE"

    while true; do
        collect_metrics
        sleep "$INTERVAL"
    done
}

# Handle signals for graceful shutdown
trap 'echo "Shutting down monitoring agent..."; exit 0' SIGTERM SIGINT

# If called with "once" argument, collect once and exit
if [ "${1:-}" = "once" ]; then
    collect_metrics
    exit 0
fi

# Run main loop
main
