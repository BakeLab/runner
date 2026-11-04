// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package lookpath

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLookPath2IgnoresTheCurrentDirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tool.exe"), nil, 0o755))
	t.Chdir(dir)
	_, err := LookPath2("tool", map[string]string{"PATH": t.TempDir()})
	require.EqualError(t, err, ErrNotFound.Error())
}
