package takod

import "testing"

func TestParseActualState(t *testing.T) {
	output := `
demo_production_web_1|registry.example.com/demo/web:abc|container-a
demo_production_web_2|registry.example.com/demo/web:abc|container-b
demo_production_api_v2_3|registry.example.com/demo/api:def|container-c
other_production_web_1|ignored|container-d
malformed
`

	actual := ParseActualState("demo", "production", output)
	if len(actual.Services) != 2 {
		t.Fatalf("expected two services, got %#v", actual.Services)
	}
	if got := actual.Services["web"].Replicas; got != 2 {
		t.Fatalf("expected web replicas to be 2, got %d", got)
	}
	if got := actual.Services["api_v2"].Replicas; got != 1 {
		t.Fatalf("expected api_v2 replicas to be 1, got %d", got)
	}
	if actual.Services["web"].Containers[1] != "container-b" {
		t.Fatalf("unexpected web containers: %#v", actual.Services["web"].Containers)
	}
}
