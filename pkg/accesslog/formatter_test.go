package accesslog

import (
	"strings"
	"testing"
)

func TestFormatterFormatsCaddyAccessLog(t *testing.T) {
	formatter := NewFormatter(false)
	line := `{"level":"info","ts":1813348800.25,"logger":"http.log.access.tako_web","msg":"handled request","request":{"remote_ip":"203.0.113.5","client_ip":"203.0.113.5","proto":"HTTP/2.0","method":"GET","host":"example.com","uri":"/dashboard"},"bytes_read":0,"duration":0.0123,"size":1234,"status":200}`

	got, err := formatter.FormatLine(line)
	if err != nil {
		t.Fatalf("FormatLine returned error: %v", err)
	}
	for _, want := range []string{"GET", "203.0.113.5", "/dashboard", "200"} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted log missing %q: %q", want, got)
		}
	}
}

func TestFormatterVerboseIncludesCaddyHost(t *testing.T) {
	formatter := NewFormatter(true)
	line := `{"ts":1813348800,"request":{"method":"POST","host":"app.example.com","uri":"/api","client_ip":"203.0.113.5"},"duration":0.001,"size":42,"status":201}`

	got, err := formatter.FormatLine(line)
	if err != nil {
		t.Fatalf("FormatLine returned error: %v", err)
	}
	if !strings.Contains(got, "Host:") || !strings.Contains(got, "app.example.com") {
		t.Fatalf("verbose formatted log should include host: %q", got)
	}
}

func TestFormatterFiltersCaddyServiceLogger(t *testing.T) {
	formatter := NewFormatter(false)
	formatter.SetServiceFilter("web")

	webLine := `{"logger":"http.log.access.tako_web","request":{"method":"GET","uri":"/","client_ip":"203.0.113.5"},"status":200}`
	apiLine := `{"logger":"http.log.access.tako_api","request":{"method":"GET","uri":"/","client_ip":"203.0.113.5"},"status":200}`

	got, err := formatter.FormatLine(webLine)
	if err != nil {
		t.Fatalf("FormatLine returned error: %v", err)
	}
	if got == "" {
		t.Fatal("expected web line to pass service filter")
	}
	got, err = formatter.FormatLine(apiLine)
	if err != nil {
		t.Fatalf("FormatLine returned error: %v", err)
	}
	if got != "" {
		t.Fatalf("expected api line to be filtered out, got %q", got)
	}
}
