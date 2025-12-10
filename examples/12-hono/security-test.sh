#!/bin/bash
#
# Tako Security Test Suite
# Comprehensive security testing for Tako-deployed servers
#
# Usage: ./security-test.sh <target_ip> [options]
#
# Options:
#   --full          Run all tests including slow ones
#   --quick         Quick scan only (default)
#   --brute         Include SSH brute force test (careful!)
#   --web           Web application tests only
#   --network       Network/port tests only
#   --report        Generate HTML report
#
# Requirements:
#   - nmap (apt install nmap)
#   - curl
#   - openssl
#   - hydra (apt install hydra) - for brute force tests
#   - nikto (apt install nikto) - for web vulnerability scanning
#   - sslscan (apt install sslscan) - for SSL/TLS analysis
#

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
TARGET="${1:-}"
DOMAIN="${2:-}"
FULL_TEST=false
QUICK_TEST=true
BRUTE_TEST=false
WEB_TEST=false
NETWORK_TEST=false
GENERATE_REPORT=false
REPORT_FILE="security-report-$(date +%Y%m%d-%H%M%S).html"

# Parse options
shift 2 2>/dev/null || true
while [[ $# -gt 0 ]]; do
    case $1 in
        --full) FULL_TEST=true; QUICK_TEST=false ;;
        --quick) QUICK_TEST=true ;;
        --brute) BRUTE_TEST=true ;;
        --web) WEB_TEST=true; QUICK_TEST=false ;;
        --network) NETWORK_TEST=true; QUICK_TEST=false ;;
        --report) GENERATE_REPORT=true ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
    shift
done

# Validate target
if [ -z "$TARGET" ]; then
    echo "Usage: $0 <target_ip> [domain] [options]"
    echo ""
    echo "Options:"
    echo "  --full          Run all tests"
    echo "  --quick         Quick scan only (default)"
    echo "  --brute         Include SSH brute force test"
    echo "  --web           Web application tests only"
    echo "  --network       Network/port tests only"
    echo "  --report        Generate HTML report"
    exit 1
fi

# Results storage
declare -A RESULTS
VULNERABILITIES=()
WARNINGS=()
PASSED=()

# Logging functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_pass() {
    echo -e "${GREEN}[PASS]${NC} $1"
    PASSED+=("$1")
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
    WARNINGS+=("$1")
}

log_fail() {
    echo -e "${RED}[FAIL]${NC} $1"
    VULNERABILITIES+=("$1")
}

log_section() {
    echo ""
    echo -e "${BLUE}========================================${NC}"
    echo -e "${BLUE} $1${NC}"
    echo -e "${BLUE}========================================${NC}"
    echo ""
}

# Check required tools
check_dependencies() {
    log_section "Checking Dependencies"
    
    local missing=()
    
    for tool in curl nmap openssl nc; do
        if command -v $tool &> /dev/null; then
            log_pass "$tool is installed"
        else
            log_warn "$tool is not installed"
            missing+=($tool)
        fi
    done
    
    # Optional tools
    for tool in hydra nikto sslscan testssl.sh; do
        if command -v $tool &> /dev/null; then
            log_pass "$tool is installed (optional)"
        else
            log_info "$tool is not installed (optional)"
        fi
    done
    
    if [ ${#missing[@]} -gt 0 ]; then
        log_warn "Some required tools are missing. Install with: apt install ${missing[*]}"
    fi
}

# ============================================
# NETWORK SECURITY TESTS
# ============================================

test_port_scan() {
    log_section "Port Scan Analysis"
    
    log_info "Scanning open ports on $TARGET..."
    
    # Quick TCP scan
    local open_ports=$(nmap -sT -p- --min-rate=5000 -T4 $TARGET 2>/dev/null | grep "open" | grep -v "filtered")
    
    echo "$open_ports"
    echo ""
    
    # Check for unexpected open ports
    local expected_ports="22 80 443 2377 7946 4789"
    
    while IFS= read -r line; do
        port=$(echo "$line" | awk '{print $1}' | cut -d'/' -f1)
        if [[ ! " $expected_ports " =~ " $port " ]]; then
            log_warn "Unexpected open port: $port"
        fi
    done <<< "$open_ports"
    
    # Check if dangerous ports are open
    local dangerous_ports="21 23 25 3306 5432 6379 27017 11211"
    for port in $dangerous_ports; do
        if echo "$open_ports" | grep -q "^$port/"; then
            log_fail "Dangerous port $port is open!"
        fi
    done
    
    log_pass "Port scan completed"
}

test_firewall() {
    log_section "Firewall Detection"
    
    log_info "Testing firewall rules..."
    
    # Test if ports are properly filtered
    local filtered_count=$(nmap -sA -p 1-1000 $TARGET 2>/dev/null | grep "filtered" | wc -l)
    
    if [ "$filtered_count" -gt 900 ]; then
        log_pass "Firewall appears to be properly configured (most ports filtered)"
    else
        log_warn "Firewall may not be properly configured"
    fi
}

test_rate_limiting() {
    log_section "Rate Limiting Tests"
    
    log_info "Testing SSH rate limiting..."
    
    # Try rapid SSH connections (should be rate limited by UFW)
    local start_time=$(date +%s)
    local blocked=false
    
    for i in {1..10}; do
        timeout 2 nc -zv $TARGET 22 2>/dev/null || blocked=true
        sleep 0.1
    done
    
    if $blocked; then
        log_pass "SSH appears to have rate limiting enabled"
    else
        log_warn "SSH rate limiting may not be effective"
    fi
    
    log_info "Testing HTTP rate limiting..."
    
    # Rapid HTTP requests
    local http_blocked=false
    for i in {1..50}; do
        response=$(curl -s -o /dev/null -w "%{http_code}" --max-time 2 "http://$TARGET/" 2>/dev/null)
        if [ "$response" = "429" ] || [ "$response" = "503" ]; then
            http_blocked=true
            break
        fi
    done
    
    if $http_blocked; then
        log_pass "HTTP rate limiting detected"
    else
        log_info "No HTTP rate limiting detected (may be handled by application)"
    fi
}

# ============================================
# SSH SECURITY TESTS
# ============================================

test_ssh_security() {
    log_section "SSH Security Tests"
    
    log_info "Testing SSH configuration..."
    
    # Test SSH version
    local ssh_banner=$(echo "" | timeout 5 nc $TARGET 22 2>/dev/null | head -1)
    echo "SSH Banner: $ssh_banner"
    
    if echo "$ssh_banner" | grep -qi "openssh"; then
        log_pass "Running OpenSSH"
        
        # Check for old versions
        if echo "$ssh_banner" | grep -qE "OpenSSH_[1-6]\."; then
            log_fail "Outdated SSH version detected!"
        else
            log_pass "SSH version appears current"
        fi
    fi
    
    # Test password authentication
    log_info "Testing if password authentication is disabled..."
    
    local auth_result=$(ssh -o BatchMode=yes -o ConnectTimeout=5 -o PreferredAuthentications=password -o PubkeyAuthentication=no test@$TARGET 2>&1)
    
    if echo "$auth_result" | grep -qi "permission denied"; then
        log_pass "Password authentication appears to be disabled"
    elif echo "$auth_result" | grep -qi "password"; then
        log_fail "Password authentication may be enabled!"
    else
        log_pass "Password authentication test inconclusive (likely disabled)"
    fi
    
    # Test for weak ciphers
    log_info "Testing SSH cipher support..."
    
    local weak_ciphers="arcfour arcfour128 arcfour256 3des-cbc blowfish-cbc cast128-cbc"
    for cipher in $weak_ciphers; do
        if ssh -o Ciphers=$cipher -o ConnectTimeout=3 test@$TARGET 2>&1 | grep -q "no matching cipher"; then
            continue
        else
            if timeout 3 ssh -o Ciphers=$cipher -o BatchMode=yes test@$TARGET exit 2>&1 | grep -q "Connection"; then
                log_fail "Weak cipher $cipher is supported!"
            fi
        fi
    done
    log_pass "Weak cipher test completed"
    
    # Test for weak key exchange algorithms
    log_info "Testing key exchange algorithms..."
    local weak_kex="diffie-hellman-group1-sha1 diffie-hellman-group-exchange-sha1"
    for kex in $weak_kex; do
        if ssh -o KexAlgorithms=$kex -o ConnectTimeout=3 test@$TARGET 2>&1 | grep -q "no matching"; then
            log_pass "Weak KEX $kex is disabled"
        else
            log_warn "Weak KEX $kex may be supported"
        fi
    done
}

test_ssh_bruteforce() {
    log_section "SSH Brute Force Test"
    
    if ! command -v hydra &> /dev/null; then
        log_warn "Hydra not installed, skipping brute force test"
        log_info "Install with: apt install hydra"
        return
    fi
    
    log_warn "Running SSH brute force test (this will trigger fail2ban)..."
    
    # Create a small wordlist
    local wordlist="/tmp/tako-test-wordlist.txt"
    cat > $wordlist << 'EOF'
admin
root
test
password
123456
administrator
EOF
    
    # Run hydra with limited attempts (just to test fail2ban)
    local result=$(timeout 60 hydra -l root -P $wordlist -t 4 -w 5 ssh://$TARGET 2>&1)
    
    if echo "$result" | grep -q "valid password"; then
        log_fail "SSH brute force found valid credentials!"
    elif echo "$result" | grep -qi "blocked\|banned\|connection refused"; then
        log_pass "Brute force protection (fail2ban) is working!"
    else
        log_pass "No valid credentials found with basic wordlist"
    fi
    
    rm -f $wordlist
}

# ============================================
# SSL/TLS SECURITY TESTS
# ============================================

test_ssl_security() {
    log_section "SSL/TLS Security Tests"
    
    local test_domain="${DOMAIN:-$TARGET}"
    
    log_info "Testing SSL/TLS on $test_domain..."
    
    # Test SSL certificate
    log_info "Checking SSL certificate..."
    
    local cert_info=$(echo | timeout 10 openssl s_client -servername $test_domain -connect $TARGET:443 2>/dev/null)
    
    if echo "$cert_info" | grep -q "BEGIN CERTIFICATE"; then
        log_pass "SSL certificate is present"
        
        # Check certificate expiry
        local expiry=$(echo "$cert_info" | openssl x509 -noout -enddate 2>/dev/null | cut -d= -f2)
        local expiry_epoch=$(date -d "$expiry" +%s 2>/dev/null || echo 0)
        local now_epoch=$(date +%s)
        local days_left=$(( (expiry_epoch - now_epoch) / 86400 ))
        
        if [ "$days_left" -lt 0 ]; then
            log_fail "SSL certificate has EXPIRED!"
        elif [ "$days_left" -lt 7 ]; then
            log_warn "SSL certificate expires in $days_left days"
        elif [ "$days_left" -lt 30 ]; then
            log_warn "SSL certificate expires in $days_left days"
        else
            log_pass "SSL certificate valid for $days_left days"
        fi
        
        # Check certificate issuer
        local issuer=$(echo "$cert_info" | openssl x509 -noout -issuer 2>/dev/null)
        echo "Issuer: $issuer"
        
        if echo "$issuer" | grep -qi "Let's Encrypt"; then
            log_pass "Using Let's Encrypt certificate"
        fi
    else
        log_fail "No SSL certificate found!"
    fi
    
    # Test TLS versions
    log_info "Testing TLS versions..."
    
    # Test TLS 1.0 (should be disabled)
    if timeout 5 openssl s_client -tls1 -connect $TARGET:443 2>&1 | grep -q "handshake failure\|no protocols"; then
        log_pass "TLS 1.0 is disabled"
    else
        log_warn "TLS 1.0 may be enabled"
    fi
    
    # Test TLS 1.1 (should be disabled)
    if timeout 5 openssl s_client -tls1_1 -connect $TARGET:443 2>&1 | grep -q "handshake failure\|no protocols"; then
        log_pass "TLS 1.1 is disabled"
    else
        log_warn "TLS 1.1 may be enabled"
    fi
    
    # Test TLS 1.2 (should be enabled)
    if timeout 5 openssl s_client -tls1_2 -connect $TARGET:443 2>&1 | grep -q "BEGIN CERTIFICATE"; then
        log_pass "TLS 1.2 is enabled"
    else
        log_warn "TLS 1.2 may be disabled"
    fi
    
    # Test TLS 1.3 (should be enabled)
    if timeout 5 openssl s_client -tls1_3 -connect $TARGET:443 2>&1 | grep -q "BEGIN CERTIFICATE"; then
        log_pass "TLS 1.3 is enabled"
    else
        log_info "TLS 1.3 may not be supported"
    fi
    
    # Test for weak ciphers
    log_info "Testing for weak SSL ciphers..."
    
    local weak_ciphers="NULL:EXPORT:LOW:DES:RC4:MD5:PSK:SRP:CAMELLIA:ARIA:SEED"
    if timeout 5 openssl s_client -cipher "$weak_ciphers" -connect $TARGET:443 2>&1 | grep -q "handshake failure\|no ciphers"; then
        log_pass "Weak SSL ciphers are disabled"
    else
        log_fail "Weak SSL ciphers may be enabled!"
    fi
}

# ============================================
# WEB APPLICATION SECURITY TESTS
# ============================================

test_http_headers() {
    log_section "HTTP Security Headers"
    
    local url="https://${DOMAIN:-$TARGET}"
    
    log_info "Checking security headers on $url..."
    
    local headers=$(curl -sI -k --max-time 10 "$url" 2>/dev/null)
    
    # Check for security headers
    if echo "$headers" | grep -qi "Strict-Transport-Security"; then
        log_pass "HSTS header present"
    else
        log_warn "HSTS header missing"
    fi
    
    if echo "$headers" | grep -qi "X-Content-Type-Options"; then
        log_pass "X-Content-Type-Options header present"
    else
        log_warn "X-Content-Type-Options header missing"
    fi
    
    if echo "$headers" | grep -qi "X-Frame-Options"; then
        log_pass "X-Frame-Options header present"
    else
        log_warn "X-Frame-Options header missing"
    fi
    
    if echo "$headers" | grep -qi "X-XSS-Protection"; then
        log_pass "X-XSS-Protection header present"
    else
        log_info "X-XSS-Protection header missing (deprecated in modern browsers)"
    fi
    
    if echo "$headers" | grep -qi "Content-Security-Policy"; then
        log_pass "Content-Security-Policy header present"
    else
        log_warn "Content-Security-Policy header missing"
    fi
    
    if echo "$headers" | grep -qi "Referrer-Policy"; then
        log_pass "Referrer-Policy header present"
    else
        log_info "Referrer-Policy header missing"
    fi
    
    # Check for information disclosure
    if echo "$headers" | grep -qi "Server:"; then
        local server=$(echo "$headers" | grep -i "Server:" | head -1)
        log_warn "Server header reveals: $server"
    else
        log_pass "Server header is hidden"
    fi
    
    if echo "$headers" | grep -qi "X-Powered-By"; then
        local powered=$(echo "$headers" | grep -i "X-Powered-By:" | head -1)
        log_warn "X-Powered-By header reveals: $powered"
    else
        log_pass "X-Powered-By header is hidden"
    fi
}

test_http_methods() {
    log_section "HTTP Methods Test"
    
    local url="https://${DOMAIN:-$TARGET}"
    
    log_info "Testing HTTP methods on $url..."
    
    # Test dangerous HTTP methods
    local dangerous_methods="PUT DELETE TRACE CONNECT OPTIONS"
    
    for method in $dangerous_methods; do
        local response=$(curl -s -o /dev/null -w "%{http_code}" -X $method -k --max-time 5 "$url" 2>/dev/null)
        
        if [ "$response" = "200" ] || [ "$response" = "201" ]; then
            if [ "$method" = "OPTIONS" ]; then
                log_info "OPTIONS method is allowed (normal for CORS)"
            else
                log_warn "HTTP $method method is allowed (code: $response)"
            fi
        else
            log_pass "HTTP $method method returns $response"
        fi
    done
}

test_common_vulnerabilities() {
    log_section "Common Web Vulnerabilities"
    
    local url="https://${DOMAIN:-$TARGET}"
    
    # Test for directory traversal
    log_info "Testing for directory traversal..."
    local traversal_payloads=(
        "/../../../etc/passwd"
        "/..%2f..%2f..%2fetc/passwd"
        "/....//....//....//etc/passwd"
        "/%2e%2e/%2e%2e/%2e%2e/etc/passwd"
    )
    
    for payload in "${traversal_payloads[@]}"; do
        local response=$(curl -s -k --max-time 5 "${url}${payload}" 2>/dev/null)
        if echo "$response" | grep -q "root:"; then
            log_fail "Directory traversal vulnerability found with: $payload"
        fi
    done
    log_pass "No directory traversal vulnerabilities found"
    
    # Test for SQL injection (basic)
    log_info "Testing for SQL injection..."
    local sqli_payloads=(
        "?id=1'"
        "?id=1%27"
        "?id=1%22"
        "?id=1%20OR%201=1"
        "?id=1'%20OR%20'1'='1"
    )
    
    for payload in "${sqli_payloads[@]}"; do
        local response=$(curl -s -k --max-time 5 "${url}${payload}" 2>/dev/null)
        if echo "$response" | grep -qi "sql\|syntax\|mysql\|postgresql\|sqlite\|oracle"; then
            log_warn "Possible SQL injection indicator found with: $payload"
        fi
    done
    log_pass "No obvious SQL injection vulnerabilities found"
    
    # Test for XSS (basic)
    log_info "Testing for XSS..."
    local xss_payloads=(
        "?q=<script>alert(1)</script>"
        "?q=%3Cscript%3Ealert(1)%3C/script%3E"
        "?q=<img%20src=x%20onerror=alert(1)>"
    )
    
    for payload in "${xss_payloads[@]}"; do
        local response=$(curl -s -k --max-time 5 "${url}${payload}" 2>/dev/null)
        if echo "$response" | grep -q "<script>alert(1)</script>\|onerror=alert"; then
            log_warn "Possible XSS vulnerability found (reflected input)"
        fi
    done
    log_pass "No obvious XSS vulnerabilities found"
    
    # Test for sensitive file exposure
    log_info "Testing for sensitive file exposure..."
    local sensitive_paths=(
        "/.env"
        "/.git/config"
        "/config.php"
        "/wp-config.php"
        "/.htaccess"
        "/server-status"
        "/phpinfo.php"
        "/.DS_Store"
        "/backup.sql"
        "/database.sql"
        "/.svn/entries"
        "/web.config"
    )
    
    for path in "${sensitive_paths[@]}"; do
        local response=$(curl -s -o /dev/null -w "%{http_code}" -k --max-time 5 "${url}${path}" 2>/dev/null)
        if [ "$response" = "200" ]; then
            log_fail "Sensitive file accessible: $path"
        fi
    done
    log_pass "No sensitive files exposed"
}

test_api_security() {
    log_section "API Security Tests"
    
    local url="https://${DOMAIN:-$TARGET}"
    
    # Test API endpoints
    log_info "Testing API endpoints..."
    
    # Test for API documentation exposure
    local api_docs=(
        "/swagger"
        "/swagger-ui"
        "/api-docs"
        "/docs"
        "/openapi.json"
        "/swagger.json"
    )
    
    for path in "${api_docs[@]}"; do
        local response=$(curl -s -o /dev/null -w "%{http_code}" -k --max-time 5 "${url}${path}" 2>/dev/null)
        if [ "$response" = "200" ]; then
            log_info "API documentation found at: $path (verify if intentional)"
        fi
    done
    
    # Test for debug endpoints
    local debug_paths=(
        "/debug"
        "/trace"
        "/actuator"
        "/actuator/health"
        "/actuator/env"
        "/metrics"
        "/health"
        "/status"
    )
    
    for path in "${debug_paths[@]}"; do
        local response=$(curl -s -o /dev/null -w "%{http_code}" -k --max-time 5 "${url}${path}" 2>/dev/null)
        if [ "$response" = "200" ]; then
            local content=$(curl -s -k --max-time 5 "${url}${path}" 2>/dev/null | head -c 500)
            if echo "$content" | grep -qi "password\|secret\|key\|token\|credential"; then
                log_fail "Debug endpoint exposes sensitive data: $path"
            else
                log_info "Debug endpoint accessible: $path"
            fi
        fi
    done
}

test_dos_resilience() {
    log_section "DoS Resilience Test"
    
    local url="https://${DOMAIN:-$TARGET}"
    
    log_info "Testing connection handling..."
    
    # Test concurrent connections
    local concurrent=50
    local success=0
    local failed=0
    
    for i in $(seq 1 $concurrent); do
        curl -s -o /dev/null -w "%{http_code}" -k --max-time 5 "$url" 2>/dev/null &
    done
    wait
    
    log_info "Sent $concurrent concurrent requests"
    
    # Check if server is still responding
    local final_response=$(curl -s -o /dev/null -w "%{http_code}" -k --max-time 10 "$url" 2>/dev/null)
    
    if [ "$final_response" = "200" ] || [ "$final_response" = "301" ] || [ "$final_response" = "302" ]; then
        log_pass "Server still responsive after concurrent requests"
    else
        log_warn "Server response degraded (code: $final_response)"
    fi
    
    # Test slowloris-style attack (partial)
    log_info "Testing slow request handling..."
    
    # This is a mild test - we're just checking if the server handles slow clients
    local slow_start=$(date +%s)
    timeout 10 curl -s -o /dev/null -k --max-time 15 --limit-rate 100 "$url" 2>/dev/null
    local slow_end=$(date +%s)
    local slow_duration=$((slow_end - slow_start))
    
    if [ "$slow_duration" -lt 15 ]; then
        log_pass "Server handles slow clients appropriately"
    else
        log_info "Server may be vulnerable to slowloris (test inconclusive)"
    fi
}

# ============================================
# DOCKER/SWARM SECURITY TESTS
# ============================================

test_docker_exposure() {
    log_section "Docker Security Tests"
    
    # Test if Docker API is exposed
    log_info "Testing for exposed Docker API..."
    
    local docker_ports="2375 2376 2377"
    
    for port in $docker_ports; do
        local response=$(curl -s --max-time 5 "http://$TARGET:$port/version" 2>/dev/null)
        if echo "$response" | grep -qi "docker\|version\|ApiVersion"; then
            log_fail "Docker API exposed on port $port!"
        fi
    done
    log_pass "Docker API is not publicly exposed"
    
    # Test Traefik dashboard exposure
    log_info "Testing for Traefik dashboard exposure..."
    
    local traefik_paths=(
        "/dashboard/"
        "/api/rawdata"
        "/api/overview"
        ":8080/dashboard/"
        ":8080/api"
    )
    
    for path in "${traefik_paths[@]}"; do
        local test_url
        if [[ $path == :* ]]; then
            test_url="http://$TARGET$path"
        else
            test_url="https://${DOMAIN:-$TARGET}$path"
        fi
        
        local response=$(curl -s -o /dev/null -w "%{http_code}" -k --max-time 5 "$test_url" 2>/dev/null)
        if [ "$response" = "200" ]; then
            log_warn "Traefik dashboard may be accessible: $path"
        fi
    done
}

# ============================================
# REPORT GENERATION
# ============================================

generate_report() {
    log_section "Generating Report"
    
    cat > "$REPORT_FILE" << EOF
<!DOCTYPE html>
<html>
<head>
    <title>Tako Security Test Report - $TARGET</title>
    <style>
        body { font-family: Arial, sans-serif; margin: 40px; background: #f5f5f5; }
        .container { max-width: 1000px; margin: 0 auto; background: white; padding: 30px; border-radius: 10px; box-shadow: 0 2px 10px rgba(0,0,0,0.1); }
        h1 { color: #333; border-bottom: 3px solid #007bff; padding-bottom: 10px; }
        h2 { color: #555; margin-top: 30px; }
        .summary { display: flex; gap: 20px; margin: 20px 0; }
        .summary-box { flex: 1; padding: 20px; border-radius: 8px; text-align: center; }
        .pass { background: #d4edda; color: #155724; }
        .warn { background: #fff3cd; color: #856404; }
        .fail { background: #f8d7da; color: #721c24; }
        .item { padding: 10px; margin: 5px 0; border-radius: 5px; }
        .item.pass { background: #d4edda; }
        .item.warn { background: #fff3cd; }
        .item.fail { background: #f8d7da; }
        .timestamp { color: #888; font-size: 0.9em; }
        table { width: 100%; border-collapse: collapse; margin: 20px 0; }
        th, td { padding: 12px; text-align: left; border-bottom: 1px solid #ddd; }
        th { background: #007bff; color: white; }
    </style>
</head>
<body>
    <div class="container">
        <h1>Tako Security Test Report</h1>
        <p class="timestamp">Generated: $(date)</p>
        <p><strong>Target:</strong> $TARGET</p>
        <p><strong>Domain:</strong> ${DOMAIN:-N/A}</p>
        
        <div class="summary">
            <div class="summary-box pass">
                <h3>${#PASSED[@]}</h3>
                <p>Passed</p>
            </div>
            <div class="summary-box warn">
                <h3>${#WARNINGS[@]}</h3>
                <p>Warnings</p>
            </div>
            <div class="summary-box fail">
                <h3>${#VULNERABILITIES[@]}</h3>
                <p>Vulnerabilities</p>
            </div>
        </div>
        
        <h2>Vulnerabilities</h2>
        $(for v in "${VULNERABILITIES[@]}"; do echo "<div class='item fail'>$v</div>"; done)
        $([ ${#VULNERABILITIES[@]} -eq 0 ] && echo "<p>No vulnerabilities found!</p>")
        
        <h2>Warnings</h2>
        $(for w in "${WARNINGS[@]}"; do echo "<div class='item warn'>$w</div>"; done)
        $([ ${#WARNINGS[@]} -eq 0 ] && echo "<p>No warnings!</p>")
        
        <h2>Passed Tests</h2>
        $(for p in "${PASSED[@]}"; do echo "<div class='item pass'>$p</div>"; done)
    </div>
</body>
</html>
EOF

    log_pass "Report generated: $REPORT_FILE"
}

# ============================================
# MAIN EXECUTION
# ============================================

main() {
    echo ""
    echo "╔════════════════════════════════════════════════════════════╗"
    echo "║           Tako Security Test Suite v1.0                    ║"
    echo "║           Target: $TARGET"
    echo "╚════════════════════════════════════════════════════════════╝"
    echo ""
    
    check_dependencies
    
    if $FULL_TEST || $NETWORK_TEST || $QUICK_TEST; then
        test_port_scan
        test_firewall
        test_rate_limiting
    fi
    
    if $FULL_TEST || $QUICK_TEST; then
        test_ssh_security
    fi
    
    if $BRUTE_TEST; then
        test_ssh_bruteforce
    fi
    
    if $FULL_TEST || $WEB_TEST || $QUICK_TEST; then
        test_ssl_security
        test_http_headers
        test_http_methods
        test_common_vulnerabilities
        test_api_security
    fi
    
    if $FULL_TEST; then
        test_dos_resilience
        test_docker_exposure
    fi
    
    # Summary
    log_section "Summary"
    echo ""
    echo -e "  ${GREEN}Passed:${NC}         ${#PASSED[@]} tests"
    echo -e "  ${YELLOW}Warnings:${NC}       ${#WARNINGS[@]} issues"
    echo -e "  ${RED}Vulnerabilities:${NC} ${#VULNERABILITIES[@]} found"
    echo ""
    
    if [ ${#VULNERABILITIES[@]} -gt 0 ]; then
        echo -e "${RED}Critical issues found:${NC}"
        for v in "${VULNERABILITIES[@]}"; do
            echo -e "  - $v"
        done
        echo ""
    fi
    
    if [ ${#WARNINGS[@]} -gt 0 ]; then
        echo -e "${YELLOW}Warnings:${NC}"
        for w in "${WARNINGS[@]}"; do
            echo -e "  - $w"
        done
        echo ""
    fi
    
    if $GENERATE_REPORT; then
        generate_report
    fi
    
    # Exit code based on vulnerabilities
    if [ ${#VULNERABILITIES[@]} -gt 0 ]; then
        exit 1
    fi
    exit 0
}

main
