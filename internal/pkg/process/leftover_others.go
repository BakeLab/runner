// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !windows

package process

import "context"

func KillProcessesWithPaths(_ctx context.Context, _dirs []string, _trackingID string) (int, error) {
	return 0, nil
}
