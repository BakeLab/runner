// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build linux || darwin

package process

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKillTrackedProcessesKillsOnlyTheJobsProcesses(t *testing.T) {
	trackingID := "gitea_test_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	start := func(id string) *exec.Cmd {
		cmd := exec.Command("sleep", "60")
		cmd.Env = append(os.Environ(), "RUNNER_TRACKING_ID="+id)
		require.NoError(t, cmd.Start())
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
		return cmd
	}
	tracked, other := start(trackingID), start(trackingID+"_other")

	killed, err := KillTrackedProcesses(trackingID)
	require.NoError(t, err)
	require.Equal(t, 1, killed)
	require.ErrorContains(t, tracked.Wait(), "killed")
	require.NoError(t, other.Process.Signal(syscall.Signal(0)))
}
