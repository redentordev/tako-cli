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

func TestActualStateEndpointEscapesQueryValues(t *testing.T) {
	got := ActualStateEndpoint("demo app", "prod/us")
	want := "/v1/actual?environment=prod%2Fus&project=demo+app"
	if got != want {
		t.Fatalf("ActualStateEndpoint() = %q, want %q", got, want)
	}
}

func TestEnvBundleEndpointEscapesQueryValues(t *testing.T) {
	got := EnvBundleEndpoint("demo app", "prod/us")
	want := "/v1/env-bundle?environment=prod%2Fus&project=demo+app"
	if got != want {
		t.Fatalf("EnvBundleEndpoint() = %q, want %q", got, want)
	}
}

func TestBackupsEndpointEscapesQueryValues(t *testing.T) {
	got := BackupsEndpoint("demo app", "prod/us", "db data", "20260613-120000")
	want := "/v1/backups?backupId=20260613-120000&environment=prod%2Fus&project=demo+app&volume=db+data"
	if got != want {
		t.Fatalf("BackupsEndpoint() = %q, want %q", got, want)
	}
}

func TestImageBuildEndpointEscapesImage(t *testing.T) {
	got := ImageBuildEndpoint("demo/web:abc123")
	want := "/v1/images/build?image=demo%2Fweb%3Aabc123"
	if got != want {
		t.Fatalf("ImageBuildEndpoint() = %q, want %q", got, want)
	}
}

func TestLogsEndpointEscapesQueryValues(t *testing.T) {
	got := LogsEndpoint("demo app", "prod/us", "web api", 250, true)
	want := "/v1/logs?environment=prod%2Fus&follow=true&project=demo+app&service=web+api&tail=250"
	if got != want {
		t.Fatalf("LogsEndpoint() = %q, want %q", got, want)
	}
}

func TestStatsEndpointEscapesQueryValues(t *testing.T) {
	got := StatsEndpoint("demo app", "prod/us", "web api", true)
	want := "/v1/stats?all=true&environment=prod%2Fus&project=demo+app&service=web+api"
	if got != want {
		t.Fatalf("StatsEndpoint() = %q, want %q", got, want)
	}
}

func TestMetricsEndpointWithCollect(t *testing.T) {
	got := MetricsEndpoint(true)
	want := "/v1/metrics?collect=true"
	if got != want {
		t.Fatalf("MetricsEndpoint() = %q, want %q", got, want)
	}
}

func TestAccessLogsEndpointWithFollow(t *testing.T) {
	got := AccessLogsEndpoint(75, true)
	want := "/v1/access-logs?follow=true&tail=75"
	if got != want {
		t.Fatalf("AccessLogsEndpoint() = %q, want %q", got, want)
	}
}
