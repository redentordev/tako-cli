package alerttarget

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

func NormalizeAndValidate(provider, value string) (string, error) {
	normalized := strings.TrimSpace(value)
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", fmt.Errorf("must be an absolute HTTPS URL without userinfo or fragment")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", fmt.Errorf("must not target a local address")
	}
	if address, err := netip.ParseAddr(host); err == nil && !IsPublicIP(net.IP(address.AsSlice())) {
		return "", fmt.Errorf("must not target a local, private, or special-use address")
	}
	switch provider {
	case "slack":
		if host != "hooks.slack.com" && host != "hooks.slack-gov.com" {
			return "", fmt.Errorf("must target an official Slack webhook host")
		}
	case "discord":
		if host != "discord.com" && host != "discordapp.com" {
			return "", fmt.Errorf("must target an official Discord webhook host")
		}
	case "webhook":
	default:
		return "", fmt.Errorf("unknown alert target provider")
	}
	return normalized, nil
}

func IsPublicIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("::/96"), netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"),
}
