//go:build !unix

package macos

import "os/exec"

func detach(*exec.Cmd) {}
