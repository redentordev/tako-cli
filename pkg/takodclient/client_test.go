package takodclient

import "testing"

func TestProxyFileEndpointEscapesName(t *testing.T) {
	got := ProxyFileEndpoint("demo production.yml")
	want := "/v1/proxy-file?name=demo+production.yml"
	if got != want {
		t.Fatalf("ProxyFileEndpoint() = %q, want %q", got, want)
	}
}

func TestStateEndpointEscapesQueryValues(t *testing.T) {
	got := StateEndpoint("demo app", "prod/us", "desired")
	want := "/v1/state?document=desired&environment=prod%2Fus&project=demo+app"
	if got != want {
		t.Fatalf("StateEndpoint() = %q, want %q", got, want)
	}
}

func TestEnvBundleEndpointEscapesQueryValues(t *testing.T) {
	got := EnvBundleEndpoint("demo app", "prod/us")
	want := "/v1/env-bundle?environment=prod%2Fus&project=demo+app"
	if got != want {
		t.Fatalf("EnvBundleEndpoint() = %q, want %q", got, want)
	}
}
