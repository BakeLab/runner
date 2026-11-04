// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package process

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

var procNtResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// startSuspended holds the process until Capture has assigned it to its job objects, so no child it starts escapes them.
func startSuspended(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
}

func resume(process *os.Process) error {
	var status windows.NTStatus
	if err := process.WithHandle(func(handle uintptr) {
		ret, _, _ := procNtResumeProcess.Call(handle)
		status = windows.NTStatus(ret)
	}); err != nil {
		return err
	}
	if status == windows.STATUS_SUCCESS || status == windows.STATUS_PROCESS_IS_TERMINATING { // cancellation may have ended it already
		return nil
	}
	return status
}
