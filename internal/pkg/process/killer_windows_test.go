// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package process

import (
	"testing"
	"time"

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

	require.NoError(t, killer.Kill())

	require.Eventually(t, func() bool {
		return !processAlive(cmd.Process.Pid) && !processAlive(childPID)
	}, 20*time.Second, 200*time.Millisecond, "parent and child should both be terminated")
}
