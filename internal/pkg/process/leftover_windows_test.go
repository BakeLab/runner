// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package process

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func startSleeperIn(t *testing.T, system, dir, trackingID string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(filepath.Join(os.Getenv("SystemRoot"), system, "ping.exe"), "-n", "60", "127.0.0.1")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "RUNNER_TRACKING_ID="+trackingID)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func processCWD(pid uint32) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return "", err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	params, err := processParametersHandle(h)
	if err != nil {
		return "", err
	}
	return params.unicodeString(h, unsafe.Offsetof(windows.RTL_USER_PROCESS_PARAMETERS{}.CurrentDirectory), 0x24)
}

func awaitCWD(t *testing.T, pid int) string {
	t.Helper()
	var cwd string
	require.Eventually(t, func() bool {
		var err error
		cwd, err = processCWD(uint32(pid))
		return err == nil && cwd != ""
	}, 5*time.Second, 50*time.Millisecond, "process cwd should become readable")
	return cwd
}

func TestKillProcessesWithPathsTracking(t *testing.T) {
	for _, system := range []string{"System32", "SysWOW64"} {
		for _, trackingID := range []string{"leftover-test", "", "another-job"} {
			t.Run(system+"/tracking="+trackingID, func(t *testing.T) {
				if system == "SysWOW64" {
					if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
						t.Skip("needs a 64-bit runner")
					}
					if _, err := os.Stat(filepath.Join(os.Getenv("SystemRoot"), system, "ping.exe")); err != nil {
						t.Skip("32-bit Windows tools unavailable")
					}
				}
				dir := t.TempDir()
				cmd := startSleeperIn(t, system, dir, trackingID)
				require.True(t, dirContainsPath(dir, awaitCWD(t, cmd.Process.Pid)))
				killed, err := KillProcessesWithPaths(t.Context(), []string{t.TempDir()}, "leftover-test")
				require.NoError(t, err)
				require.Zero(t, killed)
				killed, err = KillProcessesWithPaths(t.Context(), []string{dir}, "leftover-test")
				require.NoError(t, err)
				if trackingID == "leftover-test" {
					require.Equal(t, 1, killed)
					require.Eventually(t, func() bool {
						return !processAlive(cmd.Process.Pid)
					}, 5*time.Second, 50*time.Millisecond)
				} else {
					require.Zero(t, killed)
					require.True(t, processAlive(cmd.Process.Pid))
				}
			})
		}
	}
}

func TestKillProcessesWithPathsCommandLine(t *testing.T) {
	workspace := t.TempDir()
	cmd, ready := newTestProcess(t, "child", false)
	cmd.Dir = t.TempDir()
	cmd.Args = append(cmd.Args, filepath.Join(workspace, "reference"))
	cmd.Env = append(cmd.Env, "RUNNER_TRACKING_ID=command-test")
	require.NoError(t, cmd.Start())
	ready()
	killed, err := KillProcessesWithPaths(t.Context(), []string{workspace}, "command-test")
	require.NoError(t, err)
	require.Equal(t, 1, killed)
	require.Eventually(t, func() bool {
		return !processAlive(cmd.Process.Pid)
	}, 5*time.Second, 50*time.Millisecond)
}
