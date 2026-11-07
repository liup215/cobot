//go:build windows

package shell

import (
	"os/exec"
	"syscall"
)

// hideConsole prevents child processes from flashing a console window when
// cobot hosts run as GUI apps on Windows. The flags are harmless inside
// a terminal session.
func HideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}
