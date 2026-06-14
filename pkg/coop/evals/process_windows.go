//go:build windows

package evals

import "os/exec"

func prepareProcessGroup(cmd *exec.Cmd) {}

func terminateProcessGroup(pid int) {}
