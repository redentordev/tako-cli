package takod

import (
	"reflect"
	"strings"
	"testing"
)

// TestPersistedNodeStateCarriesNoRegistryAuths pins that the snapshots and
// job specs takod writes to disk never include request-scoped registry
// credentials; those exist only in the ephemeral DOCKER_CONFIG of one call.
func TestPersistedNodeStateCarriesNoRegistryAuths(t *testing.T) {
	for _, value := range []any{persistedActualSnapshot{}, JobSpec{}} {
		typ := reflect.TypeOf(value)
		assertNoPersistedRegistryAuths(t, typ, typ.Name(), map[reflect.Type]bool{})
	}
}

func assertNoPersistedRegistryAuths(t *testing.T, typ reflect.Type, path string, seen map[reflect.Type]bool) {
	t.Helper()
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array || typ.Kind() == reflect.Map {
		typ = typ.Elem()
	}
	if typ == reflect.TypeOf(RegistryAuth{}) {
		t.Fatalf("%s persists RegistryAuth", path)
	}
	if typ.Kind() != reflect.Struct || seen[typ] {
		return
	}
	seen[typ] = true
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name := strings.ToLower(strings.Split(field.Tag.Get("json"), ",")[0])
		if strings.Contains(name, "registryauth") {
			t.Fatalf("%s.%s persists registry credentials", path, field.Name)
		}
		assertNoPersistedRegistryAuths(t, field.Type, path+"."+field.Name, seen)
	}
}
