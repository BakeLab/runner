// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/internal/pkg/config"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

type serviceExit struct {
	specific bool
	code     uint32
}

func receiveServiceValue[T any](ctx context.Context, t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		var zero T
		return zero
	}
}

func TestDaemonServiceControl(t *testing.T) {
	for _, command := range []svc.Cmd{svc.Stop, svc.Shutdown, svc.PreShutdown} {
		t.Run(fmt.Sprint(command), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			release, finish := context.WithCancel(ctx)
			defer finish()
			cancelled := make(chan struct{})
			requests := make(chan svc.ChangeRequest, 1)
			ticks := make(chan time.Time)
			changes := make(chan svc.Status, 8)
			done := make(chan serviceExit, 1)
			handler := &daemonService{ctx: ctx, run: func(ctx context.Context) error {
				<-ctx.Done()
				close(cancelled)
				<-release.Done()
				return nil
			}}
			go func() {
				specific, code := handler.execute(requests, changes, ticks)
				done <- serviceExit{specific, code}
			}()

			require.Equal(t, svc.Status{State: svc.StartPending}, receiveServiceValue(ctx, t, changes))
			running := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown}
			require.Equal(t, running, receiveServiceValue(ctx, t, changes))
			requests <- svc.ChangeRequest{Cmd: svc.Interrogate}
			require.Equal(t, running, receiveServiceValue(ctx, t, changes))
			requests <- svc.ChangeRequest{Cmd: command}
			stopping := svc.Status{State: svc.StopPending, CheckPoint: 1, WaitHint: 10000}
			require.Equal(t, stopping, receiveServiceValue(ctx, t, changes))
			receiveServiceValue(ctx, t, cancelled)
			for range 2 {
				select {
				case ticks <- time.Time{}:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				stopping.CheckPoint++
				require.Equal(t, stopping, receiveServiceValue(ctx, t, changes))
			}
			requests <- svc.ChangeRequest{Cmd: command}
			requests <- svc.ChangeRequest{Cmd: svc.Interrogate}
			require.Equal(t, stopping, receiveServiceValue(ctx, t, changes))
			finish()
			require.Equal(t, serviceExit{}, receiveServiceValue(ctx, t, done))
			require.NoError(t, handler.err)
		})
	}
}

func TestDaemonServiceExit(t *testing.T) {
	for _, daemonErr := range []error{nil, errors.New("daemon failed")} {
		t.Run(fmt.Sprint(daemonErr), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			changes := make(chan svc.Status, 3)
			done := make(chan serviceExit, 1)
			handler := &daemonService{ctx: ctx, run: func(context.Context) error { return daemonErr }}
			go func() {
				specific, code := handler.Execute(nil, nil, changes)
				done <- serviceExit{specific, code}
			}()
			require.Equal(t, svc.StartPending, receiveServiceValue(ctx, t, changes).State)
			require.Equal(t, svc.Running, receiveServiceValue(ctx, t, changes).State)
			require.Equal(t, svc.Status{State: svc.StopPending}, receiveServiceValue(ctx, t, changes))
			if daemonErr == nil {
				require.Equal(t, serviceExit{}, receiveServiceValue(ctx, t, done))
			} else {
				require.Equal(t, serviceExit{true, 1}, receiveServiceValue(ctx, t, done))
			}
			require.Equal(t, daemonErr, handler.err)
		})
	}
}

func waitForServiceState(t *testing.T, service *mgr.Service, state svc.State) svc.Status {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := service.Query()
		require.NoError(t, err)
		if status.State == state {
			return status
		}
		if state == svc.Running && status.State == svc.Stopped {
			t.Fatalf("service stopped before running: %+v", status)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("waiting for service state %d, last status: %+v", state, status)
		}
	}
}

func TestWindowsService(t *testing.T) {
	isService, err := svc.IsWindowsService()
	require.NoError(t, err)
	if isService {
		require.NoError(t, runService(t.Context(), func(ctx context.Context) error {
			initLogging(&config.Config{})
			logging.Info("service started")
			<-ctx.Done()
			logging.Info("service stopped")
			return nil
		}))
		return
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("Windows service test requires elevation")
	}

	executable, err := os.Executable()
	require.NoError(t, err)
	binary, err := os.ReadFile(executable)
	require.NoError(t, err)
	directory := t.TempDir()
	serviceExecutable := filepath.Join(directory, "runner service.test.exe")
	require.NoError(t, os.WriteFile(serviceExecutable, binary, 0o600))
	manager, err := mgr.Connect()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Disconnect()) })
	service, err := manager.CreateService(
		fmt.Sprintf("gitea-runner-test-%d-%d", os.Getpid(), time.Now().UnixNano()),
		serviceExecutable, mgr.Config{StartType: mgr.StartManual}, "-test.run=^TestWindowsService$", "-test.timeout=1m",
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	t.Cleanup(func() { require.NoError(t, service.Delete()) })
	var process windows.Handle
	t.Cleanup(func() {
		if process != 0 {
			require.NoError(t, windows.CloseHandle(process))
		}
	})
	t.Cleanup(func() {
		if process == 0 {
			return
		}
		result, err := windows.WaitForSingleObject(process, 20000)
		require.NoError(t, err)
		if result == uint32(windows.WAIT_TIMEOUT) {
			t.Error("service process did not exit after stopping")
			require.NoError(t, windows.TerminateProcess(process, 1))
			result, err = windows.WaitForSingleObject(process, 20000)
			require.NoError(t, err)
		}
		require.Equal(t, uint32(windows.WAIT_OBJECT_0), result)
	})
	t.Cleanup(func() {
		status, err := service.Query()
		require.NoError(t, err)
		if status.State != svc.Stopped {
			_, err := service.Control(svc.Stop)
			require.NoError(t, err)
			waitForServiceState(t, service, svc.Stopped)
		}
	})
	require.NoError(t, service.Start())
	running := waitForServiceState(t, service, svc.Running)
	process, err = windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, running.ProcessId)
	require.NoError(t, err)
	_, err = service.Control(svc.Stop)
	require.NoError(t, err)
	stopped := waitForServiceState(t, service, svc.Stopped)
	require.Zero(t, stopped.Win32ExitCode)
	require.Zero(t, stopped.ServiceSpecificExitCode)
	log, err := os.ReadFile(filepath.Join(directory, "gitea-runner.log"))
	require.NoError(t, err)
	require.Contains(t, string(log), "service started")
	require.Contains(t, string(log), "service stopped")
}
