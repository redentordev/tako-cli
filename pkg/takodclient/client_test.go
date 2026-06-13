package takodclient

import "testing"

func TestProxyFileEndpointEscapesName(t *testing.T) {
	got := ProxyFileEndpoint("demo production.yml")
	want := "/v1/proxy-file?name=demo+production.yml"
	if got != want {
		t.Fatalf("ProxyFileEndpoint() = %q, want %q", got, want)
	}
}
