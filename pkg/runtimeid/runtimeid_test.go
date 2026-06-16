package runtimeid

import (
	"strings"
	"testing"
)

func TestContainerNameAvoidsAmbiguousStageServiceCollision(t *testing.T) {
	left := ContainerName("demo", "prod_api", "web", 1)
	right := ContainerName("demo", "prod", "api_web", 1)
	if left == right {
		t.Fatalf("container names collided: %q", left)
	}
	for _, name := range []string{left, right} {
		if !strings.HasPrefix(name, "tako_") {
			t.Fatalf("container name %q should be tako-prefixed", name)
		}
		if len(name) > dockerNameMax {
			t.Fatalf("container name length = %d, want <= %d", len(name), dockerNameMax)
		}
	}
}

func TestContainerAliasIsDNSSafeAndCollisionResistant(t *testing.T) {
	left := ContainerAlias("demo", "prod_api", "web", 1)
	right := ContainerAlias("demo", "prod", "api_web", 1)
	if left == right {
		t.Fatalf("container aliases collided: %q", left)
	}
	for _, alias := range []string{left, right} {
		if strings.Contains(alias, "_") {
			t.Fatalf("container alias %q should not contain underscores", alias)
		}
		if !strings.HasPrefix(alias, "tako-") {
			t.Fatalf("container alias %q should be tako-prefixed", alias)
		}
		if len(alias) > dockerNetworkMax {
			t.Fatalf("container alias length = %d, want <= %d", len(alias), dockerNetworkMax)
		}
	}
}

func TestServiceIdentityAvoidsAmbiguousStageServiceCollision(t *testing.T) {
	left := ServiceIdentity("demo", "prod_api", "web")
	right := ServiceIdentity("demo", "prod", "api_web")
	if left == right {
		t.Fatalf("service identities collided: %q", left)
	}
	if len(left) != 10 || len(right) != 10 {
		t.Fatalf("service identity should use short hash values, got %q %q", left, right)
	}
}

func TestProxyConfigFileNameIncludesAppStageIdentity(t *testing.T) {
	left := ProxyConfigFileName("demo-api", "production")
	right := ProxyConfigFileName("demo", "api-production")
	if left == right {
		t.Fatalf("proxy config names collided: %q", left)
	}
	if !strings.HasSuffix(left, ".yml") || !strings.HasSuffix(right, ".yml") {
		t.Fatalf("proxy config names should end in .yml: %q %q", left, right)
	}
}

func TestNetworkNameFitsTakodRuntimeValidationLimit(t *testing.T) {
	name := NetworkName("very-long-project-name-with-enough-characters-to-require-truncation", "production")
	if len(name) > dockerNetworkMax {
		t.Fatalf("network name length = %d, want <= %d: %q", len(name), dockerNetworkMax, name)
	}
	if !strings.HasPrefix(name, "tako_") {
		t.Fatalf("network name %q should be tako-prefixed", name)
	}
}

func TestNetworkProjectPrefixMatchesSanitizedProjectName(t *testing.T) {
	name := NetworkName("demo-app", "production")
	prefix := NetworkProjectPrefix("demo-app")
	if !strings.HasPrefix(name, prefix) {
		t.Fatalf("network name %q should start with project prefix %q", name, prefix)
	}
}

func TestVolumePrefixesMatchSanitizedRuntimeNames(t *testing.T) {
	name := VolumeName("demo-app", "production_1", "cache")
	projectPrefix := VolumeProjectPrefix("demo-app")
	environmentPrefix := VolumeEnvironmentPrefix("demo-app", "production_1")
	if !strings.HasPrefix(name, projectPrefix) {
		t.Fatalf("volume name %q should start with project prefix %q", name, projectPrefix)
	}
	if !strings.HasPrefix(name, environmentPrefix) {
		t.Fatalf("volume name %q should start with environment prefix %q", name, environmentPrefix)
	}
}
