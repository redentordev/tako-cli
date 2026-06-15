package serviceimport

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

type PortSpec struct {
	Name     string
	Target   int
	Protocol string
}

type Row struct {
	Node      string
	Service   string
	Port      PortSpec
	Container string
	Host      string
	Address   string
}

type ResolvedExport struct {
	Target   int
	Protocol string
}

func ServerNames(cfg *config.Config, envName string, importConfig config.ImportConfig, requestedServer string) ([]string, error) {
	base := append([]string(nil), importConfig.Servers...)
	if len(base) == 0 {
		serverNames, err := cfg.GetEnvironmentServers(envName)
		if err != nil {
			return nil, fmt.Errorf("failed to get environment servers: %w", err)
		}
		base = serverNames
	}
	if len(base) == 0 {
		return nil, fmt.Errorf("no servers configured for import %s/%s/%s:%s", importConfig.Project, importConfig.Environment, importConfig.Service, importConfig.Port)
	}
	if requestedServer == "" {
		return base, nil
	}
	for _, serverName := range base {
		if serverName == requestedServer {
			return []string{requestedServer}, nil
		}
	}
	return nil, fmt.Errorf("server %s is not configured for import %s/%s/%s:%s", requestedServer, importConfig.Project, importConfig.Environment, importConfig.Service, importConfig.Port)
}

func ResolveExport(client takodclient.RequestExecutor, socket string, alias string, importConfig config.ImportConfig) (ResolvedExport, error) {
	output, err := takodclient.RequestJSON(client, socket, "GET", takodclient.StateEndpoint(importConfig.Project, importConfig.Environment, "desired"), nil)
	if err != nil {
		return ResolvedExport{}, fmt.Errorf("failed to read desired state for import %s: %w", alias, err)
	}
	var response takod.StateDocumentResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return ResolvedExport{}, fmt.Errorf("failed to parse desired state response for import %s: %w", alias, err)
	}
	if !response.Found || strings.TrimSpace(response.Content) == "" {
		return ResolvedExport{}, fmt.Errorf("exporting project state not found for import %s", alias)
	}
	var desired takodstate.DesiredRevision
	if err := json.Unmarshal([]byte(response.Content), &desired); err != nil {
		return ResolvedExport{}, fmt.Errorf("failed to parse desired state for import %s: %w", alias, err)
	}
	if desired.Project != importConfig.Project || desired.Environment != importConfig.Environment {
		return ResolvedExport{}, fmt.Errorf("desired state mismatch for import %s", alias)
	}
	for _, record := range desired.Exports {
		if record.Project == importConfig.Project &&
			record.Environment == importConfig.Environment &&
			record.Service == importConfig.Service &&
			record.Port == importConfig.Port {
			if record.Target < 1 || record.Target > 65535 {
				return ResolvedExport{}, fmt.Errorf("invalid exported target for import %s", alias)
			}
			return ResolvedExport{Target: record.Target, Protocol: record.Protocol}, nil
		}
	}
	return ResolvedExport{}, fmt.Errorf("export %s/%s/%s:%s not found for import %s", importConfig.Project, importConfig.Environment, importConfig.Service, importConfig.Port, alias)
}

func RowsFromResponse(
	serverName string,
	response takod.DiscoveryResponse,
	project string,
	environment string,
	serviceName string,
	port PortSpec,
) ([]Row, error) {
	if response.Project != project {
		return nil, fmt.Errorf("project mismatch")
	}
	if response.Environment != environment {
		return nil, fmt.Errorf("environment mismatch")
	}
	node := response.Node
	if strings.TrimSpace(node) == "" {
		node = serverName
	}

	var rows []Row
	for _, endpoint := range response.Endpoints {
		if endpoint.Service != serviceName {
			return nil, fmt.Errorf("service mismatch")
		}
		endpoint, err := ValidateEndpoint(endpoint, serviceName, port.Target)
		if err != nil {
			return nil, err
		}
		rows = append(rows, Row{
			Node:      node,
			Service:   endpoint.Service,
			Port:      port,
			Container: endpoint.Container,
			Host:      endpoint.Host,
			Address:   EndpointAddress(endpoint),
		})
	}
	return rows, nil
}

func ValidateEndpoint(endpoint takod.DiscoveryEndpoint, serviceName string, port int) (takod.DiscoveryEndpoint, error) {
	if endpoint.Service != serviceName {
		return takod.DiscoveryEndpoint{}, fmt.Errorf("service mismatch")
	}
	if strings.TrimSpace(endpoint.Container) == "" {
		return takod.DiscoveryEndpoint{}, fmt.Errorf("container is required")
	}
	if strings.TrimSpace(endpoint.ContainerID) == "" {
		return takod.DiscoveryEndpoint{}, fmt.Errorf("container id is required")
	}
	switch endpoint.Scope {
	case "local", "mesh":
	default:
		return takod.DiscoveryEndpoint{}, fmt.Errorf("unsupported endpoint scope")
	}
	if !endpoint.Healthy {
		return takod.DiscoveryEndpoint{}, fmt.Errorf("endpoint is not healthy")
	}
	if endpoint.Port != port {
		return takod.DiscoveryEndpoint{}, fmt.Errorf("port mismatch")
	}
	ip := net.ParseIP(endpoint.Host)
	if ip == nil {
		return takod.DiscoveryEndpoint{}, fmt.Errorf("host must be an IP address")
	}
	if !ip.IsPrivate() {
		return takod.DiscoveryEndpoint{}, fmt.Errorf("host must be a private container IP")
	}
	if port > 0 && endpoint.Scope == "local" {
		expectedAddress := net.JoinHostPort(endpoint.Host, strconv.Itoa(port))
		if endpoint.Address != expectedAddress {
			return takod.DiscoveryEndpoint{}, fmt.Errorf("address mismatch")
		}
		endpoint.Address = expectedAddress
	} else if port > 0 && endpoint.Scope == "mesh" {
		host, portValue, err := net.SplitHostPort(endpoint.Address)
		if err != nil {
			return takod.DiscoveryEndpoint{}, fmt.Errorf("address mismatch")
		}
		if host != endpoint.Host {
			return takod.DiscoveryEndpoint{}, fmt.Errorf("address mismatch")
		}
		publishedPort, err := strconv.Atoi(portValue)
		if err != nil || publishedPort < 1 || publishedPort > 65535 {
			return takod.DiscoveryEndpoint{}, fmt.Errorf("address mismatch")
		}
	} else if endpoint.Address != "" {
		return takod.DiscoveryEndpoint{}, fmt.Errorf("address must be empty when port is omitted")
	}
	return endpoint, nil
}

func EndpointAddress(endpoint takod.DiscoveryEndpoint) string {
	if endpoint.Address != "" {
		return endpoint.Address
	}
	return endpoint.Host
}

func RowUpstream(row Row) string {
	address := strings.TrimSpace(row.Address)
	if address == "" {
		return ""
	}
	switch row.Port.Protocol {
	case "https":
		return "https://" + address
	default:
		return "http://" + address
	}
}

func RowsUpstreams(rows []Row) []string {
	seen := make(map[string]bool, len(rows))
	upstreams := make([]string, 0, len(rows))
	for _, row := range rows {
		upstream := RowUpstream(row)
		if upstream == "" || seen[upstream] {
			continue
		}
		seen[upstream] = true
		upstreams = append(upstreams, upstream)
	}
	sort.Strings(upstreams)
	return upstreams
}
