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
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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

func writeFakeNode(t *testing.T, dir, major string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node"), []byte("#!/bin/sh\necho "+major+"\n"), 0o755))
}

type hostNodeServer struct {
	source   hostNodeSource
	requests atomic.Int32
}

func newHostNodeServer(t *testing.T, goos, goarch, script string) *hostNodeServer {
	t.Helper()
	var archive bytes.Buffer
	compressed := gzip.NewWriter(&archive)
	writer := tar.NewWriter(compressed)
	payload := []byte(script)
	require.NoError(t, writer.WriteHeader(&tar.Header{Name: "node-v20.0.0-linux-x64/bin/node", Mode: 0o755, Size: int64(len(payload)), Typeflag: tar.TypeReg}))
	_, err := writer.Write(payload)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	require.NoError(t, compressed.Close())
	hash := sha256.Sum256(archive.Bytes())
	s := &hostNodeServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		assert.Equal(t, "/v20.0.0/node-v20.0.0-linux-x64.tar.gz", r.URL.Path)
		_, err := w.Write(archive.Bytes())
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	s.source = hostNodeSource{
		goos: goos, goarch: goarch, client: server.Client(), baseURL: server.URL,
		releases: map[model.ActionRunsUsing]nodeRelease{
			model.ActionRunsUsingNode20: {"20.0.0", map[string]string{"x64": hex.EncodeToString(hash[:])}},
		},
	}
	return s
}

const fakeManagedNode = "#!/bin/sh\necho v20.0.0\n"

func newHostRunContext(cacheDir string) *RunContext {
	return &RunContext{Config: &Config{ActionCacheDir: cacheDir}, platformImage: "-self-hosted"}
}

func TestHostNodeRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed host runtimes are Linux only")
	}
	managedPath := func(cacheDir string) string {
		return filepath.Join(cacheDir, "node", "node20", "v20.0.0-linux-x64", "node")
	}
	t.Run("matching PATH node is used", func(t *testing.T) {
		bin := t.TempDir()
		writeFakeNode(t, bin, "20")
		server := newHostNodeServer(t, "linux", "amd64", fakeManagedNode)
		rc := newHostRunContext(t.TempDir())
		executable, err := rc.hostNodeRuntime(t.Context(), model.ActionRunsUsingNode20, map[string]string{"PATH": bin}, server.source)
		require.NoError(t, err)
		assert.Equal(t, "node", executable)
		assert.Zero(t, server.requests.Load())
	})
	for name, major := range map[string]string{"missing": "", "mismatched": "16"} {
		t.Run(name+" PATH node installs and reuses managed runtime", func(t *testing.T) {
			bin := t.TempDir()
			if major != "" {
				writeFakeNode(t, bin, major)
			}
			env := map[string]string{"PATH": bin}
			cacheDir := t.TempDir()
			server := newHostNodeServer(t, "linux", "amd64", fakeManagedNode)
			executable, err := newHostRunContext(cacheDir).hostNodeRuntime(t.Context(), model.ActionRunsUsingNode20, env, server.source)
			require.NoError(t, err)
			assert.Equal(t, managedPath(cacheDir), executable)
			assert.Equal(t, "v20.0.0", hostNodeVersion(t.Context(), executable))
			entries, err := os.ReadDir(filepath.Dir(filepath.Dir(executable)))
			require.NoError(t, err)
			assert.Len(t, entries, 1, "staging directories must not be left behind")

			rc := newHostRunContext(cacheDir)
			executable, err = rc.hostNodeRuntime(t.Context(), model.ActionRunsUsingNode20, env, server.source)
			require.NoError(t, err)
			assert.Equal(t, managedPath(cacheDir), executable)
			assert.Equal(t, int32(1), server.requests.Load(), "a later job reuses the cached runtime")
			assert.Equal(t, executable, rc.nodeRuntimes[model.ActionRunsUsingNode20])
		})
	}
	for _, platform := range [][2]string{{"darwin", "arm64"}, {"windows", "amd64"}, {"linux", "riscv64"}, {"linux", "386"}} {
		t.Run(platform[0]+"/"+platform[1]+" keeps PATH node", func(t *testing.T) {
			server := newHostNodeServer(t, platform[0], platform[1], fakeManagedNode)
			executable, err := newHostRunContext(t.TempDir()).hostNodeRuntime(t.Context(), model.ActionRunsUsingNode20, map[string]string{"PATH": t.TempDir()}, server.source)
			require.NoError(t, err)
			assert.Equal(t, "node", executable)
			assert.Zero(t, server.requests.Load())
		})
	}
	t.Run("unpinned runs.using keeps PATH node", func(t *testing.T) {
		server := newHostNodeServer(t, "linux", "amd64", fakeManagedNode)
		executable, err := newHostRunContext(t.TempDir()).hostNodeRuntime(t.Context(), model.ActionRunsUsingNode24, map[string]string{"PATH": t.TempDir()}, server.source)
		require.NoError(t, err)
		assert.Equal(t, "node", executable)
		assert.Zero(t, server.requests.Load())
	})
	t.Run("failed install falls back to mismatched PATH node", func(t *testing.T) {
		bin := t.TempDir()
		writeFakeNode(t, bin, "16")
		server := newHostNodeServer(t, "linux", "amd64", fakeManagedNode)
		server.source.releases[model.ActionRunsUsingNode20].hashes["x64"] = strings.Repeat("0", 64)
		cacheDir := t.TempDir()
		executable, err := newHostRunContext(cacheDir).hostNodeRuntime(t.Context(), model.ActionRunsUsingNode20, map[string]string{"PATH": bin}, server.source)
		require.NoError(t, err)
		assert.Equal(t, "node", executable)
		entries, err := os.ReadDir(filepath.Join(cacheDir, "node", "node20"))
		require.NoError(t, err)
		assert.Empty(t, entries, "an unverified runtime must not be installed")
	})
	t.Run("failed install without PATH node is an error", func(t *testing.T) {
		server := newHostNodeServer(t, "linux", "amd64", "#!/bin/sh\nexit 1\n")
		_, err := newHostRunContext(t.TempDir()).hostNodeRuntime(t.Context(), model.ActionRunsUsingNode20, map[string]string{"PATH": t.TempDir()}, server.source)
		require.ErrorContains(t, err, "official binaries require glibc")
	})
	t.Run("concurrent installers converge", func(t *testing.T) {
		server := newHostNodeServer(t, "linux", "amd64", fakeManagedNode)
		release := server.source.releases[model.ActionRunsUsingNode20]
		root := filepath.Join(t.TempDir(), "node", "node20")
		var wg sync.WaitGroup
		results := make([]string, 8)
		errs := make([]error, len(results))
		for i := range results {
			wg.Go(func() {
				results[i], errs[i] = ensureHostNodeRuntime(t.Context(), server.source.client, server.source.baseURL, root, release, "x64")
			})
		}
		wg.Wait()
		for i := range results {
			require.NoError(t, errs[i])
			assert.Equal(t, filepath.Join(root, "v20.0.0-linux-x64", "node"), results[i])
		}
		entries, err := os.ReadDir(root)
		require.NoError(t, err)
		assert.Len(t, entries, 1)
		assert.Equal(t, "v20.0.0", hostNodeVersion(t.Context(), results[0]))
	})
	t.Run("unusable cached runtime is not replaced", func(t *testing.T) {
		server := newHostNodeServer(t, "linux", "amd64", fakeManagedNode)
		root := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(root, "v20.0.0-linux-x64"), 0o755))
		_, err := ensureHostNodeRuntime(t.Context(), server.source.client, server.source.baseURL, root, server.source.releases[model.ActionRunsUsingNode20], "x64")
		require.ErrorContains(t, err, "remove that directory")
		assert.Zero(t, server.requests.Load())
	})
}

func TestHostManagedNodeAction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed host runtimes are Linux only")
	}
	dir := t.TempDir()
	executable := filepath.Join(dir, "node")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s|%s' \"$*\" \"$PATH\" > \"$OUT\"\n"), 0o755))
	out := filepath.Join(dir, "out")
	rc := &RunContext{
		platformImage: "-self-hosted",
		JobContainer:  &container.HostEnvironment{Path: dir, StdOut: io.Discard},
		nodeRuntimes:  map[model.ActionRunsUsing]string{model.ActionRunsUsingNode20: executable},
	}
	require.NoError(t, rc.execNodeAction(t.Context(), model.ActionRunsUsingNode20, "/action/main.js", map[string]string{"PATH": "/usr/bin:/bin", "OUT": out}))
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "--preserve-symlinks-main /action/main.js|"+dir+":/usr/bin:/bin", string(data))
}
