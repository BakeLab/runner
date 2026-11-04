// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestHostEnvironmentTrackingCleanup(t *testing.T) {
	for _, tracking := range []string{"default", "", "shared-helper"} {
		t.Run("tracking="+tracking, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
			defer stop()
			workspace := t.TempDir()
			environment := &HostEnvironment{Path: t.TempDir(), TmpDir: workspace, Workdir: workspace, TrackingID: "gitea_test", StdOut: io.Discard}
			env := map[string]string{}
			for _, entry := range os.Environ() {
				if name, value, ok := strings.Cut(entry, "="); ok && !strings.EqualFold(name, "RUNNER_TRACKING_ID") {
					env[name] = value
				}
			}
			env["GITEA_TRACKING_HELPER"] = "step"
			env["GITEA_TRACKING_ADDRESS"] = listener.Addr().String()
			if tracking == "default" {
				env["RUNNER_TRACKING_ID"] = environment.TrackingID
				env["runner_tracking_id"] = "shared-helper"
			} else {
				env["runner_tracking_id"] = tracking
			}
			result := make(chan error, 1)
			go func() {
				result <- environment.Exec([]string{os.Args[0], "-test.run=^TestHostTrackingHelper$"}, env, "", workspace)(ctx)
			}()
			var helpers []windows.Handle
			for range 2 {
				connection, err := listener.Accept()
				require.NoError(t, err)
				t.Cleanup(func() { _ = connection.Close() })
				require.NoError(t, connection.SetReadDeadline(time.Now().Add(20*time.Second)))
				var pid int
				_, err = fmt.Fscanln(connection, &pid)
				require.NoError(t, err)
				handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
				require.NoError(t, err)
				t.Cleanup(func() {
					_ = windows.TerminateProcess(handle, 1)
					_, _ = windows.WaitForSingleObject(handle, 5000)
					_ = windows.CloseHandle(handle)
				})
				helpers = append(helpers, handle)
			}
			select {
			case err := <-result:
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			require.NoError(t, environment.Remove()(ctx))
			for _, handle := range helpers {
				if tracking == "default" {
					result, err := windows.WaitForSingleObject(handle, 5000)
					require.NoError(t, err)
					require.EqualValues(t, windows.WAIT_OBJECT_0, result)
				} else {
					result, err := windows.WaitForSingleObject(handle, 0)
					require.NoError(t, err)
					require.EqualValues(t, windows.WAIT_TIMEOUT, result)
				}
			}
		})
	}
}

func TestHostTrackingHelper(t *testing.T) {
	mode := os.Getenv("GITEA_TRACKING_HELPER")
	if mode == "" {
		return
	}
	if mode != "descendant" {
		child := exec.Command(os.Args[0], "-test.run=^TestHostTrackingHelper$")
		child.Env = append(os.Environ(), "GITEA_TRACKING_HELPER="+map[string]string{"step": "helper", "helper": "descendant"}[mode])
		require.NoError(t, child.Start())
		if mode == "step" {
			os.Exit(0)
		}
	}
	connection, err := net.Dial("tcp4", os.Getenv("GITEA_TRACKING_ADDRESS"))
	require.NoError(t, err)
	_, err = fmt.Fprintln(connection, os.Getpid())
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, connection)
	require.NoError(t, err)
	require.NoError(t, connection.Close())
	os.Exit(0)
}
