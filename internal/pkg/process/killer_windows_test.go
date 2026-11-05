// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package process

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}

func newProcessExitWaiter(t *testing.T, pids ...int) func() {
	t.Helper()
	handles := make([]windows.Handle, 0, len(pids))
	for _, pid := range pids {
		handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, windows.CloseHandle(handle)) })
		handles = append(handles, handle)
	}
	return func() {
		t.Helper()
		result, err := windows.WaitForMultipleObjects(handles, true, 20000)
		require.NoError(t, err)
		require.EqualValues(t, windows.WAIT_OBJECT_0, result, "processes should exit: %v", pids)
	}
}

func TestKillerKillsTree(t *testing.T) {
	cmd, ready := newTestProcess(t, "parent", false)
	require.NoError(t, cmd.Start())

	killer, err := NewKiller(cmd.Process)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = killer.Kill()
		_ = killer.Close()
	})

	childPID := ready()
	require.True(t, processAlive(childPID), "child process should start")

	waitForExit := newProcessExitWaiter(t, cmd.Process.Pid, childPID)
	require.NoError(t, killer.Kill())
	waitForExit()
}
