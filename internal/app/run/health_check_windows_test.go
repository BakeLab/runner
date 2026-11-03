// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExecuteHealthCheckExeAndBatchPathsWithSpace(t *testing.T) {
	whoami, err := os.ReadFile(filepath.Join(os.Getenv("SystemRoot"), "System32", "whoami.exe"))
	require.NoError(t, err)
	dir := filepath.Join(t.TempDir(), "health check")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "probe.exe"), whoami, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "probe.cmd"), []byte("@exit /b 0\r\n"), 0o755))
	for _, name := range []string{"probe.exe", "probe.cmd"} {
		require.NoError(t, executeHealthCheck(t.Context(), filepath.Join(dir, name), time.Minute, nil))
	}
}
