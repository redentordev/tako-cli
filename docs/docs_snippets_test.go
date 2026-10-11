package docs_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/internal/configtest"
	"gopkg.in/yaml.v3"
)

// enforcedDocs must mark every ```yaml block. Other docs are only checked
// where a block carries a marker.
var enforcedDocs = []string{
	"CONFIGURATION.md",
	"ORCHESTRATION-MODEL.md",
}

const markerHelp = "precede the block with one of <!-- tako:snippet complete -->, <!-- tako:snippet partial -->, or <!-- tako:snippet skip: <reason> -->"

const topLevelServicesHelp = "top-level `services:` is not a Tako config key; nest services under environments.<env>.services (for example environments.production.services)"

var (
	markerPattern     = regexp.MustCompile(`^<!--\s*tako:snippet\s+(.*?)\s*-->$`)
	markerLikePattern = regexp.MustCompile(`^<!--\s*tako:snippet\b`)
	fenceOpenPattern  = regexp.MustCompile("^(\\s*)(`{3,}|~{3,})\\s*([A-Za-z0-9_+-]*)")
)

type snippetMode string

const (
	modeNone     snippetMode = ""
	modeComplete snippetMode = "complete"
	modePartial  snippetMode = "partial"
	modeSkip     snippetMode = "skip"
)

type snippet struct {
	file    string
	line    int // line of the opening fence (1-based)
	mode    snippetMode
	reason  string
	content string
	err     string // marker problem, if any
}

func (s snippet) location() string {
	return fmt.Sprintf("%s:%d", s.file, s.line)
}

// parseSnippets extracts every ```yaml / ```yml fenced block from a Markdown
// document together with the marker on its nearest preceding non-blank line.
func parseSnippets(file string, data string) []snippet {
	lines := strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n")
	var snippets []snippet
	for i := 0; i < len(lines); i++ {
		match := fenceOpenPattern.FindStringSubmatch(lines[i])
		if match == nil {
			continue
		}
		indent, fence, language := match[1], match[2], strings.ToLower(match[3])
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			trimmed := strings.TrimSpace(lines[j])
			if strings.HasPrefix(trimmed, fence[:3]) && strings.Trim(trimmed, fence[:1]) == "" && len(trimmed) >= len(fence) {
				end = j
				break
			}
		}
		if language == "yaml" || language == "yml" {
			body := make([]string, 0, end-i-1)
			for _, line := range lines[i+1 : min(end, len(lines))] {
				body = append(body, strings.TrimPrefix(line, indent))
			}
			s := snippet{file: file, line: i + 1, content: strings.Join(body, "\n") + "\n"}
			s.mode, s.reason, s.err = markerFor(lines, i)
			snippets = append(snippets, s)
		}
		i = end
	}
	return snippets
}

func markerFor(lines []string, fenceIndex int) (snippetMode, string, string) {
	for k := fenceIndex - 1; k >= 0; k-- {
		trimmed := strings.TrimSpace(lines[k])
		if trimmed == "" {
			continue
		}
		if !markerLikePattern.MatchString(trimmed) {
			return modeNone, "", ""
		}
		match := markerPattern.FindStringSubmatch(trimmed)
		if match == nil {
			return modeNone, "", "malformed tako:snippet marker " + trimmed
		}
		spec := match[1]
		switch {
		case spec == string(modeComplete):
			return modeComplete, "", ""
		case spec == string(modePartial):
			return modePartial, "", ""
		case spec == string(modeSkip) || strings.HasPrefix(spec, string(modeSkip)+":"):
			reason := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(spec, string(modeSkip)), ":"))
			if reason == "" {
				return modeSkip, "", "skip marker needs a reason: <!-- tako:snippet skip: <reason> -->"
			}
			return modeSkip, reason, ""
		default:
			return modeNone, "", fmt.Sprintf("unknown tako:snippet mode %q", spec)
		}
	}
	return modeNone, "", ""
}

func markdownDocs(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.md")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

func isEnforced(file string) bool {
	for _, enforced := range enforcedDocs {
		if file == enforced {
			return true
		}
	}
	return false
}

func TestEnforcedDocsExist(t *testing.T) {
	for _, file := range enforcedDocs {
		if _, err := os.Stat(file); err != nil {
			t.Fatalf("enforced doc %s: %v", file, err)
		}
	}
}

func TestDocsYAMLSnippetsValidate(t *testing.T) {
	for _, file := range markdownDocs(t) {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		snippets := parseSnippets(file, string(data))
		enforced := isEnforced(file)
		var unmarked []string
		for _, s := range snippets {
			if s.err != "" {
				t.Errorf("%s: %s; %s", s.location(), s.err, markerHelp)
				continue
			}
			if s.mode == modeNone {
				if enforced {
					unmarked = append(unmarked, s.location())
				}
				continue
			}
			if s.mode == modeSkip {
				continue
			}
			s := s
			t.Run(s.location(), func(t *testing.T) {
				validateSnippet(t, s)
			})
		}
		if len(unmarked) > 0 {
			t.Errorf("%s has unmarked yaml blocks at %s; %s", file, strings.Join(unmarked, ", "), markerHelp)
		}
	}
}

func validateSnippet(t *testing.T, s snippet) {
	t.Helper()
	if err := checkSnippetShape(s.content); err != nil {
		t.Fatalf("%s: %v", s.location(), err)
	}

	content := []byte(s.content)
	if s.mode == modePartial {
		merged, err := configtest.MergeYAML([]byte(configtest.BaseYAML), content)
		if err != nil {
			t.Fatalf("%s: merge onto base config: %v", s.location(), err)
		}
		content = merged
	}

	configtest.SetupEnv(t)
	_, warnings, err := configtest.LoadAndValidate(t, t.TempDir(), "tako.yaml", content)
	if err != nil {
		t.Fatalf("%s (%s): %v\n--- validated config ---\n%s", s.location(), s.mode, err, content)
	}
	for _, warning := range warnings {
		t.Errorf("%s (%s): unexpected warning on %s: %s", s.location(), s.mode, warning.Field, warning.Message)
	}
}

func TestParseSnippetsMarkers(t *testing.T) {
	doc := strings.Join([]string{
		"# Title",
		"",
		"<!-- tako:snippet complete -->",
		"```yaml",
		"project: {}",
		"```",
		"<!-- tako:snippet partial -->",
		"",
		"  ```yml",
		"  environments: {}",
		"  ```",
		"<!-- tako:snippet skip: prometheus config -->",
		"```yaml",
		"scrape_configs: []",
		"```",
		"<!-- tako:snippet skip -->",
		"```yaml",
		"a: b",
		"```",
		"<!-- tako:snippet maybe -->",
		"```yaml",
		"a: b",
		"```",
		"Some text",
		"```yaml",
		"a: b",
		"```",
		"```bash",
		"echo '```yaml'",
		"```",
	}, "\n")
	got := parseSnippets("doc.md", doc)
	type summary struct {
		line   int
		mode   snippetMode
		reason string
		hasErr bool
	}
	var summaries []summary
	for _, s := range got {
		summaries = append(summaries, summary{s.line, s.mode, s.reason, s.err != ""})
	}
	want := []summary{
		{4, modeComplete, "", false},
		{9, modePartial, "", false},
		{13, modeSkip, "prometheus config", false},
		{17, modeSkip, "", true},
		{21, modeNone, "", true},
		{25, modeNone, "", false},
	}
	if fmt.Sprint(summaries) != fmt.Sprint(want) {
		t.Fatalf("snippets = %+v, want %+v", summaries, want)
	}
	if got[1].content != "environments: {}\n" {
		t.Fatalf("indented snippet content = %q", got[1].content)
	}
}

// checkSnippetShape rejects snippets that cannot be Tako config documents
// before they reach the loader, so the failure carries a fix-it message.
func checkSnippetShape(content string) error {
	var top map[string]any
	if err := yaml.Unmarshal([]byte(content), &top); err != nil {
		return fmt.Errorf("snippet is not a YAML mapping: %w", err)
	}
	if _, ok := top["services"]; ok {
		return fmt.Errorf("%s", topLevelServicesHelp)
	}
	return nil
}

func TestTopLevelServicesSnippetIsRejectedWithFixIt(t *testing.T) {
	err := checkSnippetShape("services:\n  web:\n    image: nginx\n")
	if err == nil || !strings.Contains(err.Error(), "environments.<env>.services") {
		t.Fatalf("err = %v, want fix-it naming environments.<env>.services", err)
	}
	if err := checkSnippetShape("environments:\n  production:\n    services: {}\n"); err != nil {
		t.Fatalf("nested services rejected: %v", err)
	}
}

// TestEnvironmentInheritanceSnippetsResolve checks that the documented x-
// anchor and extends examples resolve to what the prose describes.
func TestEnvironmentInheritanceSnippetsResolve(t *testing.T) {
	data, err := os.ReadFile("CONFIGURATION.md")
	if err != nil {
		t.Fatal(err)
	}
	find := func(marker string) snippet {
		for _, s := range parseSnippets("CONFIGURATION.md", string(data)) {
			if strings.Contains(s.content, marker) {
				return s
			}
		}
		t.Fatalf("no CONFIGURATION.md snippet contains %q", marker)
		return snippet{}
	}

	configtest.SetupEnv(t)
	cfg, _, err := configtest.LoadAndValidate(t, t.TempDir(), "tako.yaml", []byte(find("x-web: &web").content))
	if err != nil {
		t.Fatalf("x- snippet: %v", err)
	}
	for _, env := range []string{"production", "staging"} {
		web := cfg.Environments[env].Services["web"]
		if web.Image != "ghcr.io/example/app:1.4.2" || web.Port != 3000 || web.HealthCheck.Path != "/health" {
			t.Fatalf("%s web = %+v, want the shared x-web fragment", env, web)
		}
	}

	cfg, _, err = configtest.LoadAndValidate(t, t.TempDir(), "tako.yaml", []byte(find("worker: null").content))
	if err != nil {
		t.Fatalf("extends snippet: %v", err)
	}
	if _, ok := cfg.Environments["base"]; ok {
		t.Fatal("abstract base must not be a deployable environment")
	}
	staging := cfg.Environments["staging"]
	web := staging.Services["web"]
	if _, ok := staging.Services["worker"]; ok || web.Replicas != 1 || web.Env["LOG_LEVEL"] != "debug" || web.Env["REGION"] != "eu" || web.Proxy.Domain != "staging.example.com" {
		t.Fatalf("staging = %+v", staging)
	}
	production := cfg.Environments["production"]
	if production.Services["web"].Replicas != 2 || production.Services["web"].Proxy.Domain != "app.example.com" || len(production.Services) != 2 {
		t.Fatalf("production = %+v", production)
	}
}
