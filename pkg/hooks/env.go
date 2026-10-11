package hooks

import (
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redentordev/tako-cli/pkg/config"
)

// MaxErrorBytes caps TAKO_ERROR.
const MaxErrorBytes = 4096

// baselineEnv is copied into every hook environment when set.
var baselineEnv = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "LANG", "TZ", "TMPDIR", "CI"}

// baselineEnvWindows is added on Windows.
var baselineEnvWindows = []string{
	"USERPROFILE", "USERNAME", "TEMP", "TMP", "SystemRoot", "SystemDrive", "windir", "ComSpec", "PATHEXT",
	"APPDATA", "LOCALAPPDATA", "ProgramData", "ProgramFiles", "ProgramFiles(x86)", "PSModulePath",
}

// Context is the operation context rendered into TAKO_* variables. Empty
// fields are omitted from the environment.
type Context struct {
	Hook        Name
	Command     string
	Project     string
	Environment string
	Services    []string
	Servers     []string
	Hosts       []string
	Performer   string
	RecordedAt  time.Time
	Version     string

	Revision       string
	Image          string
	GitCommit      string
	GitCommitShort string
	GitBranch      string
	GitDirty       *bool

	PlanHash        string
	PlanEmpty       *bool
	PlanDestructive *bool

	DeploymentID   string
	RollbackTarget string

	Result          string
	DurationSeconds *float64
	ErrorClass      string
	Error           string
	FailedHook      Name
}

// Vars renders the context as TAKO_* variables.
func (c Context) Vars() map[string]string {
	vars := map[string]string{
		"TAKO_HOOK":        string(c.Hook),
		"TAKO_COMMAND":     c.Command,
		"TAKO_PROJECT":     c.Project,
		"TAKO_ENVIRONMENT": c.Environment,
		"TAKO_SERVICES":    joinSorted(c.Services),
		"TAKO_SERVERS":     joinSorted(c.Servers),
		"TAKO_HOSTS":       joinSorted(c.Hosts),
		"TAKO_PERFORMER":   c.Performer,
		"TAKO_VERSION":     c.Version,
	}
	recordedAt := c.RecordedAt
	if recordedAt.IsZero() {
		recordedAt = time.Now()
	}
	vars["TAKO_RECORDED_AT"] = recordedAt.UTC().Format(time.RFC3339)
	optional := map[string]string{
		"TAKO_REVISION":         c.Revision,
		"TAKO_IMAGE":            c.Image,
		"TAKO_GIT_COMMIT":       c.GitCommit,
		"TAKO_GIT_COMMIT_SHORT": c.GitCommitShort,
		"TAKO_GIT_BRANCH":       c.GitBranch,
		"TAKO_PLAN_HASH":        c.PlanHash,
		"TAKO_DEPLOYMENT_ID":    c.DeploymentID,
		"TAKO_ROLLBACK_TARGET":  c.RollbackTarget,
		"TAKO_RESULT":           c.Result,
		"TAKO_ERROR_CLASS":      c.ErrorClass,
		"TAKO_ERROR":            truncateUTF8(c.Error, MaxErrorBytes),
		"TAKO_FAILED_HOOK":      string(c.FailedHook),
		"TAKO_GIT_DIRTY":        formatBool(c.GitDirty),
		"TAKO_PLAN_EMPTY":       formatBool(c.PlanEmpty),
		"TAKO_PLAN_DESTRUCTIVE": formatBool(c.PlanDestructive),
		"TAKO_DURATION_SECONDS": formatSeconds(c.DurationSeconds),
	}
	for key, value := range optional {
		if value != "" {
			vars[key] = value
		}
	}
	return vars
}

// BuildEnv builds a hook environment from scratch: the platform baseline
// and hooks.passEnv matches taken from base (normally os.Environ()), plus
// the TAKO_* context. Nothing else from base reaches the hook, so .env
// values that config loading exported stay out unless passEnv names them.
// Inherited TAKO_* variables are always dropped.
func BuildEnv(base []string, passEnv []string, ctx Context, goos string) []string {
	out := map[string]string{}
	for _, entry := range base {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" || isTakoName(name) {
			continue
		}
		if inBaseline(name, goos) || matchesPassEnv(name, passEnv, goos) {
			out[name] = value
		}
	}
	for key, value := range ctx.Vars() {
		out[key] = value
	}
	env := make([]string, 0, len(out))
	for key, value := range out {
		env = append(env, key+"="+value)
	}
	sort.Strings(env)
	return env
}

// CredentialValues returns the values of variables forwarded by passEnv
// whose names look like credentials (TOKEN, SECRET, KEY, PASSWORD, PASS,
// CREDENTIAL, AUTH). Callers register them with the output redactor;
// other passed values (NODE_ENV=production) are not registered so they do
// not mask ordinary words in events.
func CredentialValues(base []string, passEnv []string, goos string) []string {
	var values []string
	for _, entry := range base {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" || isTakoName(name) {
			continue
		}
		if matchesPassEnv(name, passEnv, goos) && config.IsCredentialEnvName(name) {
			values = append(values, value)
		}
	}
	return values
}

func isTakoName(name string) bool {
	return strings.HasPrefix(strings.ToUpper(name), "TAKO_")
}

func inBaseline(name string, goos string) bool {
	if strings.HasPrefix(name, "LC_") {
		return true
	}
	allowed := baselineEnv
	if goos == "windows" {
		allowed = append(append([]string(nil), baselineEnv...), baselineEnvWindows...)
	}
	for _, candidate := range allowed {
		if sameName(name, candidate, goos) {
			return true
		}
	}
	return false
}

func matchesPassEnv(name string, patterns []string, goos string) bool {
	for _, pattern := range patterns {
		if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
			if len(name) >= len(prefix) && sameName(name[:len(prefix)], prefix, goos) {
				return true
			}
			continue
		}
		if sameName(name, pattern, goos) {
			return true
		}
	}
	return false
}

// sameName compares variable names: case-insensitively on Windows, where
// environment names are case-insensitive (Path vs PATH).
func sameName(a, b, goos string) bool {
	if goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func joinSorted(values []string) string {
	if len(values) == 0 {
		return ""
	}
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

func formatBool(value *bool) string {
	if value == nil {
		return ""
	}
	return strconv.FormatBool(*value)
}

func formatSeconds(value *float64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatFloat(*value, 'f', 3, 64)
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
