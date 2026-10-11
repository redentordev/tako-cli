package hooks

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
)

// windowsNativeExts are the hook variants Windows runs natively, in
// resolution-report order.
var windowsNativeExts = []string{".ps1", ".cmd", ".bat", ".exe"}

// Resolution is the outcome of resolving a project's hooks. Resolve
// collects every problem instead of stopping at the first, so `tako
// validate` can report them together; lifecycle commands fail on Err().
type Resolution struct {
	// Dir is the hook directory (absolute when the project dir is).
	Dir string
	// Hooks holds the runnable hooks by name.
	Hooks map[Name]Invocation
	// Defined lists every hook name with a file or command definition,
	// runnable or not, in lifecycle order.
	Defined []Name
	// Errors are resolution errors; most implement HookError.
	Errors []error
	// Unknown lists hook-like file names (pre-*, post-*, on-*) in the hook
	// directory that Tako will never run, with a suggestion each.
	Unknown []UnknownHookFile
}

// UnknownHookFile is a file in the hook directory that looks like a hook but
// is not a known hook name or runnable variant.
type UnknownHookFile struct {
	Name    string
	Message string
}

// Err joins all resolution errors, or returns nil.
func (r *Resolution) Err() error {
	if r == nil || len(r.Errors) == 0 {
		return nil
	}
	return errors.Join(r.Errors...)
}

// Empty reports whether no hook is defined.
func (r *Resolution) Empty() bool {
	return r == nil || len(r.Defined) == 0
}

// Has reports whether name resolved to a runnable hook.
func (r *Resolution) Has(name Name) bool {
	if r == nil {
		return false
	}
	_, ok := r.Hooks[name]
	return ok
}

// ResolveOptions makes platform behavior injectable for tests.
type ResolveOptions struct {
	// GOOS selects Unix or Windows resolution rules.
	GOOS string
	// Getenv reads PATH, PATHEXT, ComSpec, and SystemRoot.
	Getenv func(string) string
	// LookPath resolves hooks.commands argv[0]. Defaults to exec.LookPath
	// when GOOS is the running platform.
	LookPath func(string) (string, error)
	// IsFile reports whether a candidate interpreter path exists. Defaults
	// to a regular-file stat.
	IsFile func(string) bool
}

// Resolve resolves the hooks for a project whose config file lives in
// projectDir, using the running platform's rules.
func Resolve(projectDir string, cfg *config.HooksConfig, goos string) *Resolution {
	return ResolveWith(projectDir, cfg, ResolveOptions{GOOS: goos})
}

// ResolveWith is Resolve with injectable platform behavior.
func ResolveWith(projectDir string, cfg *config.HooksConfig, opts ResolveOptions) *Resolution {
	opts = opts.withDefaults()
	r := &resolver{opts: opts, projectDir: projectDir}
	return r.resolve(cfg)
}

func (o ResolveOptions) withDefaults() ResolveOptions {
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.IsFile == nil {
		o.IsFile = func(candidate string) bool {
			info, err := os.Stat(candidate)
			return err == nil && info.Mode().IsRegular()
		}
	}
	if o.LookPath == nil {
		if o.GOOS == runtime.GOOS {
			o.LookPath = exec.LookPath
		} else {
			o.LookPath = func(file string) (string, error) {
				if o.IsFile(file) {
					return file, nil
				}
				return "", exec.ErrNotFound
			}
		}
	}
	return o
}

type resolver struct {
	opts       ResolveOptions
	projectDir string
}

// hookFile is one candidate file for a known hook.
type hookFile struct {
	name string // file name as found
	path string // full path
	ext  string // lowercase extension, "" for the extensionless file
	mode fs.FileMode
}

func (r *resolver) windows() bool { return r.opts.GOOS == "windows" }

func (r *resolver) resolve(cfg *config.HooksConfig) *Resolution {
	dir := filepath.Join(r.projectDir, filepath.FromSlash(cfg.HooksDir()))
	res := &Resolution{Dir: dir, Hooks: map[Name]Invocation{}}
	files, err := r.scan(dir, res)
	if err != nil {
		res.Errors = append(res.Errors, err)
	}

	var commands map[string][]string
	if cfg != nil {
		commands = cfg.Commands
	}
	for _, name := range Known() {
		candidates := files[name]
		argv, hasCommand := commands[string(name)]
		if len(candidates) == 0 && !hasCommand {
			continue
		}
		res.Defined = append(res.Defined, name)
		if len(candidates) > 0 && hasCommand {
			sources := make([]string, 0, len(candidates)+1)
			for _, candidate := range candidates {
				sources = append(sources, r.rel(candidate.path))
			}
			sources = append(sources, "hooks.commands."+string(name))
			res.Errors = append(res.Errors, &AmbiguousHookError{Hook: name, Sources: sources})
			continue
		}
		var invocation Invocation
		var resolveErr error
		if hasCommand {
			invocation, resolveErr = r.resolveCommand(name, argv)
		} else if r.windows() {
			invocation, resolveErr = r.resolveWindowsFile(name, candidates)
		} else {
			invocation, resolveErr = r.resolveUnixFile(name, candidates)
		}
		if resolveErr != nil {
			res.Errors = append(res.Errors, resolveErr)
			continue
		}
		invocation.Name = name
		invocation.Dir = r.projectDir
		res.Hooks[name] = invocation
	}
	return res
}

// scan reads the hook directory and groups candidate files by hook name.
// A missing directory means no file hooks.
func (r *resolver) scan(dir string, res *Resolution) (map[Name][]hookFile, error) {
	if info, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("hooks: cannot read hook directory %s: %w", r.rel(dir), err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("hooks: cannot read hook directory %s: not a directory", r.rel(dir))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("hooks: cannot read hook directory %s: %w", r.rel(dir), err)
	}
	files := map[Name][]hookFile{}
	for _, entry := range entries {
		fileName := entry.Name()
		full := filepath.Join(dir, fileName)
		info, err := os.Stat(full) // follow symlinks
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if strings.HasSuffix(strings.ToLower(fileName), ".sample") {
			continue
		}
		// Hook names contain no dots, so a name with a dot is stem+ext.
		ext := strings.ToLower(filepath.Ext(fileName))
		stem := strings.TrimSuffix(fileName, filepath.Ext(fileName))
		if r.windows() {
			// Windows file names are case-insensitive.
			stem = strings.ToLower(stem)
		}
		switch {
		case IsKnown(stem) && (ext == "" || slices.Contains(windowsNativeExts, ext)):
			files[Name(stem)] = append(files[Name(stem)], hookFile{name: fileName, path: full, ext: ext, mode: info.Mode()})
		case looksLikeHook(fileName):
			res.Unknown = append(res.Unknown, UnknownHookFile{Name: fileName, Message: unknownHookMessage(r.rel(full), fileName, stem, ext)})
		}
	}
	sort.Slice(res.Unknown, func(i, j int) bool { return res.Unknown[i].Name < res.Unknown[j].Name })
	for name := range files {
		sort.Slice(files[name], func(i, j int) bool {
			return variantRank(files[name][i].ext) < variantRank(files[name][j].ext)
		})
	}
	return files, nil
}

func variantRank(ext string) int {
	if ext == "" {
		return len(windowsNativeExts)
	}
	return slices.Index(windowsNativeExts, ext)
}

func (r *resolver) resolveUnixFile(name Name, candidates []hookFile) (Invocation, error) {
	for _, candidate := range candidates {
		if candidate.ext != "" {
			continue
		}
		if candidate.mode.Perm()&0o111 == 0 {
			return Invocation{}, &NotExecutableError{Hook: name, Path: r.rel(candidate.path)}
		}
		return Invocation{Source: SourceFile, Path: candidate.path, RelPath: r.rel(candidate.path), Argv: []string{candidate.path}}, nil
	}
	return Invocation{}, &NoRunnableVariantError{Hook: name, GOOS: r.opts.GOOS, Files: r.relNames(candidates)}
}

func (r *resolver) resolveWindowsFile(name Name, candidates []hookFile) (Invocation, error) {
	var native []hookFile
	var plain *hookFile
	for i := range candidates {
		if candidates[i].ext == "" {
			plain = &candidates[i]
			continue
		}
		native = append(native, candidates[i])
	}
	if len(native) > 1 {
		return Invocation{}, &AmbiguousHookError{Hook: name, Sources: r.relNames(native)}
	}
	if len(native) == 1 {
		hook := native[0]
		invocation := Invocation{Source: SourceFile, Path: hook.path, RelPath: r.rel(hook.path)}
		switch hook.ext {
		case ".ps1":
			shell := r.findInterpreter("pwsh")
			if shell == "" {
				shell = r.findInterpreter("powershell")
			}
			if shell == "" {
				return Invocation{}, &InterpreterNotFoundError{Hook: name, Path: invocation.RelPath, Interpreter: "pwsh or powershell.exe"}
			}
			invocation.Interpreter = shell
			invocation.Argv = []string{shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", hook.path}
		case ".cmd", ".bat":
			comspec := strings.TrimSpace(r.opts.Getenv("ComSpec"))
			if comspec == "" {
				comspec = "cmd.exe"
			}
			invocation.Interpreter = comspec
			invocation.Argv = []string{comspec, "/d", "/c", hook.path}
		default: // .exe
			invocation.Argv = []string{hook.path}
		}
		return invocation, nil
	}
	if plain == nil {
		return Invocation{}, &NoRunnableVariantError{Hook: name, GOOS: r.opts.GOOS, Files: r.relNames(candidates)}
	}
	interpreters := shebangInterpreters(plain.path)
	var shell string
	for _, candidate := range interpreters {
		if shell = r.findInterpreter(candidate); shell != "" {
			break
		}
	}
	if shell == "" {
		return Invocation{}, &InterpreterNotFoundError{Hook: name, Path: r.rel(plain.path), Interpreter: strings.Join(interpreters, " or ") + " (for example from Git for Windows)"}
	}
	return Invocation{
		Source:      SourceFile,
		Path:        plain.path,
		RelPath:     r.rel(plain.path),
		Interpreter: shell,
		// MSYS and Cygwin shells accept forward-slash Windows paths.
		Argv: []string{shell, filepath.ToSlash(plain.path)},
	}, nil
}

// shebangInterpreters returns the interpreter names to try for an
// extensionless script on Windows: the shebang program (the argument after
// env for `#!/usr/bin/env x`), otherwise sh then bash.
func shebangInterpreters(scriptPath string) []string {
	defaults := []string{"sh", "bash"}
	file, err := os.Open(scriptPath)
	if err != nil {
		return defaults
	}
	defer file.Close()
	line, err := bufio.NewReader(file).ReadString('\n')
	if err != nil && line == "" {
		return defaults
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
	if !strings.HasPrefix(line, "#!") {
		return defaults
	}
	fields := strings.Fields(strings.TrimPrefix(line, "#!"))
	if len(fields) == 0 {
		return defaults
	}
	program := path.Base(filepath.ToSlash(fields[0]))
	if strings.TrimSuffix(strings.ToLower(program), ".exe") == "env" {
		program = ""
		for _, field := range fields[1:] {
			if !strings.HasPrefix(field, "-") {
				program = field
				break
			}
		}
		if program == "" {
			return defaults
		}
	}
	return []string{strings.TrimSuffix(program, ".exe")}
}

// findInterpreter searches PATH for a Windows program, skipping the WSL
// launcher bash.exe (System32 and WindowsApps), which runs scripts in a
// different filesystem.
func (r *resolver) findInterpreter(program string) string {
	exts := []string{".exe"}
	if r.windows() {
		if pathext := r.opts.Getenv("PATHEXT"); pathext != "" {
			exts = nil
			for _, ext := range strings.Split(pathext, ";") {
				if ext = strings.ToLower(strings.TrimSpace(ext)); ext == ".exe" || ext == ".com" {
					exts = append(exts, ext)
				}
			}
			if len(exts) == 0 {
				exts = []string{".exe"}
			}
		}
	} else {
		exts = []string{""}
	}
	separator := ":"
	if r.windows() {
		separator = ";"
	}
	for _, dir := range strings.Split(r.opts.Getenv("PATH"), separator) {
		dir = strings.Trim(strings.TrimSpace(dir), `"`)
		if dir == "" {
			continue
		}
		for _, ext := range exts {
			candidate := joinPath(r.opts.GOOS, dir, program+ext)
			if r.isWSLLauncher(candidate) {
				continue
			}
			if r.opts.IsFile(candidate) {
				return candidate
			}
		}
	}
	return ""
}

func (r *resolver) isWSLLauncher(candidate string) bool {
	if !r.windows() {
		return false
	}
	normalized := strings.ToLower(strings.ReplaceAll(candidate, "/", `\`))
	if !strings.HasSuffix(normalized, `\bash.exe`) {
		return false
	}
	systemRoot := strings.ToLower(strings.TrimRight(strings.ReplaceAll(r.opts.Getenv("SystemRoot"), "/", `\`), `\`))
	if systemRoot == "" {
		systemRoot = `c:\windows`
	}
	return strings.HasPrefix(normalized, systemRoot+`\system32\`) || strings.Contains(normalized, `\microsoft\windowsapps\`)
}

func joinPath(goos, dir, name string) string {
	if goos == "windows" {
		return strings.TrimRight(dir, `\/`) + `\` + name
	}
	return strings.TrimRight(dir, "/") + "/" + name
}

func (r *resolver) resolveCommand(name Name, argv []string) (Invocation, error) {
	command := strings.TrimSpace(argv[0])
	target := command
	if strings.ContainsAny(command, `/\`) && !filepath.IsAbs(command) {
		target = filepath.Join(r.projectDir, filepath.FromSlash(command))
	}
	resolved, err := r.opts.LookPath(target)
	if err != nil {
		return Invocation{}, &CommandNotFoundError{Hook: name, Command: command, Err: err}
	}
	full := append([]string{resolved}, argv[1:]...)
	return Invocation{Source: SourceConfig, Argv: full}, nil
}

func (r *resolver) rel(target string) string {
	if rel, err := filepath.Rel(r.projectDir, target); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(target)
}

func (r *resolver) relNames(files []hookFile) []string {
	names := make([]string, 0, len(files))
	for _, file := range files {
		names = append(names, r.rel(file.path))
	}
	return names
}

// looksLikeHook reports whether a file name looks like an attempted hook:
// pre-*, post-*, on-* (or with an underscore).
func looksLikeHook(fileName string) bool {
	lower := strings.ToLower(fileName)
	for _, prefix := range []string{"pre-", "pre_", "post-", "post_", "on-", "on_"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func unknownHookMessage(relPath, fileName, stem, ext string) string {
	if IsKnown(stem) {
		return fmt.Sprintf("hook file %s is ignored: hook files are named exactly %s (no extension) on Linux and macOS; Windows also accepts .ps1, .cmd, .bat, and .exe", relPath, stem)
	}
	message := fmt.Sprintf("hook file %s is not a known hook name and will never run", relPath)
	if suggestion := closestHookName(stem); suggestion != "" {
		message += fmt.Sprintf("; did you mean %s?", suggestion)
	}
	names := make([]string, 0, len(config.HookNames))
	names = append(names, config.HookNames...)
	return message + " Known hooks: " + strings.Join(names, ", ")
}

func closestHookName(stem string) string {
	normalized := strings.ToLower(strings.ReplaceAll(stem, "_", "-"))
	best, bestDistance := "", 3
	for _, name := range config.HookNames {
		if distance := levenshtein(normalized, name); distance < bestDistance {
			best, bestDistance = name, distance
		}
	}
	return best
}

func levenshtein(a, b string) int {
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}
