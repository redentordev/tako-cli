package alerttarget

import (
	"net"
	"testing"
)

func TestNormalizeAndValidate(t *testing.T) {
	value, err := NormalizeAndValidate("webhook", "  https://alerts.example/token  ")
	if err != nil || value != "https://alerts.example/token" {
		t.Fatalf("NormalizeAndValidate() = %q, %v", value, err)
	}
	for _, target := range []string{"https://127.0.0.1/x", "https://100.64.0.1/x", "https://192.0.2.1/x", "http://alerts.example/x"} {
		if _, err := NormalizeAndValidate("webhook", target); err == nil {
			t.Fatalf("target %q accepted", target)
		}
	}
	if _, err := NormalizeAndValidate("slack", "https://alerts.example/token"); err == nil {
		t.Fatal("unofficial Slack target accepted")
	}
}

func TestIsPublicIP(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "100.64.0.1", "169.254.169.254", "64:ff9b::a9fe:a9fe", "2002:a9fe:a9fe::1"} {
		if IsPublicIP(net.ParseIP(value)) {
			t.Fatalf("non-public address %s accepted", value)
		}
	}
	for _, value := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if !IsPublicIP(net.ParseIP(value)) {
			t.Fatalf("public address %s rejected", value)
		}
	}
}
