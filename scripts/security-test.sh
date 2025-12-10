#!/bin/bash
#
# Tako Security Test Suite v1.0
# Comprehensive security testing for Tako-deployed servers
#
# Usage: ./security-test.sh <target_ip> <domain>
#
# Prerequisites (run first):
#   apt-get update && apt-get install -y nmap hydra nikto sslscan netcat-openbsd jq
#
# Example:
#   ./security-test.sh 77.42.21.99 hono.77.42.21.99.sslip.io
#

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

# Configuration
TARGET="${1:-}"
DOMAIN="${2:-}"
REPORT_FILE="security-report-$(date +%Y%m%d-%H%M%S).html"

if [ -z "$TARGET" ]; then
    echo "Usage: $0 <target_ip> <domain>"
    echo ""
    echo "Prerequisites:"
    echo "  apt-get update && apt-get install -y nmap hydra nikto sslscan netcat-openbsd jq"
    echo ""
    echo "Example:"
    echo "  $0 77.42.21.99 hono.77.42.21.99.sslip.io"
    exit 1
fi

[ -z "$DOMAIN" ] && DOMAIN="$TARGET"

# Results
declare -a PASSED=()
declare -a WARNINGS=()
declare -a FAILURES=()

log_pass() { echo -e "${GREEN}[PASS]${NC} $1"; PASSED+=("$1"); }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; WARNINGS+=("$1"); }
log_fail() { echo -e "${RED}[FAIL]${NC} $1"; FAILURES+=("$1"); }
log_info() { echo -e "${BLUE}[INFO]${NC} $1"; }
log_section() { echo -e "\n${CYAN}═══════════════════════════════════════════════════════════${NC}"; echo -e "${CYAN} $1${NC}"; echo -e "${CYAN}═══════════════════════════════════════════════════════════${NC}\n"; }

# ============================================================================
# DEPENDENCY CHECK
# ============================================================================
check_deps() {
    log_section "Checking Dependencies"
    local missing=()
    
    for tool in curl openssl nc nmap; do
        if command -v $tool &>/dev/null; then
            log_pass "$tool installed"
        else
            log_warn "$tool missing"
            missing+=($tool)
        fi
    done
    
    for tool in hydra sslscan nikto; do
        if command -v $tool &>/dev/null; then
            log_pass "$tool installed (optional)"
        else
            log_info "$tool not installed (optional - some tests skipped)"
        fi
    done
    
    if [ ${#missing[@]} -gt 0 ]; then
        echo ""
        log_warn "Install missing tools: apt-get install -y ${missing[*]}"
    fi
}

# ============================================================================
# PORT SCANNING
# ============================================================================
test_ports() {
    log_section "Port Scan Analysis"
    
    if ! command -v nmap &>/dev/null; then
        log_warn "nmap not installed, using basic port check"
        
        # Basic port check with nc
        local common_ports="21 22 23 25 80 443 2377 3306 5432 6379 7946 8080 27017"
        for port in $common_ports; do
            if timeout 2 nc -zv $TARGET $port 2>&1 | grep -q "succeeded\|open"; then
                case $port in
                    22|80|443|2377|7946) log_pass "Port $port open (expected)" ;;
                    21) log_fail "FTP port 21 is open!" ;;
                    23) log_fail "Telnet port 23 is open!" ;;
                    25) log_warn "SMTP port 25 is open" ;;
                    3306) log_fail "MySQL port 3306 is exposed!" ;;
                    5432) log_fail "PostgreSQL port 5432 is exposed!" ;;
                    6379) log_fail "Redis port 6379 is exposed!" ;;
                    8080) log_warn "Port 8080 is open (Traefik dashboard?)" ;;
                    27017) log_fail "MongoDB port 27017 is exposed!" ;;
                    *) log_info "Port $port is open" ;;
                esac
            fi
        done
        return
    fi
    
    log_info "Running comprehensive port scan..."
    
    # Quick SYN scan of common ports
    local scan_result=$(nmap -sS -T4 --top-ports 1000 -Pn $TARGET 2>/dev/null)
    echo "$scan_result" | grep "^[0-9]" | head -20
    echo ""
    
    # Check for dangerous open ports
    local dangerous_ports="21 23 25 111 135 139 445 1433 3306 3389 5432 5900 6379 11211 27017"
    for port in $dangerous_ports; do
        if echo "$scan_result" | grep -q "^$port/.*open"; then
            log_fail "Dangerous port $port is open!"
        fi
    done
    
    # Check expected ports
    local expected_open="22 80 443"
    for port in $expected_open; do
        if echo "$scan_result" | grep -q "^$port/.*open"; then
            log_pass "Port $port is open (expected)"
        fi
    done
    
    # Full port scan for thorough testing
    log_info "Running full port scan (this may take a minute)..."
    local full_scan=$(nmap -sS -T4 -p- --min-rate=10000 -Pn $TARGET 2>/dev/null | grep "^[0-9].*open")
    local open_count=$(echo "$full_scan" | wc -l)
    
    if [ "$open_count" -gt 10 ]; then
        log_warn "Many open ports detected ($open_count). Review: "
        echo "$full_scan"
    else
        log_pass "Limited number of open ports ($open_count)"
    fi
}

# ============================================================================
# SSH SECURITY
# ============================================================================
test_ssh() {
    log_section "SSH Security Tests"
    
    # Get SSH banner
    local banner=$(echo "" | timeout 5 nc $TARGET 22 2>/dev/null | head -1)
    log_info "SSH Banner: $banner"
    
    # Check SSH version
    if echo "$banner" | grep -qE "OpenSSH_[1-6]\."; then
        log_fail "Outdated SSH version!"
    elif echo "$banner" | grep -qi "openssh"; then
        log_pass "SSH version appears current"
    fi
    
    # Test password authentication
    log_info "Testing password authentication..."
    local auth_test=$(timeout 10 ssh -v -o BatchMode=yes -o ConnectTimeout=5 \
        -o PreferredAuthentications=password \
        -o PubkeyAuthentication=no \
        -o StrictHostKeyChecking=no \
        test@$TARGET 2>&1 || true)
    
    if echo "$auth_test" | grep -qi "password"; then
        if echo "$auth_test" | grep -qi "permission denied"; then
            log_pass "Password authentication disabled or working correctly"
        else
            log_warn "Password authentication may be enabled"
        fi
    else
        log_pass "Password authentication appears disabled"
    fi
    
    # Test weak ciphers
    log_info "Testing for weak ciphers..."
    local weak_ciphers="3des-cbc aes128-cbc aes192-cbc aes256-cbc"
    for cipher in $weak_ciphers; do
        if timeout 5 ssh -o Ciphers=$cipher -o BatchMode=yes -o ConnectTimeout=3 \
            -o StrictHostKeyChecking=no test@$TARGET 2>&1 | grep -qi "no matching cipher"; then
            log_pass "Weak cipher $cipher is disabled"
        else
            log_warn "Weak cipher $cipher may be enabled"
        fi
    done
    
    # Test weak key exchange
    log_info "Testing for weak key exchange algorithms..."
    local weak_kex="diffie-hellman-group1-sha1"
    if timeout 5 ssh -o KexAlgorithms=$weak_kex -o BatchMode=yes -o ConnectTimeout=3 \
        -o StrictHostKeyChecking=no test@$TARGET 2>&1 | grep -qi "no matching"; then
        log_pass "Weak KEX diffie-hellman-group1-sha1 is disabled"
    else
        log_warn "Weak KEX may be enabled"
    fi
}

# ============================================================================
# SSH BRUTE FORCE TEST
# ============================================================================
test_ssh_bruteforce() {
    log_section "SSH Brute Force Test (fail2ban validation)"
    
    if ! command -v hydra &>/dev/null; then
        log_warn "hydra not installed, skipping brute force test"
        log_info "Install: apt-get install -y hydra"
        return
    fi
    
    log_warn "Testing SSH brute force protection (will trigger fail2ban)..."
    
    # Create test wordlist
    cat > /tmp/tako-wordlist.txt << 'EOF'
admin
root
test
password
123456
letmein
welcome
password123
admin123
root123
qwerty
EOF

    # Run hydra with limited attempts
    local result=$(timeout 120 hydra -l root -P /tmp/tako-wordlist.txt \
        -t 4 -w 3 -f -V ssh://$TARGET 2>&1 || true)
    
    rm -f /tmp/tako-wordlist.txt
    
    if echo "$result" | grep -qi "valid password found"; then
        log_fail "SSH brute force found valid credentials!"
        echo "$result" | grep -i "password"
    elif echo "$result" | grep -qi "blocked\|banned\|refused\|error.*connect"; then
        log_pass "Brute force protection (fail2ban) is working!"
    else
        log_pass "No valid credentials found"
    fi
    
    # Check fail2ban status
    log_info "Checking fail2ban status..."
    if command -v fail2ban-client &>/dev/null; then
        fail2ban-client status sshd 2>/dev/null || log_info "fail2ban-client not available locally"
    fi
}

# ============================================================================
# SSL/TLS SECURITY
# ============================================================================
test_ssl() {
    log_section "SSL/TLS Security Tests"
    
    local test_host="$DOMAIN"
    
    # Certificate check
    log_info "Checking SSL certificate..."
    local cert_info=$(echo | timeout 10 openssl s_client -servername $test_host -connect $TARGET:443 2>/dev/null)
    
    if echo "$cert_info" | grep -q "BEGIN CERTIFICATE"; then
        log_pass "SSL certificate present"
        
        # Check expiry
        local expiry=$(echo "$cert_info" | openssl x509 -noout -enddate 2>/dev/null | cut -d= -f2)
        local expiry_epoch=$(date -d "$expiry" +%s 2>/dev/null || echo 0)
        local now_epoch=$(date +%s)
        local days_left=$(( (expiry_epoch - now_epoch) / 86400 ))
        
        if [ "$days_left" -lt 0 ]; then
            log_fail "SSL certificate EXPIRED!"
        elif [ "$days_left" -lt 7 ]; then
            log_fail "SSL certificate expires in $days_left days!"
        elif [ "$days_left" -lt 30 ]; then
            log_warn "SSL certificate expires in $days_left days"
        else
            log_pass "SSL certificate valid for $days_left days"
        fi
        
        # Check issuer
        local issuer=$(echo "$cert_info" | openssl x509 -noout -issuer 2>/dev/null)
        log_info "Issuer: $issuer"
    else
        log_fail "No SSL certificate found!"
    fi
    
    # TLS version tests
    log_info "Testing TLS versions..."
    
    for ver in tls1 tls1_1; do
        if timeout 5 openssl s_client -$ver -connect $TARGET:443 2>&1 | grep -qi "handshake failure\|no protocols\|unsupported"; then
            log_pass "$(echo $ver | tr '_' '.') is disabled"
        else
            log_warn "$(echo $ver | tr '_' '.') may be enabled (should be disabled)"
        fi
    done
    
    for ver in tls1_2 tls1_3; do
        if timeout 5 openssl s_client -$ver -connect $TARGET:443 2>&1 | grep -q "BEGIN CERTIFICATE"; then
            log_pass "$(echo $ver | tr '_' '.') is enabled"
        else
            log_info "$(echo $ver | tr '_' '.') may not be supported"
        fi
    done
    
    # Weak cipher test
    log_info "Testing for weak ciphers..."
    local weak="NULL:EXPORT:LOW:DES:RC4:MD5"
    local weak_result=$(timeout 5 openssl s_client -cipher "$weak" -connect $TARGET:443 2>&1)
    if echo "$weak_result" | grep -qi "handshake failure\|no ciphers\|no cipher"; then
        log_pass "Weak SSL ciphers disabled"
    elif echo "$weak_result" | grep -qi "self-signed\|TRAEFIK DEFAULT"; then
        # Got default/fallback cert, not a successful weak cipher connection
        log_pass "Weak SSL ciphers disabled (fallback cert returned)"
    else
        log_warn "Weak SSL ciphers test inconclusive - verify with sslscan output below"
    fi
    
    # SSLScan for detailed analysis
    if command -v sslscan &>/dev/null; then
        log_info "Running detailed SSL scan..."
        sslscan --no-colour $TARGET:443 2>/dev/null | grep -E "Accepted|SSLv|TLSv|RC4|MD5|NULL|EXPORT|DES" | head -30
    fi
}

# ============================================================================
# HTTP SECURITY HEADERS
# ============================================================================
test_http_headers() {
    log_section "HTTP Security Headers"
    
    local url="https://$DOMAIN"
    log_info "Testing headers on $url"
    
    local headers=$(curl -sI -k --max-time 10 "$url" 2>/dev/null)
    
    # Required security headers
    local required_headers=(
        "Strict-Transport-Security:HSTS"
        "X-Content-Type-Options:X-Content-Type-Options"
        "X-Frame-Options:X-Frame-Options"
        "Content-Security-Policy:CSP"
    )
    
    for h in "${required_headers[@]}"; do
        local header=$(echo $h | cut -d: -f1)
        local name=$(echo $h | cut -d: -f2)
        if echo "$headers" | grep -qi "^$header"; then
            log_pass "$name header present"
        else
            log_warn "$name header missing"
        fi
    done
    
    # Information disclosure
    if echo "$headers" | grep -qi "^Server:"; then
        local server=$(echo "$headers" | grep -i "^Server:" | head -1)
        log_warn "Server header reveals: $server"
    else
        log_pass "Server header hidden"
    fi
    
    if echo "$headers" | grep -qi "^X-Powered-By:"; then
        local powered=$(echo "$headers" | grep -i "^X-Powered-By:" | head -1)
        log_warn "X-Powered-By reveals: $powered"
    else
        log_pass "X-Powered-By header hidden"
    fi
}

# ============================================================================
# WEB VULNERABILITY TESTS
# ============================================================================
test_web_vulns() {
    log_section "Web Vulnerability Tests"
    
    local url="https://$DOMAIN"
    
    # Directory traversal
    log_info "Testing directory traversal..."
    local traversal_payloads=(
        "/../../../etc/passwd"
        "/..%2f..%2f..%2fetc/passwd"
        "/%2e%2e/%2e%2e/%2e%2e/etc/passwd"
    )
    local traversal_found=false
    for payload in "${traversal_payloads[@]}"; do
        if curl -sk --max-time 5 "${url}${payload}" 2>/dev/null | grep -q "root:"; then
            log_fail "Directory traversal vulnerability: $payload"
            traversal_found=true
        fi
    done
    [ "$traversal_found" = false ] && log_pass "No directory traversal found"
    
    # Sensitive files
    log_info "Testing sensitive file exposure..."
    local sensitive_files=(
        "/.env" "/.git/config" "/.git/HEAD" "/config.php" "/wp-config.php"
        "/.htaccess" "/.svn/entries" "/backup.sql" "/dump.sql" "/.DS_Store"
        "/server-status" "/phpinfo.php" "/info.php" "/test.php" "/debug"
        "/elmah.axd" "/trace.axd" "/web.config"
    )
    local exposed=false
    for file in "${sensitive_files[@]}"; do
        local code=$(curl -sk -o /dev/null -w "%{http_code}" --max-time 3 "${url}${file}" 2>/dev/null)
        if [ "$code" = "200" ]; then
            log_fail "Sensitive file exposed: $file"
            exposed=true
        fi
    done
    [ "$exposed" = false ] && log_pass "No sensitive files exposed"
    
    # SQL Injection basic test
    log_info "Testing for SQL injection indicators..."
    local sqli_payloads=("?id=1'" "?id=1%27" "?id=1%22OR%221%22=%221")
    local sqli_found=false
    for payload in "${sqli_payloads[@]}"; do
        local response=$(curl -sk --max-time 5 "${url}${payload}" 2>/dev/null)
        if echo "$response" | grep -qiE "sql|syntax|mysql|postgresql|sqlite|oracle|ORA-"; then
            log_warn "Possible SQL injection indicator: $payload"
            sqli_found=true
        fi
    done
    [ "$sqli_found" = false ] && log_pass "No obvious SQL injection found"
    
    # HTTP Methods
    log_info "Testing HTTP methods..."
    for method in PUT DELETE TRACE; do
        local code=$(curl -sk -o /dev/null -w "%{http_code}" -X $method --max-time 5 "$url" 2>/dev/null)
        if [ "$code" = "200" ] || [ "$code" = "201" ]; then
            log_warn "HTTP $method method allowed (code: $code)"
        else
            log_pass "HTTP $method returns $code"
        fi
    done
}

# ============================================================================
# DOCKER/INFRASTRUCTURE SECURITY
# ============================================================================
test_docker() {
    log_section "Docker & Infrastructure Security"
    
    # Docker API exposure
    log_info "Testing Docker API exposure..."
    for port in 2375 2376; do
        local response=$(curl -s --max-time 3 "http://$TARGET:$port/version" 2>/dev/null)
        if echo "$response" | grep -qi "docker\|ApiVersion"; then
            log_fail "Docker API exposed on port $port!"
        else
            log_pass "Docker API not exposed on port $port"
        fi
    done
    
    # Traefik dashboard
    log_info "Testing Traefik dashboard exposure..."
    for path in ":8080" ":8080/dashboard/" ":8080/api"; do
        local code=$(curl -sk -o /dev/null -w "%{http_code}" --max-time 3 "http://$TARGET$path" 2>/dev/null)
        if [ "$code" = "200" ]; then
            log_warn "Traefik dashboard may be exposed at $path"
        fi
    done
    
    # Kubernetes API (if applicable)
    for port in 6443 8443 10250; do
        if timeout 2 nc -zv $TARGET $port 2>&1 | grep -q "succeeded\|open"; then
            log_warn "Kubernetes-related port $port is open"
        fi
    done
}

# ============================================================================
# DOS RESILIENCE
# ============================================================================
test_dos() {
    log_section "DoS Resilience Test"
    
    local url="https://$DOMAIN"
    
    log_info "Testing concurrent connection handling..."
    
    # Send concurrent requests
    for i in $(seq 1 30); do
        curl -sk -o /dev/null --max-time 5 "$url" 2>/dev/null &
    done
    wait
    
    # Check if still responsive
    sleep 2
    local code=$(curl -sk -o /dev/null -w "%{http_code}" --max-time 10 "$url" 2>/dev/null)
    
    if [ "$code" = "200" ] || [ "$code" = "301" ] || [ "$code" = "302" ]; then
        log_pass "Server responsive after concurrent requests (code: $code)"
    else
        log_warn "Server response degraded (code: $code)"
    fi
}

# ============================================================================
# NIKTO WEB SCANNER
# ============================================================================
test_nikto() {
    log_section "Nikto Web Vulnerability Scanner"
    
    if ! command -v nikto &>/dev/null; then
        log_warn "nikto not installed, skipping"
        log_info "Install: apt-get install -y nikto"
        return
    fi
    
    log_info "Running Nikto scan (this may take several minutes)..."
    nikto -h https://$DOMAIN -ssl -Tuning 123bde -maxtime 300 2>/dev/null | \
        grep -E "^\+|OSVDB|CVE|vulnerability|found" | head -50
}

# ============================================================================
# GENERATE REPORT
# ============================================================================
generate_report() {
    log_section "Generating Report"
    
    cat > "$REPORT_FILE" << EOF
<!DOCTYPE html>
<html>
<head>
    <title>Tako Security Report - $TARGET</title>
    <style>
        body{font-family:system-ui,sans-serif;margin:40px;background:#1a1a2e;color:#eee}
        .container{max-width:900px;margin:0 auto;background:#16213e;padding:30px;border-radius:12px}
        h1{color:#e94560;border-bottom:2px solid #e94560;padding-bottom:15px}
        h2{color:#0f3460;background:#e94560;padding:10px;border-radius:6px;margin-top:30px}
        .summary{display:flex;gap:15px;margin:25px 0}
        .box{flex:1;padding:20px;border-radius:8px;text-align:center}
        .pass{background:#1b4332;color:#95d5b2}
        .warn{background:#774936;color:#ffcb69}
        .fail{background:#641220;color:#ff758f}
        .item{padding:10px;margin:5px 0;border-radius:5px;border-left:4px solid}
        .item.pass{background:#1b4332;border-color:#52b788}
        .item.warn{background:#774936;border-color:#f77f00}
        .item.fail{background:#641220;border-color:#ef233c}
        code{background:#0f3460;padding:2px 6px;border-radius:3px}
        .timestamp{color:#888;font-size:0.9em}
    </style>
</head>
<body>
<div class="container">
    <h1>Tako Security Test Report</h1>
    <p class="timestamp">Generated: $(date)</p>
    <p><strong>Target:</strong> <code>$TARGET</code></p>
    <p><strong>Domain:</strong> <code>$DOMAIN</code></p>
    
    <div class="summary">
        <div class="box pass"><h3>${#PASSED[@]}</h3><p>Passed</p></div>
        <div class="box warn"><h3>${#WARNINGS[@]}</h3><p>Warnings</p></div>
        <div class="box fail"><h3>${#FAILURES[@]}</h3><p>Failures</p></div>
    </div>
    
    <h2>Critical Failures</h2>
    $([ ${#FAILURES[@]} -eq 0 ] && echo "<p>No critical failures!</p>")
    $(for f in "${FAILURES[@]}"; do echo "<div class='item fail'>$f</div>"; done)
    
    <h2>Warnings</h2>
    $([ ${#WARNINGS[@]} -eq 0 ] && echo "<p>No warnings!</p>")
    $(for w in "${WARNINGS[@]}"; do echo "<div class='item warn'>$w</div>"; done)
    
    <h2>Passed Tests</h2>
    $(for p in "${PASSED[@]}"; do echo "<div class='item pass'>$p</div>"; done)
</div>
</body>
</html>
EOF
    
    log_pass "Report saved: $REPORT_FILE"
}

# ============================================================================
# MAIN
# ============================================================================
main() {
    echo ""
    echo -e "${CYAN}╔═══════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${CYAN}║         Tako Security Test Suite v1.0                         ║${NC}"
    echo -e "${CYAN}║         Target: $TARGET${NC}"
    echo -e "${CYAN}║         Domain: $DOMAIN${NC}"
    echo -e "${CYAN}╚═══════════════════════════════════════════════════════════════╝${NC}"
    
    check_deps
    test_ports
    test_ssh
    test_ssh_bruteforce
    test_ssl
    test_http_headers
    test_web_vulns
    test_docker
    test_dos
    test_nikto
    
    generate_report
    
    # Final Summary
    log_section "Final Summary"
    echo -e "  ${GREEN}Passed:${NC}   ${#PASSED[@]}"
    echo -e "  ${YELLOW}Warnings:${NC} ${#WARNINGS[@]}"
    echo -e "  ${RED}Failures:${NC} ${#FAILURES[@]}"
    echo ""
    
    if [ ${#FAILURES[@]} -gt 0 ]; then
        echo -e "${RED}Critical issues require immediate attention:${NC}"
        for f in "${FAILURES[@]}"; do
            echo -e "  ${RED}•${NC} $f"
        done
        echo ""
    fi
    
    if [ ${#WARNINGS[@]} -gt 0 ]; then
        echo -e "${YELLOW}Warnings to review:${NC}"
        for w in "${WARNINGS[@]}"; do
            echo -e "  ${YELLOW}•${NC} $w"
        done
        echo ""
    fi
    
    echo -e "Full report: ${CYAN}$REPORT_FILE${NC}"
    echo ""
    
    [ ${#FAILURES[@]} -gt 0 ] && exit 1
    exit 0
}

main "$@"
