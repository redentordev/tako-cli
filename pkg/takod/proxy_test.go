package takod

import (
	"reflect"
	"testing"
)

func TestBuildProxyContainerArgs(t *testing.T) {
	got := buildProxyContainerArgs(ReconcileProxyRequest{
		Network: "tako_demo_production",
		Email:   "ops@example.com",
		Image:   "traefik:v3.6.1",
	})
	want := []string{
		"run", "-d",
		"--name", "tako-proxy",
		"--restart", "unless-stopped",
		"--network", "tako_demo_production",
		"--publish", "80:80",
		"--publish", "443:443",
		"--volume", "/etc/tako/proxy/acme:/acme",
		"--volume", "/etc/tako/proxy/dynamic:/etc/traefik/dynamic:ro",
		"--volume", "/var/log/tako/proxy:/var/log/traefik",
		"--label", "tako.runtime=takod",
		"--label", "tako.component=proxy",
		"traefik:v3.6.1",
		"--api.dashboard=false",
		"--providers.file.directory=/etc/traefik/dynamic",
		"--providers.file.watch=true",
		"--entryPoints.web.address=:80",
		"--entryPoints.websecure.address=:443",
		"--certificatesResolvers.letsencrypt.acme.email=ops@example.com",
		"--certificatesResolvers.letsencrypt.acme.storage=/acme/acme.json",
		"--certificatesResolvers.letsencrypt.acme.httpChallenge.entryPoint=web",
		"--log.level=INFO",
		"--accessLog.filePath=/var/log/traefik/access.log",
		"--accessLog.format=json",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected proxy args:\ngot:  %#v\nwant: %#v", got, want)
	}
}
