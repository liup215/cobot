// Package shell resolves which shell the run tool executes commands with.
//
// bash is preferred everywhere it exists (POSIX semantics, consistent with
// dev containers and CI); on Windows the WSL bash stub is deliberately
// skipped because its path and cwd semantics differ from native Windows
// tools, and cmd.exe is the fallback when no usable bash is found.
package shell

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Shell describes a resolved shell executable.
type Shell struct {
	Path   string   // executable path (or bare name) to pass to exec
	Args   []string // fixed args preceding the command, e.g. {"-c"}
	IsBash bool     // true when the shell speaks POSIX bash
}

// Name returns a short human/model-readable label.
func (s Shell) Name() string {
	if s.IsBash {
		return "bash"
	}
	return filepath.Base(strings.TrimSuffix(s.Path, ".exe"))
}

var (
	once   sync.Once
	cached Shell
)

// Resolve returns the shell the run tool should use. The result is resolved
// once per process and cached.
func Resolve() Shell {
	once.Do(func() { cached = resolve() })
	return cached
}

func resolve() Shell {
	if runtime.GOOS == "windows" {
		if p, ok := findWindowsBash(); ok {
			return Shell{Path: p, Args: []string{"-c"}, IsBash: true}
		}
		return Shell{Path: "cmd", Args: []string{"/C"}}
	}
	// Unix: prefer bash, fall back to sh.
	if p, err := exec.LookPath("bash"); err == nil {
		return Shell{Path: p, Args: []string{"-c"}, IsBash: true}
	}
	return Shell{Path: "sh", Args: []string{"-c"}}
}

// findWindowsBash locates a usable bash.exe, skipping the WSL stubs
// (System32/WindowsApps) whose path and cwd semantics differ from native
// Windows tools.
func findWindowsBash() (string, bool) {
	// Explicit Git-for-Windows install locations first, so a machine whose
	// PATH only exposes the WSL stub still finds Git Bash.
	candidates := []string{
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files\Git\usr\bin\bash.exe`,
		`C:\Program Files (x86)\Git\bin\bash.exe`,
		filepath.Join(osLocalAppData(), "Programs", "Git", "bin", "bash.exe"),
	}
	for _, p := range candidates {
		if p != "" && fileExists(p) {
			return p, true
		}
	}
	if p, err := exec.LookPath("bash"); err == nil && !isWSLStub(p) {
		return p, true
	}
	return "", false
}

// isWSLStub reports whether path points at the bash.exe WSL launcher shipped
// with Windows itself.
func isWSLStub(path string) bool {
	dir := strings.ToLower(filepath.Dir(path))
	return strings.Contains(dir, `\windows\system32`) ||
		strings.Contains(dir, `\windows\sysnative`) ||
		strings.Contains(dir, `\windowsapps`)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// osLocalAppData returns %LOCALAPPDATA% or "" when unavailable.
func osLocalAppData() string {
	return os.Getenv("LOCALAPPDATA")
}
