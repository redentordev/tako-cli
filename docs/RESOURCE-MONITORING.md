# Resource Monitoring Guide

Tako CLI provides comprehensive resource monitoring capabilities for both system-level and container-level metrics.

## Commands Overview

### 1. `tako metrics` - System Metrics

View system-level resource usage (CPU, RAM, Disk, Network, Disk I/O).

**Basic Usage:**
```bash
# View current metrics
tako metrics --once

# Continuous live updates
tako metrics --live

# Monitor specific server
tako metrics --server prod-1
```

**Output Example:**
```
=== System Metrics ===
Timestamp: 2024-01-15 10:30:45

📊 prod (95.216.194.236)
─────────────────────────────────────────────────
CPU:    67.20% [█████████████░░░░░░░]
Memory: 71.70% (2739 MB / 3820 MB) [██████████████░░░░░░]
Disk:   53% (19295 MB / 38123 MB) [██████████░░░░░░░░░░]
Net I/O: ↓ 15.23 MB  ↑ 8.45 MB (since last check)
Disk I/O: ⬇ 120.5 MB  ⬆ 45.2 MB (since last check)
Load:   3.06 (1m) / 2.89 (5m) / 3.34 (15m)
Uptime: 20h 8m
Updated: 2024-01-15T10:30:45Z
```

**Metrics Collected:**
- **CPU Usage**: Percentage of CPU utilization
- **Memory**: Total, used, available memory + swap
- **Disk**: Total, used, available disk space
- **Network I/O**: Bytes received/transmitted (delta since last check)
- **Disk I/O**: Bytes read/written (delta since last check)
- **Load Average**: 1, 5, and 15-minute load averages
- **Uptime**: System uptime in human-readable format

---

### 2. `tako stats` - Container Statistics

View per-container resource usage using Docker stats.

**Basic Usage:**
```bash
# View container stats (current project only)
tako stats

# Continuous live updates
tako stats --live

# Filter by service
tako stats --service web

# Show all containers (not just current project)
tako stats --all
```

**Output Example:**
```
=== Container Statistics ===
Timestamp: 2024-01-15 10:35:22

📊 prod (95.216.194.236)
─────────────────────────────────────────────────────────────────────────────────────
CONTAINER                                   CPU %        MEMORY     MEM %           NET I/O      BLOCK I/O
─────────────────────────────────────────────────────────────────────────────────────
nextjs-todos_prod_web.1.abc123             2.45%    256MiB / 2GiB    12.5%    1.2MB / 850kB   10MB / 2MB
nextjs-todos_prod_postgres.1.def456        0.89%    128MiB / 2GiB     6.3%     450kB / 320kB    5MB / 8MB
```

**Metrics Per Container:**
- **CPU %**: CPU usage percentage
- **Memory**: Used / Limit
- **Mem %**: Memory usage percentage
- **Net I/O**: Network bytes received / transmitted
- **Block I/O**: Disk bytes read / written

---

### 3. `tako prometheus` - Prometheus Export

Export metrics in Prometheus exposition format for scraping.

**Basic Usage:**
```bash
# Export metrics
tako prometheus
```

**Output Example:**
```
# HELP tako_cpu_usage_percent CPU usage percentage
# TYPE tako_cpu_usage_percent gauge
tako_cpu_usage_percent{server="prod",host="95.216.194.236"} 67.20

# HELP tako_memory_total_bytes Total memory in bytes
# TYPE tako_memory_total_bytes gauge
tako_memory_total_bytes{server="prod",host="95.216.194.236"} 4006387712

# HELP tako_container_cpu_usage_percent Container CPU usage percentage
# TYPE tako_container_cpu_usage_percent gauge
tako_container_cpu_usage_percent{server="prod",host="95.216.194.236",container="nextjs-todos_prod_web.1.abc"} 2.45
```

**Metrics Exported:**

**System Metrics:**
- `tako_cpu_usage_percent`
- `tako_memory_total_bytes`
- `tako_memory_used_bytes`
- `tako_memory_usage_percent`
- `tako_swap_total_bytes`
- `tako_swap_used_bytes`
- `tako_disk_total_bytes`
- `tako_disk_used_bytes`
- `tako_disk_usage_percent`
- `tako_network_receive_bytes` (counter)
- `tako_network_transmit_bytes` (counter)
- `tako_disk_read_bytes` (counter)
- `tako_disk_write_bytes` (counter)
- `tako_load_average_1m`
- `tako_load_average_5m`
- `tako_load_average_15m`
- `tako_uptime_seconds` (counter)

**Container Metrics:**
- `tako_container_cpu_usage_percent`
- `tako_container_memory_used_bytes`
- `tako_container_memory_limit_bytes`
- `tako_container_memory_usage_percent`

---

## Integration with Prometheus

### Setup Prometheus Scraping

1. **Create a cron job or systemd timer** to periodically export metrics:

```bash
# Export metrics every minute and save to file
*/1 * * * * /usr/local/bin/tako prometheus > /var/lib/tako/metrics/prometheus.txt
```

2. **Configure Prometheus** to scrape the file:

```yaml
scrape_configs:
  - job_name: 'tako'
    scrape_interval: 60s
    static_configs:
      - targets: ['localhost:9090']
    file_sd_configs:
      - files:
        - '/var/lib/tako/metrics/prometheus.txt'
```

3. **Or expose via HTTP endpoint** (requires additional setup):

```bash
# Simple HTTP server to serve metrics
while true; do
  tako prometheus > /tmp/metrics.txt
  nc -l -p 9100 < /tmp/metrics.txt
done
```

---

## Monitoring Agent

The monitoring agent runs on each server and collects metrics every 60 seconds.

### Agent Installation

The agent is automatically installed when you run:
```bash
tako setup
```

### Manual Agent Management

```bash
# Start agent
sudo systemctl start tako-monitor

# Stop agent
sudo systemctl stop tako-monitor

# Check status
sudo systemctl status tako-monitor

# View agent logs
sudo journalctl -u tako-monitor -f
```

### Agent Configuration

Agent settings are in `/usr/local/bin/tako-monitor.sh`:

```bash
# Collection interval (default: 60 seconds)
MONITOR_INTERVAL=60
```

To change the interval:
```bash
# Edit the service file
sudo systemctl edit tako-monitor

# Add:
[Service]
Environment="MONITOR_INTERVAL=30"

# Restart
sudo systemctl restart tako-monitor
```

---

## Use Cases

### 1. Real-time Monitoring Dashboard

```bash
# Live system metrics
tako metrics --live

# Live container stats
tako stats --live
```

### 2. Debugging Performance Issues

```bash
# Check which containers are using most resources
tako stats

# Check system-level bottlenecks
tako metrics --once
```

### 3. Capacity Planning

```bash
# Export metrics for analysis
tako prometheus > metrics-$(date +%Y%m%d-%H%M%S).txt

# Or continuous collection
while true; do
  tako prometheus >> metrics-history.txt
  sleep 300  # Every 5 minutes
done
```

### 4. Alerting

Create alerts based on thresholds:

```bash
#!/bin/bash
# Check if CPU > 90%
CPU=$(tako metrics --once | grep "CPU:" | awk '{print $2}' | sed 's/%//')
if (( $(echo "$CPU > 90" | bc -l) )); then
  echo "ALERT: High CPU usage: $CPU%"
  # Send notification (email, Slack, etc.)
fi
```

---

## Comparison with Other Tools

| Feature | tako metrics | tako stats | docker stats | prometheus |
|---------|-------------|-----------|--------------|------------|
| System CPU/RAM/Disk | ✅ | ❌ | ❌ | ✅ |
| Network I/O | ✅ | ❌ | ✅ | ✅ |
| Disk I/O | ✅ | ✅ | ✅ | ✅ |
| Container metrics | ❌ | ✅ | ✅ | ✅ |
| Historical data | ❌ | ❌ | ❌ | ✅ |
| Live updates | ✅ | ✅ | ✅ | ✅ |
| Prometheus format | ❌ | ❌ | ❌ | ✅ |
| Multi-server | ✅ | ✅ | ❌ | ✅ |

---

## Performance Impact

The monitoring agent has minimal performance impact:

- **CPU**: < 0.1% average
- **Memory**: ~ 5-10 MB
- **Disk I/O**: Minimal (only writes once per interval)
- **Network**: None (metrics stored locally)

---

## Troubleshooting

### Metrics not available

```bash
# Check if agent is running
sudo systemctl status tako-monitor

# Check metrics file
cat /var/lib/tako/metrics/current.json

# Manually collect metrics
/usr/local/bin/tako-monitor.sh once
```

### Container stats empty

```bash
# Verify containers are running
docker ps

# Check if project name matches
tako ps
```

### Prometheus format issues

```bash
# Validate output
tako prometheus | promtool check metrics
```

---

## Future Enhancements

Planned features:
- [ ] Built-in HTTP server for Prometheus scraping
- [ ] Metrics aggregation across multiple servers
- [ ] Alerting rules and notifications
- [ ] Metrics retention and historical queries
- [ ] Grafana dashboard templates
- [ ] Custom metric collection plugins
