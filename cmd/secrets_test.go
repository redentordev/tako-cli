package cmd

import "testing"

func TestImportedSecretKeyStripsPrefixAndNormalizesPath(t *testing.T) {
	got := importedSecretKey("/truenextglobal/production/twenty/database-url", "/truenextglobal/production/")
	if got != "TWENTY_DATABASE_URL" {
		t.Fatalf("importedSecretKey = %q, want TWENTY_DATABASE_URL", got)
	}
}

func TestMapImportedSecretsUsesExplicitMappingsAndRejectsCollisions(t *testing.T) {
	values := map[string]string{
		"/app/prod/db/url":      "one",
		"/app/prod/db-url":      "two",
		"/app/prod/redis/url":   "three",
		"/app/prod/custom/name": "four",
	}

	_, err := mapImportedSecrets(values, "/app/prod/", nil)
	if err == nil {
		t.Fatal("expected collision without explicit mapping")
	}

	got, err := mapImportedSecrets(values, "/app/prod/", map[string]string{
		"/app/prod/db-url":      "DATABASE_URL_ALT",
		"/app/prod/custom/name": "CUSTOM_NAME",
	})
	if err != nil {
		t.Fatalf("mapImportedSecrets returned error: %v", err)
	}
	if got["DB_URL"] != "one" || got["DATABASE_URL_ALT"] != "two" || got["REDIS_URL"] != "three" || got["CUSTOM_NAME"] != "four" {
		t.Fatalf("mapped secrets = %#v", got)
	}
}

func TestParseSecretMappingsValidatesDestinationKeys(t *testing.T) {
	if _, err := parseSecretMappings([]string{"/path=DATABASE_URL"}); err != nil {
		t.Fatalf("parseSecretMappings returned error: %v", err)
	}
	if _, err := parseSecretMappings([]string{"/path=bad-key"}); err == nil {
		t.Fatal("expected invalid destination key to fail")
	}
}
