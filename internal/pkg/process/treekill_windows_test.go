// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package process

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestTreeKillWaitTrackingLeftoverOutput(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(map[bool]string{false: "tracked", true: "opted out"}[keep], func(t *testing.T) {
			cmd, ready := newTestProcess(t, "orphan", true)
			tree := NewTreeKill(cmd)
			tree.KeepLeftovers = keep
			cmd.WaitDelay = 50 * time.Millisecond
			require.NoError(t, cmd.Start())
			killer, err := tree.Capture(cmd.Process)
			require.NoError(t, err)
			defer killer.Close()
			childPID := ready()
			handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(childPID))
			require.NoError(t, err)
			t.Cleanup(func() {
				_ = windows.TerminateProcess(handle, 1)
				_, _ = windows.WaitForSingleObject(handle, 5000)
				_ = windows.CloseHandle(handle)
			})
			require.NoError(t, tree.Wait(t.Context(), cmd))
			if keep {
				result, err := windows.WaitForSingleObject(handle, 0)
				require.NoError(t, err)
				require.EqualValues(t, windows.WAIT_TIMEOUT, result)
			} else {
				result, err := windows.WaitForSingleObject(handle, 5000)
				require.NoError(t, err)
				require.EqualValues(t, windows.WAIT_OBJECT_0, result)
			}
		})
	}
}
