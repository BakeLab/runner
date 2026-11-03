// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !windows

package cmd

import "context"

func runService(ctx context.Context, run func(context.Context) error) error {
	return run(ctx)
}
