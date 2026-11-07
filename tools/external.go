package tools

import (
	"os/exec"
	"sync"
)

// External search tool detection.
//
// cobot shells out to ripgrep ("rg") for content search and fd for filename
// search when they are on PATH. Both are dramatically faster than a pure-Go
// walk on large repositories: parallel directory traversal, SIMD-accelerated
// matching, memory-mapped IO and .gitignore awareness. When a binary is
// missing the tools transparently fall back to the built-in pure-Go
// implementations, so cobot stays fully portable without external deps.

var (
	rgOnce sync.Once
	rgPath string

	fdOnce sync.Once
	fdPath string

	// Test hooks: when non-empty these bypass the PATH lookup, letting unit
	// tests point at fixture binaries or force the pure-Go fallback by
	// naming a binary that does not exist.
	rgPathOverride string
	fdPathOverride string
)

// rgBinary returns the path to ripgrep, or "" when unavailable.
func rgBinary() string {
	if rgPathOverride != "" {
		return rgPathOverride
	}
	rgOnce.Do(func() {
		rgPath, _ = exec.LookPath("rg")
	})
	return rgPath
}

// fdBinary returns the path to fd-find, or "" when unavailable.
func fdBinary() string {
	if fdPathOverride != "" {
		return fdPathOverride
	}
	fdOnce.Do(func() {
		fdPath, _ = exec.LookPath("fd")
	})
	return fdPath
}
