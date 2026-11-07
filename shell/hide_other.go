//go:build !windows

package shell

import "os/exec"

// hideConsole is a no-op on non-Windows platforms; child processes never
// flash console windows there.
func HideConsole(cmd *exec.Cmd) {}
