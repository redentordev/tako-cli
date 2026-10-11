package engine

import (
	"encoding/json"
	"testing"
)

func TestAliasListResultJSONFieldNames(t *testing.T) {
	result := AliasListResult{
		APIVersion: "tako.redentor.dev/v1alpha1",
		Kind:       KindAliasList,
		ConfigPath: "tako.yaml",
		Aliases: []AliasEntry{
			{Name: "console", Args: []string{"exec", "-it", "web", "--", "sh"}, Form: "list", Target: "exec"},
			{Name: "logs", Args: []string{"logs", "web"}, Form: "string", Target: "logs", Shadowed: true, ShadowedBy: "logs"},
		},
		Findings: []ValidateFinding{{Severity: ValidateSeverityWarn, Path: "tako.yaml", Field: "aliases.logs", Message: "shadowed"}},
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "AliasList",
  "configPath": "tako.yaml",
  "aliases": [
    {
      "name": "console",
      "args": [
        "exec",
        "-it",
        "web",
        "--",
        "sh"
      ],
      "form": "list",
      "target": "exec",
      "shadowed": false
    },
    {
      "name": "logs",
      "args": [
        "logs",
        "web"
      ],
      "form": "string",
      "target": "logs",
      "shadowed": true,
      "shadowedBy": "logs"
    }
  ],
  "findings": [
    {
      "severity": "warning",
      "path": "tako.yaml",
      "field": "aliases.logs",
      "message": "shadowed"
    }
  ]
}`
	if string(payload) != want {
		t.Fatalf("alias list document drifted:\n%s", payload)
	}

	empty, err := json.Marshal(AliasListResult{Kind: KindAliasList, Aliases: []AliasEntry{}})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if want := `{"apiVersion":"","kind":"AliasList","configPath":"","aliases":[]}`; string(empty) != want {
		t.Fatalf("empty document = %s, want %s", empty, want)
	}
}
