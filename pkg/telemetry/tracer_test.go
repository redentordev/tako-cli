package telemetry

import "testing"

func TestDefaultConfigOTLPInsecureOptIn(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "")
	if DefaultConfig().OTLPInsecure {
		t.Fatalf("OTLPInsecure should default to false")
	}

	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	if !DefaultConfig().OTLPInsecure {
		t.Fatalf("OTLPInsecure should be enabled by explicit env opt-in")
	}
}
