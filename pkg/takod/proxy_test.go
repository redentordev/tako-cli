package takod

import (
	"reflect"
	"strings"
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
		"--publish", "443:443/udp",
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
		"--entryPoints.websecure.http3=true",
		"--entryPoints.websecure.http3.advertisedPort=443",
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

func TestProxyContainerIsCurrentRequiresHTTP3(t *testing.T) {
	args := `["--providers.file.directory=/etc/traefik/dynamic","--entryPoints.websecure.http3=true"]`
	ports := `{"443/tcp":[{"HostPort":"443"}],"443/udp":[{"HostPort":"443"}]}`

	if !proxyContainerIsCurrent(args, ports) {
		t.Fatal("expected proxy with dynamic provider and HTTP/3 UDP publish to be current")
	}
	if proxyContainerIsCurrent(`["--providers.file.directory=/etc/traefik/dynamic"]`, ports) {
		t.Fatal("expected proxy without HTTP/3 entrypoint to require replacement")
	}
	if proxyContainerIsCurrent(args, `{"443/tcp":[{"HostPort":"443"}]}`) {
		t.Fatal("expected proxy without UDP 443 publish to require replacement")
	}
}

func TestValidateReconcileProxyRequestAllowsDefaults(t *testing.T) {
	req := ReconcileProxyRequest{Network: "tako_demo_production"}

	normalizeReconcileProxyRequest(&req)
	if err := validateReconcileProxyRequest(req); err != nil {
		t.Fatalf("expected defaulted proxy request to validate: %v", err)
	}
	if req.Email != defaultProxyEmail {
		t.Fatalf("email = %q, want %q", req.Email, defaultProxyEmail)
	}
	if req.Image != defaultProxyImage {
		t.Fatalf("image = %q, want %q", req.Image, defaultProxyImage)
	}
}

func TestValidateReconcileProxyRequestRejectsUnsafeValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ReconcileProxyRequest)
		want   string
	}{
		{
			name: "network",
			mutate: func(req *ReconcileProxyRequest) {
				req.Network = "tako;rm"
			},
			want: "invalid network name",
		},
		{
			name: "image",
			mutate: func(req *ReconcileProxyRequest) {
				req.Image = "--help"
			},
			want: "image must not start",
		},
		{
			name: "email",
			mutate: func(req *ReconcileProxyRequest) {
				req.Email = "ops@example.com\nbad"
			},
			want: "invalid proxy email",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := ReconcileProxyRequest{Network: "tako_demo_production"}
			normalizeReconcileProxyRequest(&req)
			tt.mutate(&req)

			err := validateReconcileProxyRequest(req)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}
