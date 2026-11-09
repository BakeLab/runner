// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func expectNodeProbe(cm *containerMock, major string, count int) {
	for range count {
		cm.On("ExecOutput", mock.Anything, mock.MatchedBy(func(args []string) bool {
			return len(args) == 5 && args[2] == nodeProbe
		})).Return(io.NopCloser(strings.NewReader(major)), nil).Once()
	}
}

func TestNodeRuntimeSelection(t *testing.T) {
	t.Run("matching installed runtime", func(t *testing.T) {
		cm := &containerMock{}
		expectNodeProbe(cm, "20", 1)
		rc := &RunContext{JobContainer: cm}
		executable, err := rc.nodeRuntime(t.Context(), model.ActionRunsUsingNode20, map[string]string{"PATH": "/custom/bin"})
		require.NoError(t, err)
		assert.Equal(t, "node", executable)
		cm.AssertExpectations(t)
	})
	for _, major := range []string{"missing", "16"} {
		t.Run(major+" needs managed runtime", func(t *testing.T) {
			cm := &containerMock{}
			expectNodeProbe(cm, major, 1)
			cm.On("ExecOutput", mock.Anything, []string{"uname", "-m"}).
				Return(io.NopCloser(strings.NewReader("riscv64")), nil).Once()
			rc := &RunContext{JobContainer: cm}
			_, err := rc.nodeRuntime(t.Context(), model.ActionRunsUsingNode20, nil)
			require.ErrorContains(t, err, "no managed node20 runtime for Linux riscv64")
			cm.AssertExpectations(t)
		})
	}
	t.Run("probe failure is not a missing runtime", func(t *testing.T) {
		cm := &containerMock{}
		cm.On("ExecOutput", mock.Anything, mock.Anything).Return(nil, assert.AnError).Once()
		rc := &RunContext{JobContainer: cm}
		_, err := rc.nodeRuntime(t.Context(), model.ActionRunsUsingNode20, nil)
		require.ErrorIs(t, err, assert.AnError)
		cm.AssertExpectations(t)
	})
	t.Run("composite shares job runtimes", func(t *testing.T) {
		cm := &containerMock{}
		parent := &RunContext{nodeRuntimes: map[model.ActionRunsUsing]string{
			model.ActionRunsUsingNode20: "/var/run/act/node/node20/node",
			model.ActionRunsUsingNode24: "/var/run/act/node/node24/node",
		}}
		rc := &RunContext{Parent: parent, JobContainer: cm}
		for using, want := range parent.nodeRuntimes {
			executable, err := rc.nodeRuntime(t.Context(), using, nil)
			require.NoError(t, err)
			assert.Equal(t, want, executable)
		}
		cm.AssertNotCalled(t, "ExecOutput", mock.Anything, mock.Anything)
	})
	t.Run("host mode is unchanged", func(t *testing.T) {
		rc := &RunContext{platformImage: "-self-hosted"}
		executable, err := rc.nodeRuntime(t.Context(), model.ActionRunsUsingNode20, nil)
		require.NoError(t, err)
		assert.Equal(t, "node", executable)
	})
}

func TestManagedNodeActionStages(t *testing.T) {
	for _, stage := range []string{"pre", "main", "post"} {
		t.Run(stage, func(t *testing.T) {
			cm := &containerMock{}
			executable := "/var/run/act/node/node20/node"
			env := map[string]string{"PATH": "/usr/bin", "STATE_key": "value"}
			cm.On("Exec", []string{executable, "--preserve-symlinks-main", "/workspace/action/" + stage + ".js"},
				mock.MatchedBy(func(env map[string]string) bool {
					return env["PATH"] == "/var/run/act/node/node20:/usr/bin" && env["STATE_key"] == "value"
				}), "", "").
				Return(noopExecutor).Once()
			rc := &RunContext{
				Config: &Config{}, JobContainer: cm,
				nodeRuntimes: map[model.ActionRunsUsing]string{model.ActionRunsUsingNode20: executable},
			}
			step := &stepActionLocal{
				Step: &model.Step{ID: "action", Uses: "./action"}, RunContext: rc, env: env,
				action: &model.Action{Runs: model.ActionRuns{
					Using: model.ActionRunsUsingNode20, Pre: "pre.js", Main: "main.js", Post: "post.js",
				}},
			}
			rc.Config.Workdir = "/workspace"
			rc.Config.PresetGitHubContext = &model.GithubContext{}
			rc.Run = &model.Run{JobID: "job", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"job": {}}}}
			switch stage {
			case "pre":
				require.NoError(t, runPreStep(step)(t.Context()))
			case "main":
				require.NoError(t, runActionImpl(step, "/workspace/action", nil)(t.Context()))
			case "post":
				require.NoError(t, runPostStep(step)(t.Context()))
			}
			assert.Equal(t, "/usr/bin", env["PATH"])
			cm.AssertExpectations(t)
		})
	}
}

func TestInstallNodeRuntime(t *testing.T) {
	for _, test := range []struct {
		name    string
		entry   string
		kind    byte
		badHash bool
		status  int
		wantErr string
	}{
		{name: "verified binary only", entry: "node-v20.0.0-linux-x64/bin/node", kind: tar.TypeReg},
		{name: "checksum mismatch", badHash: true, wantErr: "SHA256 mismatch"},
		{name: "missing binary", entry: "../node", kind: tar.TypeReg, wantErr: "missing bin/node"},
		{name: "symlink rejected", entry: "node-v20.0.0-linux-x64/bin/node", kind: tar.TypeSymlink, wantErr: "invalid bin/node"},
		{name: "HTTP error", status: http.StatusNotFound, wantErr: "HTTP 404"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var archive bytes.Buffer
			compressed := gzip.NewWriter(&archive)
			writer := tar.NewWriter(compressed)
			payload := []byte("node-binary")
			header := &tar.Header{Name: test.entry, Mode: 0o755, Typeflag: test.kind}
			if header.Name == "" {
				header.Name = "node-v20.0.0-linux-x64/bin/node"
			}
			if header.Typeflag == 0 {
				header.Typeflag = tar.TypeReg
			}
			if header.Typeflag == tar.TypeReg {
				header.Size = int64(len(payload))
			}
			require.NoError(t, writer.WriteHeader(header))
			if header.Size > 0 {
				_, err := writer.Write(payload)
				require.NoError(t, err)
			}
			require.NoError(t, writer.Close())
			require.NoError(t, compressed.Close())
			hash := sha256.Sum256(archive.Bytes())
			release := nodeRelease{version: "20.0.0", hashes: map[string]string{"x64": hex.EncodeToString(hash[:])}}
			if test.badHash {
				release.hashes["x64"] = strings.Repeat("0", 64)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v20.0.0/node-v20.0.0-linux-x64.tar.gz", r.URL.Path)
				if test.status != 0 {
					w.WriteHeader(test.status)
					return
				}
				_, err := w.Write(archive.Bytes())
				assert.NoError(t, err)
			}))
			defer server.Close()
			dest := t.TempDir()
			err := installNodeRuntime(t.Context(), &container.HostEnvironment{}, server.Client(), server.URL, release, "x64", dest)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				_, statErr := os.Stat(filepath.Join(dest, "node"))
				require.ErrorIs(t, statErr, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			data, err := os.ReadFile(filepath.Join(dest, "node"))
			require.NoError(t, err)
			assert.Equal(t, payload, data)
		})
	}
}

func TestNodeRuntimeProbeUsesStepPath(t *testing.T) {
	cm := &containerMock{}
	cm.On("ExecOutput", mock.Anything, []string{"sh", "-c", nodeProbe, "node-probe", "/step/bin"}).
		Return(io.NopCloser(strings.NewReader("24")), nil).Once()
	rc := &RunContext{JobContainer: cm}
	executable, err := rc.nodeRuntime(t.Context(), model.ActionRunsUsingNode24, map[string]string{"PATH": "/step/bin"})
	require.NoError(t, err)
	assert.Equal(t, "node", executable)
	cm.AssertExpectations(t)
}
