package takod

import "testing"

func TestValidateCleanupRequest(t *testing.T) {
	valid := CleanupRequest{
		Project:     "demo-app",
		Environment: "production_1",
		ProxyFiles:  []string{"demo-production.yml"},
	}
	if err := validateCleanupRequest(valid); err != nil {
		t.Fatalf("valid cleanup request returned error: %v", err)
	}

	invalid := valid
	invalid.Project = "../demo"
	if err := validateCleanupRequest(invalid); err == nil {
		t.Fatalf("expected unsafe project to be rejected")
	}

	invalid = valid
	invalid.Environment = "prod;rm"
	if err := validateCleanupRequest(invalid); err == nil {
		t.Fatalf("expected unsafe environment to be rejected")
	}

	invalid = valid
	invalid.ProxyFiles = []string{"../demo.yml"}
	if err := validateCleanupRequest(invalid); err == nil {
		t.Fatalf("expected unsafe proxy file to be rejected")
	}
}

func TestImageRepositoryMatchesProject(t *testing.T) {
	for _, repo := range []string{
		"demo/web",
		"demo",
		"registry.example.com/demo/web",
		"localhost:5000/demo/web",
	} {
		if !imageRepositoryMatchesProject(repo, "demo") {
			t.Fatalf("expected repository %q to match project", repo)
		}
	}

	for _, repo := range []string{
		"demo-app/web",
		"company/demo-web",
		"registry.example.com/notdemo/web",
	} {
		if imageRepositoryMatchesProject(repo, "demo") {
			t.Fatalf("expected repository %q not to match project", repo)
		}
	}
}
