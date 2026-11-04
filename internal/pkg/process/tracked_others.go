// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !linux && !darwin

package process

// KillTrackedProcesses does nothing, Windows ends a job's processes through its job object.
func KillTrackedProcesses(_trackingID string) (int, error) {
	return 0, nil
}
