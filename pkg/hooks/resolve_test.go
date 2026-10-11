package hooks

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func writeHookFile(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKnownMatchesConfig(t *testing.T) {
	known := Known()
	if len(known) != 9 || known[0] != PreDeploy || known[8] != OnFailure {
		t.Fatalf("Known() = %v", known)
	}
	for _, name := range known {
		if !IsKnown(string(name)) {
			t.Fatalf("%s not known", name)
		}
	}
}

func TestResolveNoHooks(t *testing.T) {
	project := t.TempDir()
	res := Resolve(project, nil, runtime.GOOS)
	if !res.Empty() || res.Err() != nil || len(res.Unknown) != 0 || len(res.Hooks) != 0 {
		t.Fatalf("resolution = %+v", res)
	}
}

func TestResolveHooksDirIsAFile(t *testing.T) {
	project := t.TempDir()
	writeHookFile(t, project, ".tako-hooks", "x", 0o644)
	res := Resolve(project, nil, runtime.GOOS)
	if res.Err() == nil || !strings.Contains(res.Err().Error(), "cannot read hook directory .tako-hooks") {
		t.Fatalf("err = %v", res.Err())
	}
}

func TestResolveUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix executable-bit resolution")
	}
	project := t.TempDir()
	dir := filepath.Join(project, ".tako-hooks")
	preDeploy := writeHookFile(t, dir, "pre-deploy", "#!/bin/sh\necho ok\n", 0o755)
	writeHookFile(t, dir, "post-deploy", "#!/bin/sh\n", 0o644)
	writeHookFile(t, dir, "pre-promote.ps1", "Write-Host hi", 0o644)
	writeHookFile(t, dir, "pre-setup", "#!/bin/sh\n", 0o755)
	writeHookFile(t, dir, "pre-setup.ps1", "Write-Host hi", 0o644) // cross-platform teams keep both
	writeHookFile(t, dir, "pre-rollback.sample", "#!/bin/sh\n", 0o755)
	writeHookFile(t, dir, "README.md", "docs", 0o644)
	writeHookFile(t, dir, "pre_deploy", "#!/bin/sh\n", 0o755)
	writeHookFile(t, dir, "post-deploi", "#!/bin/sh\n", 0o755)
	writeHookFile(t, dir, "on-failure.sh", "#!/bin/sh\n", 0o755)

	res := Resolve(project, nil, "linux")
	invocation, ok := res.Hooks[PreDeploy]
	if !ok || invocation.Source != SourceFile || invocation.RelPath != ".tako-hooks/pre-deploy" || invocation.Argv[0] != preDeploy || invocation.Dir != project {
		t.Fatalf("pre-deploy = %+v", invocation)
	}
	if !res.Has(PreSetup) || res.Hooks[PreSetup].Interpreter != "" {
		t.Fatalf("pre-setup = %+v", res.Hooks[PreSetup])
	}

	var notExec *NotExecutableError
	var noVariant *NoRunnableVariantError
	if len(res.Errors) != 2 || !errors.As(res.Errors[0], &notExec) || !errors.As(res.Errors[1], &noVariant) {
		t.Fatalf("errors = %v", res.Errors)
	}
	if notExec.Hook != PostDeploy || !strings.Contains(notExec.Error(), "chmod +x .tako-hooks/post-deploy") || !strings.Contains(notExec.Error(), "git update-index --chmod=+x") {
		t.Fatalf("not executable error = %v", notExec)
	}
	if noVariant.Hook != PrePromote || !strings.Contains(noVariant.Error(), "no runnable pre-promote variant exists for linux") {
		t.Fatalf("no variant error = %v", noVariant)
	}
	if got := definedNames(res); got != "pre-deploy,post-deploy,pre-promote,pre-setup" {
		t.Fatalf("defined = %s", got)
	}

	unknown := map[string]string{}
	for _, file := range res.Unknown {
		unknown[file.Name] = file.Message
	}
	if len(unknown) != 3 {
		t.Fatalf("unknown = %+v", res.Unknown)
	}
	if !strings.Contains(unknown["pre_deploy"], "did you mean pre-deploy?") || !strings.Contains(unknown["post-deploi"], "did you mean post-deploy?") {
		t.Fatalf("unknown = %+v", unknown)
	}
	if !strings.Contains(unknown["on-failure.sh"], "named exactly on-failure (no extension)") {
		t.Fatalf("unknown = %+v", unknown)
	}
}

func definedNames(res *Resolution) string {
	names := make([]string, 0, len(res.Defined))
	for _, name := range res.Defined {
		names = append(names, string(name))
	}
	return strings.Join(names, ",")
}

// fakeWindows builds Windows resolve options over a fake PATH. Hook files
// are real temp files; interpreter files exist only in the fake set.
func fakeWindows(path string, existing ...string) ResolveOptions {
	files := map[string]bool{}
	for _, file := range existing {
		files[strings.ToLower(file)] = true
	}
	env := map[string]string{
		"PATH":       path,
		"PATHEXT":    ".COM;.EXE;.BAT;.CMD",
		"ComSpec":    `C:\Windows\system32\cmd.exe`,
		"SystemRoot": `C:\Windows`,
	}
	return ResolveOptions{
		GOOS:   "windows",
		Getenv: func(key string) string { return env[key] },
		IsFile: func(candidate string) bool { return files[strings.ToLower(candidate)] },
	}
}

func TestResolveWindowsVariants(t *testing.T) {
	const gitBin = `C:\Program Files\Git\usr\bin`
	const system32 = `C:\Windows\System32`
	const pwshDir = `C:\Program Files\PowerShell\7`
	const winPS = `C:\Windows\System32\WindowsPowerShell\v1.0`
	cases := []struct {
		name        string
		files       map[string]string
		path        string
		existing    []string
		wantArgv0   string
		wantArgs    []string
		wantErr     any
		errContains []string
	}{
		{
			name:      "ps1 prefers pwsh",
			files:     map[string]string{"pre-deploy.ps1": "Write-Host hi"},
			path:      winPS + ";" + pwshDir,
			existing:  []string{winPS + `\powershell.exe`, pwshDir + `\pwsh.exe`},
			wantArgv0: pwshDir + `\pwsh.exe`,
			wantArgs:  []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File"},
		},
		{
			name:      "ps1 falls back to powershell.exe",
			files:     map[string]string{"Pre-Deploy.PS1": "Write-Host hi"},
			path:      winPS,
			existing:  []string{winPS + `\powershell.exe`},
			wantArgv0: winPS + `\powershell.exe`,
			wantArgs:  []string{"-NoLogo", "-NoProfile", "-NonInteractive"},
		},
		{
			name:        "ps1 without any PowerShell",
			files:       map[string]string{"pre-deploy.ps1": "Write-Host hi"},
			path:        gitBin,
			wantErr:     &InterpreterNotFoundError{},
			errContains: []string{"pwsh or powershell.exe"},
		},
		{
			name:      "cmd via ComSpec",
			files:     map[string]string{"pre-deploy.cmd": "@echo hi"},
			wantArgv0: `C:\Windows\system32\cmd.exe`,
			wantArgs:  []string{"/d", "/c"},
		},
		{
			name:      "bat via ComSpec",
			files:     map[string]string{"pre-deploy.bat": "@echo hi"},
			wantArgv0: `C:\Windows\system32\cmd.exe`,
			wantArgs:  []string{"/d", "/c"},
		},
		{
			name:      "exe runs directly",
			files:     map[string]string{"pre-deploy.exe": "MZ"},
			wantArgv0: "pre-deploy.exe",
		},
		{
			name:      "native variant wins over extensionless",
			files:     map[string]string{"pre-deploy": "#!/bin/sh\n", "pre-deploy.cmd": "@echo hi"},
			path:      gitBin,
			existing:  []string{gitBin + `\sh.exe`},
			wantArgv0: `C:\Windows\system32\cmd.exe`,
		},
		{
			name:        "two native variants are ambiguous",
			files:       map[string]string{"pre-deploy.ps1": "x", "pre-deploy.cmd": "x"},
			wantErr:     &AmbiguousHookError{},
			errContains: []string{".tako-hooks/pre-deploy.ps1", ".tako-hooks/pre-deploy.cmd"},
		},
		{
			name:      "bash shebang skips the WSL launcher",
			files:     map[string]string{"pre-deploy": "#!/usr/bin/env bash\necho hi\n"},
			path:      system32 + ";" + gitBin,
			existing:  []string{system32 + `\bash.exe`, gitBin + `\bash.exe`},
			wantArgv0: gitBin + `\bash.exe`,
		},
		{
			name:        "only the WSL launcher is not enough",
			files:       map[string]string{"pre-deploy": "#!/bin/bash\n"},
			path:        system32,
			existing:    []string{system32 + `\bash.exe`},
			wantErr:     &InterpreterNotFoundError{},
			errContains: []string{"needs bash"},
		},
		{
			name:      "no shebang uses sh",
			files:     map[string]string{"pre-deploy": "echo hi\n"},
			path:      gitBin,
			existing:  []string{gitBin + `\sh.exe`, gitBin + `\bash.exe`},
			wantArgv0: gitBin + `\sh.exe`,
		},
		{
			name:      "no shebang falls back to bash",
			files:     map[string]string{"pre-deploy": "echo hi\n"},
			path:      gitBin,
			existing:  []string{gitBin + `\bash.exe`},
			wantArgv0: gitBin + `\bash.exe`,
		},
		{
			name:      "env -S shebang",
			files:     map[string]string{"pre-deploy": "#!/usr/bin/env -S python3 -u\n"},
			path:      `C:\Python`,
			existing:  []string{`C:\Python\python3.exe`},
			wantArgv0: `C:\Python\python3.exe`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			project := t.TempDir()
			dir := filepath.Join(project, ".tako-hooks")
			for name, content := range tc.files {
				writeHookFile(t, dir, name, content, 0o644)
			}
			res := ResolveWith(project, nil, fakeWindows(tc.path, tc.existing...))
			if tc.wantErr != nil {
				if len(res.Errors) != 1 || res.Has(PreDeploy) {
					t.Fatalf("errors = %v, hooks = %+v", res.Errors, res.Hooks)
				}
				switch tc.wantErr.(type) {
				case *InterpreterNotFoundError:
					var target *InterpreterNotFoundError
					if !errors.As(res.Errors[0], &target) {
						t.Fatalf("error = %T %v", res.Errors[0], res.Errors[0])
					}
				case *AmbiguousHookError:
					var target *AmbiguousHookError
					if !errors.As(res.Errors[0], &target) {
						t.Fatalf("error = %T %v", res.Errors[0], res.Errors[0])
					}
				}
				for _, want := range tc.errContains {
					if !strings.Contains(res.Errors[0].Error(), want) {
						t.Fatalf("error = %v, want %q", res.Errors[0], want)
					}
				}
				return
			}
			if res.Err() != nil {
				t.Fatalf("err = %v", res.Err())
			}
			invocation, ok := res.Hooks[PreDeploy]
			if !ok {
				t.Fatalf("pre-deploy not resolved: %+v", res)
			}
			if tc.wantArgv0 == "pre-deploy.exe" {
				if filepath.Base(invocation.Argv[0]) != "pre-deploy.exe" || len(invocation.Argv) != 1 {
					t.Fatalf("argv = %v", invocation.Argv)
				}
				return
			}
			if invocation.Argv[0] != tc.wantArgv0 || invocation.Interpreter != tc.wantArgv0 {
				t.Fatalf("argv = %v interpreter = %q, want %q", invocation.Argv, invocation.Interpreter, tc.wantArgv0)
			}
			for i, arg := range tc.wantArgs {
				if invocation.Argv[1+i] != arg {
					t.Fatalf("argv = %v, want args %v", invocation.Argv, tc.wantArgs)
				}
			}
			if last := invocation.Argv[len(invocation.Argv)-1]; filepath.ToSlash(last) != filepath.ToSlash(invocation.Path) {
				t.Fatalf("last arg = %q, want hook path %q", last, invocation.Path)
			}
		})
	}
}

func TestResolveWindowsOnlyVariantOnLinuxEvenWithoutExecBit(t *testing.T) {
	project := t.TempDir()
	writeHookFile(t, filepath.Join(project, ".tako-hooks"), "pre-deploy.ps1", "x", 0o644)
	res := ResolveWith(project, nil, ResolveOptions{GOOS: "linux", IsFile: func(string) bool { return false }})
	var noVariant *NoRunnableVariantError
	if !errors.As(res.Err(), &noVariant) || !strings.Contains(noVariant.Error(), ".tako-hooks/pre-deploy.ps1") {
		t.Fatalf("err = %v", res.Err())
	}
}

func TestResolveCommands(t *testing.T) {
	project := t.TempDir()
	writeHookFile(t, filepath.Join(project, ".tako-hooks"), "post-deploy", "#!/bin/sh\n", 0o755)
	scripts := filepath.Join(project, "scripts")
	writeHookFile(t, scripts, "smoke", "#!/bin/sh\n", 0o755)
	lookups := []string{}
	opts := ResolveOptions{
		GOOS: "linux",
		LookPath: func(file string) (string, error) {
			lookups = append(lookups, file)
			switch {
			case file == "make":
				return "/usr/bin/make", nil
			case strings.HasSuffix(filepath.ToSlash(file), "/scripts/smoke"):
				return file, nil
			}
			return "", exec.ErrNotFound
		},
	}
	cfg := &config.HooksConfig{Commands: map[string][]string{
		"pre-deploy":   {"make", "check"},
		"post-deploy":  {"notify"},
		"pre-rollback": {"missing-tool"},
		"pre-promote":  {"./scripts/smoke", "--fast"},
	}}
	res := ResolveWith(project, cfg, opts)

	if got := res.Hooks[PreDeploy]; got.Source != SourceConfig || strings.Join(got.Argv, " ") != "/usr/bin/make check" || got.Dir != project {
		t.Fatalf("pre-deploy = %+v", got)
	}
	if got := res.Hooks[PrePromote]; got.Argv[0] != filepath.Join(project, "scripts", "smoke") || got.Argv[1] != "--fast" {
		t.Fatalf("pre-promote = %+v", got)
	}
	var ambiguous *AmbiguousHookError
	var missing *CommandNotFoundError
	if len(res.Errors) != 2 || !errors.As(res.Errors[0], &ambiguous) || !errors.As(res.Errors[1], &missing) {
		t.Fatalf("errors = %v", res.Errors)
	}
	if ambiguous.Hook != PostDeploy || !strings.Contains(ambiguous.Error(), ".tako-hooks/post-deploy, hooks.commands.post-deploy") {
		t.Fatalf("ambiguous = %v", ambiguous)
	}
	if missing.Hook != PreRollback || !strings.Contains(missing.Error(), `"missing-tool"`) {
		t.Fatalf("missing = %v", missing)
	}
	var hookErr HookError
	if !errors.As(res.Errors[1], &hookErr) || hookErr.HookName() != PreRollback {
		t.Fatalf("hook attribution = %v", hookErr)
	}
}

func TestResolveCustomPath(t *testing.T) {
	project := t.TempDir()
	writeHookFile(t, filepath.Join(project, "ops", "hooks"), "on-failure.cmd", "@echo x", 0o644)
	res := ResolveWith(project, &config.HooksConfig{Path: "ops/hooks"}, fakeWindows(""))
	if !res.Has(OnFailure) || res.Hooks[OnFailure].RelPath != "ops/hooks/on-failure.cmd" {
		t.Fatalf("resolution = %+v", res)
	}
}
