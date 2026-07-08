package config

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// PortPublish is a parsed service ports entry: a host port bound directly on
// the node (docker --publish), bypassing tako-proxy for raw TCP/UDP traffic.
type PortPublish struct {
	// HostIP restricts the bind address; empty binds all interfaces.
	HostIP        string
	HostPort      int
	ContainerPort int
	// Protocol is "tcp" or "udp".
	Protocol string
}

// ParsePortPublish parses a ports entry in docker-compose publish syntax:
// "PORT", "HOST:CONTAINER", "IP:HOST:CONTAINER" (IPv6 in brackets), each with
// an optional "/tcp" or "/udp" suffix.
func ParsePortPublish(entry string) (PortPublish, error) {
	publish := PortPublish{Protocol: "tcp"}
	value := strings.TrimSpace(entry)
	if value == "" {
		return publish, fmt.Errorf("ports entries cannot be empty")
	}
	if base, proto, ok := strings.Cut(value, "/"); ok {
		switch strings.ToLower(strings.TrimSpace(proto)) {
		case "tcp":
			publish.Protocol = "tcp"
		case "udp":
			publish.Protocol = "udp"
		default:
			return publish, fmt.Errorf("invalid ports protocol %q (tcp or udp)", proto)
		}
		value = base
	}

	hostPart := value
	containerPart := ""
	if strings.HasPrefix(value, "[") {
		// Bracketed IPv6 bind address: [ADDR]:HOST:CONTAINER.
		addr, rest, ok := strings.Cut(strings.TrimPrefix(value, "["), "]")
		if !ok || !strings.HasPrefix(rest, ":") {
			return publish, fmt.Errorf("invalid ports entry %q", entry)
		}
		publish.HostIP = addr
		hostPart, containerPart, ok = strings.Cut(strings.TrimPrefix(rest, ":"), ":")
		if !ok {
			return publish, fmt.Errorf("ports entry %q must be IP:HOST:CONTAINER", entry)
		}
	} else if strings.Contains(value, ":") {
		parts := strings.Split(value, ":")
		switch len(parts) {
		case 2:
			hostPart, containerPart = parts[0], parts[1]
		case 3:
			publish.HostIP, hostPart, containerPart = parts[0], parts[1], parts[2]
		default:
			return publish, fmt.Errorf("invalid ports entry %q (use PORT, HOST:CONTAINER, or IP:HOST:CONTAINER)", entry)
		}
	}
	if publish.HostIP != "" {
		addr, err := netip.ParseAddr(publish.HostIP)
		if err != nil {
			return publish, fmt.Errorf("invalid ports bind address %q", publish.HostIP)
		}
		publish.HostIP = addr.String()
	}

	hostPort, err := parsePublishPort(hostPart)
	if err != nil {
		return publish, fmt.Errorf("invalid ports entry %q: %w", entry, err)
	}
	publish.HostPort = hostPort
	publish.ContainerPort = hostPort
	if containerPart != "" {
		containerPort, err := parsePublishPort(containerPart)
		if err != nil {
			return publish, fmt.Errorf("invalid ports entry %q: %w", entry, err)
		}
		publish.ContainerPort = containerPort
	}
	return publish, nil
}

func parsePublishPort(value string) (int, error) {
	port, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("port %q is not a number", value)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("port %d must be between 1 and 65535", port)
	}
	return port, nil
}

// String renders the entry in canonical docker --publish syntax.
func (p PortPublish) String() string {
	var builder strings.Builder
	if p.HostIP != "" {
		if strings.Contains(p.HostIP, ":") {
			builder.WriteString("[" + p.HostIP + "]:")
		} else {
			builder.WriteString(p.HostIP + ":")
		}
	}
	builder.WriteString(strconv.Itoa(p.HostPort))
	builder.WriteString(":")
	builder.WriteString(strconv.Itoa(p.ContainerPort))
	builder.WriteString("/" + p.Protocol)
	return builder.String()
}
