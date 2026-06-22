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

func TestRevisionContainerIDsAreRevisionScopedAndDNSSafe(t *testing.T) {
	stableName := ContainerName("demo", "production", "web", 1)
	blueName := RevisionContainerName("demo", "production", "web", "rev-blue", 1)
	greenName := RevisionContainerName("demo", "production", "web", "rev-green", 1)
	if blueName == stableName {
		t.Fatalf("revision container name %q should differ from stable name %q", blueName, stableName)
	}
	if blueName == greenName {
		t.Fatalf("revision container names collided across revisions: %q", blueName)
	}
	if !strings.Contains(blueName, "_r_") {
		t.Fatalf("revision container name %q should include revision marker", blueName)
	}
	if len(blueName) > dockerNameMax {
		t.Fatalf("revision container name length = %d, want <= %d: %q", len(blueName), dockerNameMax, blueName)
	}

	stableAlias := ContainerAlias("demo", "production", "web", 1)
	blueAlias := RevisionContainerAlias("demo", "production", "web", "rev-blue", 1)
	greenAlias := RevisionContainerAlias("demo", "production", "web", "rev-green", 1)
	if blueAlias == stableAlias {
		t.Fatalf("revision container alias %q should differ from stable alias %q", blueAlias, stableAlias)
	}
	if blueAlias == greenAlias {
		t.Fatalf("revision container aliases collided across revisions: %q", blueAlias)
	}
	if strings.Contains(blueAlias, "_") {
		t.Fatalf("revision container alias %q should not contain underscores", blueAlias)
	}
	if !strings.Contains(blueAlias, "-r-") {
		t.Fatalf("revision container alias %q should include revision marker", blueAlias)
	}
	if len(blueAlias) > dockerNetworkMax {
		t.Fatalf("revision container alias length = %d, want <= %d: %q", len(blueAlias), dockerNetworkMax, blueAlias)
	}
}

func TestExportAliasIsReadableAndDNSSafe(t *testing.T) {
	alias := ExportAlias("backend-api", "production", "api")
	if alias != "backend-api-production-api" {
		t.Fatalf("export alias = %q, want readable project-environment-service alias", alias)
	}
	if strings.Contains(alias, "_") {
		t.Fatalf("export alias %q should not contain underscores", alias)
	}

	longAlias := ExportAlias("very-long-provider-project-name", "production", "very-long-service-name")
	if len(longAlias) > dockerNetworkMax {
		t.Fatalf("export alias length = %d, want <= %d: %q", len(longAlias), dockerNetworkMax, longAlias)
	}
}

func TestExportNetworkNameIsServiceScoped(t *testing.T) {
	api := ExportNetworkName("backend-api", "production", "api")
	db := ExportNetworkName("backend-api", "production", "database")
	if api == db {
		t.Fatalf("export networks should be service-scoped: %q", api)
	}
	if !strings.Contains(api, "export") {
		t.Fatalf("export network should include export marker: %q", api)
	}
	if len(api) > dockerNetworkMax {
		t.Fatalf("export network length = %d, want <= %d: %q", len(api), dockerNetworkMax, api)
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
	if !strings.HasSuffix(left, ".json") || !strings.HasSuffix(right, ".json") {
		t.Fatalf("proxy config names should end in .json: %q %q", left, right)
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

func TestNetworkEnvironmentPrefixMatchesSanitizedAppStage(t *testing.T) {
	name := ExportNetworkName("demo-app", "production_1", "api")
	prefix := NetworkEnvironmentPrefix("demo-app", "production_1")
	if !strings.HasPrefix(name, prefix) {
		t.Fatalf("network name %q should start with environment prefix %q", name, prefix)
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
