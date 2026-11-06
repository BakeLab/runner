// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package uploadartifact

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/artifacts"
	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/internal/pkg/action"
	"gitea.com/gitea/runner/internal/pkg/action/artifact"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type localContainer struct {
	*container.HostEnvironment
	bin string
}

func (c localContainer) ExecOutput(ctx context.Context, command []string) (io.ReadCloser, error) {
	if c.bin != "" {
		command = append([]string{"env", "PATH=" + c.bin}, command...)
	}
	return c.HostEnvironment.ExecOutput(ctx, command)
}

type harness struct {
	t                *testing.T
	server           string
	client           *artifact.Client
	workspace        string
	env              container.ExecutionsEnvironment
	logs, annotation []string
	retentionDays    string
	variables        map[string]string
	output           string
}

func newHarness(t *testing.T, env container.ExecutionsEnvironment) *harness {
	t.Helper()
	router := http.NewServeMux()
	artifacts.RegisterV4(router, t.TempDir())
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	client, err := artifact.New(server.URL, artifacts.LocalRuntimeToken("7", time.Hour), nil)
	require.NoError(t, err)
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return &harness{t: t, server: server.URL, client: client, workspace: workspace, env: env}
}

func (h *harness) write(rel, content string, mode os.FileMode) {
	h.t.Helper()
	p := filepath.Join(h.workspace, filepath.FromSlash(rel))
	require.NoError(h.t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(h.t, os.WriteFile(p, []byte(content), mode))
}

func (h *harness) run(inputs map[string]string) error {
	h.logs, h.annotation = nil, nil
	h.output = filepath.Join(h.t.TempDir(), "output")
	variables := map[string]string{"GITHUB_OUTPUT": h.output}
	maps.Copy(variables, h.variables)
	return Main(h.t.Context(), &action.Context{
		Container:    h.env,
		Github:       &model.GithubContext{ServerURL: "https://gitea.example", Repository: "owner/repo", RunID: "7", RunAttempt: "2", RetentionDays: h.retentionDays},
		Inputs:       inputs,
		Workspace:    h.workspace,
		Env:          variables,
		ResultsURL:   h.server,
		RuntimeToken: artifacts.LocalRuntimeToken("7", time.Hour),
		Infof:        func(format string, args ...any) { h.logs = append(h.logs, fmt.Sprintf(format, args...)) },
		Warning: func(format string, args ...any) {
			h.annotation = append(h.annotation, "warning: "+fmt.Sprintf(format, args...))
		},
	})
}

func (h *harness) fetch(name string) ([]byte, map[string]string) {
	h.t.Helper()
	signed, err := h.client.SignedURL(h.t.Context(), name)
	require.NoError(h.t, err)
	resp, err := h.client.Download(h.t.Context(), signed, time.Minute)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	content, err := io.ReadAll(resp.Body)
	require.NoError(h.t, err)
	reader, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return content, nil
	}
	entries := map[string]string{}
	for _, f := range reader.File {
		body, err := f.Open()
		require.NoError(h.t, err)
		data, err := io.ReadAll(body)
		require.NoError(h.t, err)
		require.NoError(h.t, body.Close())
		entries[f.Name] = string(data)
	}
	return content, entries
}

func TestUploadSelectsWhatTheToolkitSelects(t *testing.T) {
	envs := map[string]container.ExecutionsEnvironment{"host": &container.HostEnvironment{}}
	if runtime.GOOS != "windows" {
		envs["container gzip"] = localContainer{HostEnvironment: &container.HostEnvironment{}}
		bin := t.TempDir()
		for _, name := range []string{"sh", "find", "readlink", "tar"} {
			binary, err := exec.LookPath(name)
			require.NoError(t, err)
			require.NoError(t, os.Symlink(binary, filepath.Join(bin, name)))
		}
		envs["container plain"] = localContainer{HostEnvironment: &container.HostEnvironment{}, bin: bin}
	}
	for envName, env := range envs {
		t.Run(envName, func(t *testing.T) {
			h := newHarness(t, env)
			h.write("dist/a.txt", "a", 0o644)
			h.write("dist/run.sh", "#!/bin/sh\n", 0o755)
			h.write("dist/sub/b.txt", "b", 0o644)
			h.write("dist/empty", "", 0o644)
			require.NoError(t, os.Mkdir(filepath.Join(h.workspace, "dist", "emptydir"), 0o755))
			h.write("dist/.hidden", "h", 0o644)
			h.write("dist/.git/config", "c", 0o644)
			h.write("logs/x.log", "x", 0o644)
			h.write(".store/target.txt", "hidden target", 0o644)
			if runtime.GOOS != "windows" {
				require.NoError(t, os.Symlink("a.txt", filepath.Join(h.workspace, "dist", "link.txt")))
				require.NoError(t, os.Symlink("sub", filepath.Join(h.workspace, "dist", "linkdir")))
				require.NoError(t, os.Symlink("../.store", filepath.Join(h.workspace, "dist", "alias")))
				require.NoError(t, os.Symlink("missing", filepath.Join(h.workspace, "dist", "broken")))
				require.NoError(t, os.Symlink("..", filepath.Join(h.workspace, "dist", "loop")))
				require.NoError(t, os.Link(filepath.Join(h.workspace, "dist", "a.txt"), filepath.Join(h.workspace, "dist", "hard.txt")))
			}

			for _, test := range []struct {
				name     string
				inputs   map[string]string
				expected map[string]string
				links    map[string]string
			}{
				{
					name:     "a directory",
					inputs:   map[string]string{"path": "dist"},
					expected: map[string]string{"a.txt": "a", "run.sh": "#!/bin/sh\n", "sub/b.txt": "b", "empty": ""},
					links:    map[string]string{"link.txt": "a", "hard.txt": "a", "linkdir/b.txt": "b", "loop/logs/x.log": "x", "alias/target.txt": "hidden target"},
				},
				{
					name:     "hidden files included",
					inputs:   map[string]string{"path": "dist/", "include-hidden-files": "true"},
					expected: map[string]string{"a.txt": "a", "run.sh": "#!/bin/sh\n", "sub/b.txt": "b", ".hidden": "h", ".git/config": "c", "empty": ""},
					links:    map[string]string{"link.txt": "a", "hard.txt": "a", "linkdir/b.txt": "b", "loop/logs/x.log": "x", "loop/.store/target.txt": "hidden target", "alias/target.txt": "hidden target"},
				},
				{
					name:     "only exclusions and comments",
					inputs:   map[string]string{"path": "!dist\n# dist"},
					expected: map[string]string{},
				},
				{
					name:     "several patterns share their common ancestor as root",
					inputs:   map[string]string{"path": "dist/**/*.txt\n!dist/sub/**\n# a comment\nlogs/x.log"},
					expected: map[string]string{"dist/a.txt": "a", "logs/x.log": "x"},
					links:    map[string]string{"dist/link.txt": "a", "dist/hard.txt": "a", "dist/linkdir/b.txt": "b", "dist/alias/target.txt": "hidden target"},
				},
				{
					name:     "hidden root skipped",
					inputs:   map[string]string{"path": ".store"},
					expected: map[string]string{},
				},
				{
					name:     "a single file keeps no directory",
					inputs:   map[string]string{"path": h.workspace + "/dist/sub/b.txt"},
					expected: map[string]string{"b.txt": "b"},
				},
			} {
				t.Run(test.name, func(t *testing.T) {
					inputs := map[string]string{"name": "art", "overwrite": "true"}
					maps.Copy(inputs, test.inputs)
					require.NoError(t, h.run(inputs))
					expected := test.expected
					if runtime.GOOS != "windows" {
						maps.Copy(expected, test.links)
					}
					if len(expected) == 0 {
						assert.Contains(t, h.annotation, "warning: No files were found with the provided path: "+test.inputs["path"]+". No artifacts will be uploaded.")
						return
					}
					_, entries := h.fetch("art")
					assert.Equal(t, expected, entries)
				})
			}
		})
	}
}

func TestUploadOptions(t *testing.T) {
	h := newHarness(t, &container.HostEnvironment{})
	h.write("out/run.sh", "#!/bin/sh\n", 0o755)
	h.write("out/data.txt", strings.Repeat("data", 1000), 0o644)
	h.variables = map[string]string{"ACTIONS_ARTIFACT_UPLOAD_CONCURRENCY": " 2 workers", "ACTIONS_ARTIFACT_UPLOAD_TIMEOUT_MS": " 60000ms"}
	h.retentionDays = "5"
	require.NoError(t, h.run(map[string]string{"name": "packed", "path": "out", "compression-level": "0", "retention-days": "10 days"}))
	assert.Equal(t, []string{"warning: Retention days cannot be greater than the maximum allowed retention set within the repository. Using 5 instead."}, h.annotation)

	content, _ := h.fetch("packed")
	reader, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	require.NoError(t, err)
	for _, f := range reader.File {
		assert.Equal(t, zip.Store, f.Method, f.Name)
		if runtime.GOOS != "windows" && f.Name == "run.sh" {
			assert.Equal(t, os.FileMode(0o755), f.Mode().Perm())
		}
	}

	list, err := h.client.List(t.Context(), "")
	require.NoError(t, err)
	require.Len(t, list, 1)
	outputs, err := os.ReadFile(h.output)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("artifact-id=%d\nartifact-digest=%s\nartifact-url=https://gitea.example/owner/repo/actions/runs/7/artifacts/packed?attempt=2\n", list[0].ID, strings.TrimPrefix(list[0].Digest, "sha256:")), string(outputs))

	h.write("report.html", "<html></html>", 0o644)

	require.NoError(t, h.run(map[string]string{"name": "packed", "path": "report.html", "archive": "false", "overwrite": "true"}))
	list, err = h.client.List(t.Context(), "packed")
	require.NoError(t, err)
	assert.Empty(t, list)
	content, entries := h.fetch("report.html")
	assert.Nil(t, entries)
	assert.Equal(t, "<html></html>", string(content))
	assert.Contains(t, h.logs, "Uploading artifact: report.html")
	assert.Contains(t, strings.Join(h.logs, "\n"), "Artifact packed has been successfully uploaded!")
	require.ErrorContains(t, h.run(map[string]string{"path": "out", "archive": "false"}), "When 'archive' is set to false, only a single file can be uploaded. Found 2 files to upload.")

	require.NoError(t, h.run(map[string]string{"path": "nothing/here"}))
	assert.Equal(t, []string{"warning: No files were found with the provided path: nothing/here. No artifacts will be uploaded."}, h.annotation)
	require.NoError(t, h.run(map[string]string{"path": "nothing", "if-no-files-found": "ignore"}))
	assert.Contains(t, h.logs, "No files were found with the provided path: nothing. No artifacts will be uploaded.")
	require.ErrorContains(t, h.run(map[string]string{"path": "nothing", "if-no-files-found": "error"}), "No files were found")

	for _, test := range []struct {
		inputs map[string]string
		want   string
	}{
		{map[string]string{"name": "a/b", "path": "out/data.txt"}, "The artifact name is not valid: a/b"},
		{map[string]string{}, "Input required and not supplied: path"},
		{map[string]string{"name": "", "path": "out/data.txt"}, "Provided artifact name input during validation is empty"},
		{map[string]string{"path": "out/data.txt", "overwrite": ""}, "Core Schema"},
		{map[string]string{"path": "out/data.txt", "compression-level": "10"}, "Invalid compression-level. Valid values are 0-9"},
		{map[string]string{"path": "out/data.txt", "compression-level": "+-1"}, "Invalid compression-level"},
		{map[string]string{"path": "out/data.txt", "if-no-files-found": "fail"}, "Unrecognized if-no-files-found input"},
	} {
		require.ErrorContains(t, h.run(test.inputs), test.want)
	}
	require.NoError(t, h.run(map[string]string{"path": "out/data.txt", "retention": "1", "overwrite": "true"}))
	assert.Equal(t, []string{"warning: Unexpected input(s) 'retention', valid inputs are ['name', 'path', 'if-no-files-found', 'retention-days', 'compression-level', 'overwrite', 'include-hidden-files', 'archive']"}, h.annotation)
	if runtime.GOOS != "windows" {
		h.write("bad/a:b.txt", "x", 0o644)
		require.ErrorContains(t, h.run(map[string]string{"path": "bad"}), "The path for one of the files in artifact is not valid: /a:b.txt")
	}
}
