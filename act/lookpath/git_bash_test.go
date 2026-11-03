// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package lookpath

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookPathWindowsFallsBackToGitBash(t *testing.T) {
	root := "C:" + string(filepath.Separator)
	system32 := filepath.Join(root, "Windows", "System32")
	gitCmd, gitBin := filepath.Join(root, "Git", "cmd"), filepath.Join(root, "Git", "bin")
	msys := filepath.Join(root, "msys64", "usr", "bin")
	for _, test := range []struct {
		name, command, want string
		path                []string
	}{
		{"wsl launcher first", "bash", filepath.Join(gitBin, "bash.exe"), []string{system32, gitCmd}},
		{"no bash on path", "BASH.EXE", filepath.Join(gitBin, "bash.exe"), []string{gitCmd}},
		{"own bash first", "bash", filepath.Join(msys, "bash.exe"), []string{msys, system32, gitCmd}},
		{"no git", "bash", filepath.Join(system32, "bash.exe"), []string{system32}},
		{"other command", "sh", filepath.Join(system32, "sh.exe"), []string{system32, gitCmd}},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := map[string]bool{
				filepath.Join(system32, "bash.exe"): true, filepath.Join(system32, "sh.exe"): true,
				filepath.Join(gitCmd, "git.exe"): true, filepath.Join(gitBin, "bash.exe"): true, filepath.Join(msys, "bash.exe"): true,
			}
			lookup := func(file string, env map[string]string) (string, error) {
				candidates := []string{file}
				if !strings.ContainsAny(file, `:\/`) {
					candidates = nil
					for directory := range strings.SplitSeq(env["PATH"], ";") {
						candidates = append(candidates, filepath.Join(directory, strings.TrimSuffix(strings.ToLower(file), ".exe")+".exe"))
					}
				}
				for _, candidate := range candidates {
					if files[filepath.Clean(candidate)] {
						return filepath.Clean(candidate), nil
					}
				}
				return "", fs.ErrNotExist
			}
			got, err := lookPathWindows(test.command, map[string]string{"PATH": strings.Join(test.path, ";")}, lookup)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}
