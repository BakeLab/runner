// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !windows

package process

import (
	"os"
	"os/exec"
)

func startSuspended(_cmd *exec.Cmd) {}

func resume(_process *os.Process) error {
	return nil
}
