package takodstate

import (
	"reflect"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
)

// TestPersistedDesiredStateCarriesNoRegistryAuths pins that the desired
// state the CLI writes to takod can never hold request-scoped registry
// credentials (minted provider tokens included): no field has the
// takod.RegistryAuth type or a registryAuths JSON name.
func TestPersistedDesiredStateCarriesNoRegistryAuths(t *testing.T) {
	for _, value := range []any{DesiredService{}, DesiredBuild{}, DesiredRevision{}, DesiredVolume{}, takoapi.DesiredStateDocument{}} {
		assertNoRegistryAuthFields(t, reflect.TypeOf(value), reflect.TypeOf(value).Name(), map[reflect.Type]bool{})
	}
}

func assertNoRegistryAuthFields(t *testing.T, typ reflect.Type, path string, seen map[reflect.Type]bool) {
	t.Helper()
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array || typ.Kind() == reflect.Map {
		typ = typ.Elem()
	}
	if typ == reflect.TypeOf(takod.RegistryAuth{}) {
		t.Fatalf("%s persists takod.RegistryAuth", path)
	}
	if typ.Kind() != reflect.Struct || seen[typ] {
		return
	}
	seen[typ] = true
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name := strings.ToLower(strings.Split(field.Tag.Get("json"), ",")[0])
		if strings.Contains(name, "registryauth") || strings.Contains(strings.ToLower(field.Name), "registryauth") {
			t.Fatalf("%s.%s persists registry credentials", path, field.Name)
		}
		assertNoRegistryAuthFields(t, field.Type, path+"."+field.Name, seen)
	}
}
