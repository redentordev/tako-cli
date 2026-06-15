package config

import (
	"fmt"
	"strings"
)

// ResolvePlacementTargets returns the ordered node set a service may use after
// applying placement strategy and supported node label constraints.
func ResolvePlacementTargets(placement *PlacementConfig, servers map[string]ServerConfig, environmentServers []string, environment string) ([]string, error) {
	targets := append([]string(nil), environmentServers...)
	if len(targets) == 0 {
		return nil, fmt.Errorf("environment %s has no target servers", environment)
	}

	if placement != nil {
		strategy := strings.TrimSpace(placement.Strategy)
		switch strategy {
		case "", "spread", "any", "global":
			if len(placement.Servers) > 0 {
				return nil, fmt.Errorf("placement servers require strategy pinned")
			}
		case "pinned":
			if len(placement.Servers) == 0 {
				return nil, fmt.Errorf("pinned placement requires servers")
			}
			targets = append([]string(nil), placement.Servers...)
		default:
			return nil, fmt.Errorf("unknown placement strategy: %s", placement.Strategy)
		}
		if len(placement.Preferences) > 0 {
			return nil, fmt.Errorf("placement preferences are not supported yet")
		}
	}

	if err := validatePlacementTargets(targets, servers, environmentServers, environment); err != nil {
		return nil, err
	}
	if placement == nil || len(placement.Constraints) == 0 {
		return targets, nil
	}

	filtered := make([]string, 0, len(targets))
	for _, serverName := range targets {
		server := servers[serverName]
		matches, err := serverMatchesPlacementConstraints(server, placement.Constraints)
		if err != nil {
			return nil, err
		}
		if matches {
			filtered = append(filtered, serverName)
		}
	}
	if len(filtered) == 0 {
		return nil, fmt.Errorf("placement constraints match no servers in environment %s", environment)
	}
	return filtered, nil
}

func validatePlacementTargets(targets []string, servers map[string]ServerConfig, environmentServers []string, environment string) error {
	allowed := make(map[string]bool, len(environmentServers))
	for _, serverName := range environmentServers {
		allowed[serverName] = true
	}
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		if seen[target] {
			return fmt.Errorf("placement target %s is listed more than once", target)
		}
		seen[target] = true
		if !allowed[target] {
			return fmt.Errorf("placement target %s is outside the selected takod node set for environment %s", target, environment)
		}
		if _, ok := servers[target]; !ok {
			return fmt.Errorf("placement target %s is not defined in servers", target)
		}
	}
	return nil
}

func serverMatchesPlacementConstraints(server ServerConfig, constraints []string) (bool, error) {
	for _, constraint := range constraints {
		key, want, err := parsePlacementConstraint(constraint)
		if err != nil {
			return false, err
		}
		if server.Labels[key] != want {
			return false, nil
		}
	}
	return true, nil
}

func parsePlacementConstraint(constraint string) (string, string, error) {
	original := constraint
	constraint = strings.TrimSpace(constraint)
	if constraint == "" {
		return "", "", fmt.Errorf("placement constraint must not be empty")
	}
	parts := strings.Split(constraint, "==")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("unsupported placement constraint %q; supported form is node.labels.<key>==<value>", original)
	}
	left := strings.TrimSpace(parts[0])
	value := trimPlacementConstraintValue(strings.TrimSpace(parts[1]))
	const prefix = "node.labels."
	if !strings.HasPrefix(left, prefix) {
		return "", "", fmt.Errorf("unsupported placement constraint %q; supported form is node.labels.<key>==<value>", original)
	}
	key := strings.TrimSpace(strings.TrimPrefix(left, prefix))
	if key == "" || value == "" {
		return "", "", fmt.Errorf("placement constraint %q must include a label key and value", original)
	}
	if hasControlChars(key) || hasControlChars(value) {
		return "", "", fmt.Errorf("placement constraint %q contains unsupported characters", original)
	}
	return key, value, nil
}

func trimPlacementConstraintValue(value string) string {
	if len(value) < 2 {
		return value
	}
	if (value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"') {
		return value[1 : len(value)-1]
	}
	return value
}

func ValidatePlacementConfig(placement *PlacementConfig) error {
	if placement == nil {
		return nil
	}
	strategy := strings.TrimSpace(placement.Strategy)
	switch strategy {
	case "", "spread", "any", "global":
		if len(placement.Servers) > 0 {
			return fmt.Errorf("placement servers require strategy pinned")
		}
	case "pinned":
		if len(placement.Servers) == 0 {
			return fmt.Errorf("pinned placement requires servers")
		}
	default:
		return fmt.Errorf("unknown placement strategy: %s", placement.Strategy)
	}
	if len(placement.Preferences) > 0 {
		return fmt.Errorf("placement preferences are not supported yet")
	}
	for _, constraint := range placement.Constraints {
		if _, _, err := parsePlacementConstraint(constraint); err != nil {
			return err
		}
	}
	return nil
}

func hasControlChars(value string) bool {
	for _, r := range value {
		if r < 32 || r == 127 {
			return true
		}
	}
	return false
}
