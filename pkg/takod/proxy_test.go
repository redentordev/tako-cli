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
	req := ReconcileProxyRequest{
		Network: "tako_demo_production",
		Email:   "ops@example.com",
		Image:   "traefik:v3.6.1",
	}
	args := currentProxyArgsJSON(req.Email)
	ports := currentProxyPortsJSON()
	mounts := currentProxyMountsJSON()

	if !proxyContainerIsCurrent(req, args, ports, req.Image, mounts) {
		t.Fatal("expected proxy with dynamic provider and HTTP/3 UDP publish to be current")
	}
	if proxyContainerIsCurrent(req, `["--providers.file.directory=/etc/traefik/dynamic"]`, ports, req.Image, mounts) {
		t.Fatal("expected proxy without HTTP/3 entrypoint to require replacement")
	}
	if proxyContainerIsCurrent(req, args, `{"80/tcp":[{"HostPort":"80"}],"443/tcp":[{"HostPort":"443"}]}`, req.Image, mounts) {
		t.Fatal("expected proxy without UDP 443 publish to require replacement")
	}
}

func TestProxyContainerIsCurrentRejectsStaleImageEmailAndMounts(t *testing.T) {
	req := ReconcileProxyRequest{
		Network: "tako_demo_production",
		Email:   "ops@example.com",
		Image:   "traefik:v3.6.1",
	}
	args := currentProxyArgsJSON(req.Email)
	ports := currentProxyPortsJSON()
	mounts := currentProxyMountsJSON()

	tests := []struct {
		name   string
		args   string
		image  string
		mounts string
	}{
		{
			name:   "stale image",
			args:   args,
			image:  "traefik:v3.5.0",
			mounts: mounts,
		},
		{
			name:   "stale acme email",
			args:   currentProxyArgsJSON("old@example.com"),
			image:  req.Image,
			mounts: mounts,
		},
		{
			name:   "missing access log mount",
			args:   args,
			image:  req.Image,
			mounts: `[{"Source":"/etc/tako/proxy/acme","Destination":"/acme"},{"Source":"/etc/tako/proxy/dynamic","Destination":"/etc/traefik/dynamic"}]`,
		},
		{
			name:   "wrong dynamic mount source",
			args:   args,
			image:  req.Image,
			mounts: `[{"Source":"/etc/tako/proxy/acme","Destination":"/acme"},{"Source":"/tmp/proxy/dynamic","Destination":"/etc/traefik/dynamic"},{"Source":"/var/log/tako/proxy","Destination":"/var/log/traefik"}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if proxyContainerIsCurrent(req, tt.args, ports, tt.image, tt.mounts) {
				t.Fatal("expected stale proxy container to require replacement")
			}
		})
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

func currentProxyArgsJSON(email string) string {
	return `[` +
		`"--api.dashboard=false",` +
		`"--providers.file.directory=/etc/traefik/dynamic",` +
		`"--providers.file.watch=true",` +
		`"--entryPoints.web.address=:80",` +
		`"--entryPoints.websecure.address=:443",` +
		`"--entryPoints.websecure.http3=true",` +
		`"--entryPoints.websecure.http3.advertisedPort=443",` +
		`"--certificatesResolvers.letsencrypt.acme.email=` + email + `",` +
		`"--certificatesResolvers.letsencrypt.acme.storage=/acme/acme.json",` +
		`"--certificatesResolvers.letsencrypt.acme.httpChallenge.entryPoint=web",` +
		`"--log.level=INFO",` +
		`"--accessLog.filePath=/var/log/traefik/access.log",` +
		`"--accessLog.format=json"` +
		`]`
}

func currentProxyPortsJSON() string {
	return `{"80/tcp":[{"HostPort":"80"}],"443/tcp":[{"HostPort":"443"}],"443/udp":[{"HostPort":"443"}]}`
}

func currentProxyMountsJSON() string {
	return `[` +
		`{"Source":"/etc/tako/proxy/acme","Destination":"/acme"},` +
		`{"Source":"/etc/tako/proxy/dynamic","Destination":"/etc/traefik/dynamic"},` +
		`{"Source":"/var/log/tako/proxy","Destination":"/var/log/traefik"}` +
		`]`
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
