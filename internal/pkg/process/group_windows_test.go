// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package process

import (
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGroupClosePropagatesToOrphan(t *testing.T) {
	group, err := NewGroup()
	require.NoError(t, err)
	t.Cleanup(func() { _ = group.Close() })

	cmd, ready := newTestProcess(t, "orphan", false)
	require.NoError(t, cmd.Start())
	require.NoError(t, group.Assign(cmd.Process))

	childPID := ready()

	require.NoError(t, cmd.Wait())
	require.Eventually(t, func() bool {
		return !processAlive(cmd.Process.Pid)
	}, 20*time.Second, 200*time.Millisecond, "the step process should have exited on its own")

	require.True(t, processAlive(childPID), "orphan should outlive its parent")

	require.NoError(t, group.Close())
	require.Eventually(t, func() bool {
		return !processAlive(childPID)
	}, 20*time.Second, 200*time.Millisecond, "closing the job must terminate the orphan")
}

// TestGroupNestsWithKiller: assigned to the group first and the step's Killer
// second, cancelling the step still kills exactly that step's tree.
func TestGroupNestsWithKiller(t *testing.T) {
	group, err := NewGroup()
	require.NoError(t, err)
	t.Cleanup(func() { _ = group.Close() })

	cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", "Start-Sleep -Seconds 600")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	require.NoError(t, group.Assign(cmd.Process))

	// Nesting must be accepted, else cancellation falls back to a single-process kill.
	killer, err := NewKiller(cmd.Process)
	require.NoError(t, err, "the step's job object must nest inside the group's")
	defer killer.Close()

	require.NoError(t, killer.Kill())
	require.Eventually(t, func() bool {
		return !processAlive(cmd.Process.Pid)
	}, 20*time.Second, 200*time.Millisecond, "cancelling the step should still kill its tree")
}

// TestGroupNilSafe covers the fallback path: when the job object cannot be
// created the caller holds a nil *Group and must still be able to use it.
func TestGroupNilSafe(t *testing.T) {
	var group *Group
	require.NoError(t, group.Assign(nil))
	require.NoError(t, group.Close())
	require.NoError(t, group.Close())
}
