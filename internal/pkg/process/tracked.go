// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build linux || darwin

package process

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"syscall"
)

// KillTrackedProcesses kills every process whose environment carries the job's RUNNER_TRACKING_ID, like GitHub's orphan cleanup.
func KillTrackedProcesses(trackingID string) (int, error) {
	if trackingID == "" {
		return 0, nil
	}
	pids, err := listPIDs()
	if err != nil {
		return 0, err
	}
	marker := []byte("RUNNER_TRACKING_ID=" + trackingID)
	self := os.Getpid()
	var killed int
	var errs []error
	for _, pid := range pids {
		if pid == self {
			continue
		}
		env, err := environment(pid)
		if err != nil || !slices.ContainsFunc(bytes.Split(env, []byte{0}), func(entry []byte) bool { return bytes.Equal(entry, marker) }) {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, fmt.Errorf("kill pid %d: %w", pid, err))
			continue
		}
		killed++
	}
	return killed, errors.Join(errs...)
}
