// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package downloadartifact

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"gitea.com/gitea/runner/act/artifacts"
	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/internal/pkg/action"
	"gitea.com/gitea/runner/internal/pkg/action/artifact"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type zipEntry struct {
	name, body string
	mode       os.FileMode
}

func zipOf(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		header := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		header.SetMode(e.mode)
		w, err := zw.CreateHeader(header)
		require.NoError(t, err)
		_, err = io.WriteString(w, e.body)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

type harness struct {
	t                *testing.T
	server, store    string
	client           *artifact.Client
	workspace        string
	env              map[string]string
	mu               sync.Mutex
	logs, annotation []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store := t.TempDir()
	router := http.NewServeMux()
	artifacts.RegisterV4(router, store)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	client, err := artifact.New(server.URL, artifacts.LocalRuntimeToken("7", time.Hour), nil)
	require.NoError(t, err)
	return &harness{t: t, server: server.URL, store: store, client: client, workspace: t.TempDir()}
}

func (h *harness) upload(name, mimeType string, content []byte) int64 {
	h.t.Helper()
	ctx := h.t.Context()
	signed, err := h.client.Create(ctx, name, mimeType, nil)
	require.NoError(h.t, err)
	uploaded, err := h.client.Upload(ctx, signed, mimeType, bytes.NewReader(content), 2, time.Minute)
	require.NoError(h.t, err)
	id, err := h.client.Finalize(ctx, name, uploaded.Size, uploaded.Digest)
	require.NoError(h.t, err)
	return id
}

func (h *harness) run(inputs map[string]string) error {
	h.logs, h.annotation = nil, nil
	env := map[string]string{"GITHUB_OUTPUT": filepath.Join(h.workspace, "output")}
	maps.Copy(env, h.env)
	return Main(h.t.Context(), &action.Context{
		Container:    &container.HostEnvironment{},
		Github:       &model.GithubContext{Repository: "owner/repo", RunID: "7"},
		Inputs:       inputs,
		Workspace:    h.workspace,
		Env:          env,
		ResultsURL:   h.server,
		RuntimeToken: artifacts.LocalRuntimeToken("7", time.Hour),
		Infof: func(format string, args ...any) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.logs = append(h.logs, fmt.Sprintf(format, args...))
		},
		Warning: func(format string, args ...any) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.annotation = append(h.annotation, "warning: "+fmt.Sprintf(format, args...))
		},
	})
}

func (h *harness) read(rel string) string {
	h.t.Helper()
	content, err := os.ReadFile(filepath.Join(h.workspace, filepath.FromSlash(rel)))
	require.NoError(h.t, err)
	return string(content)
}

func TestDownload(t *testing.T) {
	h := newHarness(t)
	first := h.upload("alpha", "", zipOf(t, zipEntry{name: "a.txt", body: "a1"}))
	_, err := h.client.Delete(t.Context(), "alpha")
	require.NoError(t, err)
	archive := zipOf(t,
		zipEntry{name: "run.sh", body: "#!/bin/sh\n", mode: 0o755},
		zipEntry{name: "empty/", mode: os.ModeDir | 0o755},
		zipEntry{name: "nested/../a.txt", body: "a2"},
		zipEntry{name: "empty.txt"},
	)
	second := h.upload("alpha", "", archive)
	h.upload("beta ü.txt", "text/plain", []byte("plain text"))
	h.upload(".hidden", "", zipOf(t, zipEntry{name: "h.txt", body: "h"}))

	for _, test := range []struct {
		inputs map[string]string
		files  map[string]string
	}{
		{map[string]string{"name": "alpha", "path": "named"}, map[string]string{"named/a.txt": "a2", "named/run.sh": "#!/bin/sh\n", "named/empty.txt": ""}},
		{map[string]string{"path": "all"}, map[string]string{"all/alpha/a.txt": "a2", "all/beta ü.txt/beta ü.txt": "plain text", "all/.hidden/h.txt": "h"}},
		{map[string]string{"path": "merged", "merge-multiple": "true"}, map[string]string{"merged/a.txt": "a2", "merged/beta ü.txt": "plain text", "merged/h.txt": "h"}},
		{map[string]string{"path": "pattern", "pattern": "*"}, map[string]string{"pattern/alpha/a.txt": "a2", "pattern/beta ü.txt/beta ü.txt": "plain text"}},
		{map[string]string{"path": "ids", "artifact-ids": fmt.Sprintf("%dtrailing, %d", second, first), "pattern": "ignored"}, map[string]string{"ids/a.txt": "a2"}},
		{map[string]string{"name": "alpha", "path": "kept", "skip-decompress": "true"}, map[string]string{"kept/alpha.zip": string(archive)}},
		{map[string]string{"path": "none", "pattern": "nope-*", "repository": "ignored", "run-id": "invalid"}, nil},
	} {
		require.NoError(t, h.run(test.inputs))
		assert.Contains(t, h.read("output"), "download-path="+filepath.Join(h.workspace, test.inputs["path"])+"\n")
		for name, body := range test.files {
			assert.Equal(t, body, h.read(name))
		}
	}
	assert.Contains(t, h.logs, "Total of 0 artifact(s) downloaded")
	assert.NoDirExists(t, filepath.Join(h.workspace, "pattern", ".hidden"))
	assert.DirExists(t, filepath.Join(h.workspace, "named", "empty"))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(h.workspace, "named", "run.sh"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o644), info.Mode())
		assert.WithinDuration(t, time.Now(), info.ModTime(), time.Minute)
	}
	for _, test := range []struct {
		inputs map[string]string
		error  string
	}{
		{map[string]string{"artifact-ids": strconv.FormatInt(first, 10)}, "None of the provided artifact IDs were found"},
		{map[string]string{"name": "gone"}, "Artifact not found for name: gone"},
		{map[string]string{"name": "alpha", "artifact-ids": "1"}, "cannot be used together"},
		{map[string]string{"artifact-ids": "x"}, "Invalid artifact ID: 'x'"},
		{map[string]string{"artifact-ids": " , "}, "No valid artifact IDs provided"},
		{map[string]string{"digest-mismatch": "loud"}, "Valid options are: ignore, info, warn, error"},
		{map[string]string{"merge-multiple": "yes"}, "Core Schema"},
	} {
		require.ErrorContains(t, h.run(test.inputs), test.error)
	}
	h.env = map[string]string{"GITHUB_WORKSPACE": filepath.Join(h.workspace, "overridden")}
	require.NoError(t, h.run(map[string]string{"name": "beta ü.txt"}))
	assert.Equal(t, "plain text", h.read("overridden/beta ü.txt"))
	h.env = map[string]string{"HOME": filepath.Join(h.workspace, "home")}
	require.NoError(t, h.run(map[string]string{"name": "beta ü.txt", "path": "~/tilde", "retention-days": "1"}))
	assert.Equal(t, "plain text", h.read("home/tilde/beta ü.txt"))
	assert.Equal(t, []string{"warning: Unexpected input(s) 'retention-days', valid inputs are ['name', 'artifact-ids', 'path', 'pattern', 'merge-multiple', 'github-token', 'repository', 'run-id', 'skip-decompress', 'digest-mismatch']"}, h.annotation)
	h.upload("..", "", archive)
	require.ErrorContains(t, h.run(map[string]string{}), "Artifact name is not valid: ..")
}

func TestDownloadFormats(t *testing.T) {
	for _, test := range []struct {
		contentType, url string
		zip              bool
	}{
		{"application/zip", "raw", true},
		{"Application/X-Zip-Compressed; charset=binary", "raw", true},
		{"application/zip-compressed", "raw", true},
		{"", "FILE.ZIP?sig=x", true},
		{"", "raw?name=file.zip", false},
	} {
		assert.Equal(t, test.zip, isZip(&http.Response{Header: http.Header{"Content-Type": {test.contentType}}}, "https://example.test/"+test.url))
	}
	for _, test := range []struct {
		pattern, name string
		want          bool
	}{
		{"a*", "alpha", true},
		{"*", ".hidden", false},
		{"!alpha", "beta", true},
		{"#alpha", "alpha", false},
	} {
		assert.Equal(t, test.want, matchName(test.pattern, test.name))
	}
}

func TestDownloadDigestMismatch(t *testing.T) {
	h := newHarness(t)
	h.upload("art", "", zipOf(t, zipEntry{name: "f.txt", body: "original"}))
	blobs, err := filepath.Glob(filepath.Join(h.store, "v4", "*", "*", "blob"))
	require.NoError(t, err)
	require.Len(t, blobs, 1)
	tampered := zipOf(t, zipEntry{name: "f.txt", body: "tampered"})
	require.NoError(t, os.WriteFile(blobs[0], tampered, 0o644))

	for _, behavior := range []string{"", "ignore", "info", "warn", "error"} {
		err = h.run(map[string]string{"name": "art", "path": "digest", "digest-mismatch": behavior})
		if behavior == "" || behavior == "error" {
			require.ErrorContains(t, err, "Digest validation failed for artifact(s): art. Use 'digest-mismatch: warn' to continue on mismatch.")
		} else {
			require.NoError(t, err)
		}
		assert.Equal(t, "tampered", h.read("digest/f.txt"))
		if behavior == "warn" {
			assert.Equal(t, []string{"warning: Artifact 'art' digest validation failed. Please verify the integrity of the artifact."}, h.annotation)
		} else {
			assert.Empty(t, h.annotation)
		}
		if behavior == "info" {
			assert.Contains(t, h.logs, "Artifact 'art' digest validation failed. Please verify the integrity of the artifact.")
		}
	}
}

func TestDownloadFromAnotherRun(t *testing.T) {
	h := &harness{t: t, workspace: t.TempDir()}
	archive := zipOf(t, zipEntry{name: "other.txt", body: "from run 12"})
	var expired atomic.Bool
	var zipCalls atomic.Int32
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(archive)
	}))
	defer blob.Close()
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/someone/else/actions/runs/12/artifacts":
			assert.Equal(t, "token pat", r.Header.Get("Authorization"))
			_, _ = io.WriteString(w, `{"total_count":3,"artifacts":[
				{"id":10,"name":"dist","size_in_bytes":1,"expired":false},
				{"id":30,"name":"dist","size_in_bytes":1,"expired":true},
				{"id":20,"name":"dist","size_in_bytes":1,"expired":false}]}`)
		case "/api/v1/repos/someone/else/actions/artifacts/20/zip":
			zipCalls.Add(1)
			if expired.Load() {
				http.Error(w, "Artifact has expired", http.StatusNotFound)
				return
			}
			http.Redirect(w, r, blob.URL+"/raw/20?sig=abc", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer gitea.Close()
	h.server = gitea.URL

	inputs := map[string]string{"github-token": "pat", "repository": "someone/else/ignored", "run-id": "12", "path": "other"}
	require.NoError(t, h.run(inputs))
	assert.Equal(t, "from run 12", h.read("other/other.txt"))
	assert.Contains(t, h.logs, "Found 1 artifact(s)")
	expired.Store(true)
	require.ErrorContains(t, h.run(inputs), "Artifact has expired")
	assert.Equal(t, int32(2), zipCalls.Load(), "an API error is not retried")
	require.ErrorContains(t, h.run(map[string]string{"github-token": "pat", "repository": "nobody"}), "Invalid repository: 'nobody'. Must be in format owner/repo")
	require.ErrorContains(t, h.run(map[string]string{"github-token": "pat", "run-id": "0"}), "Invalid run-id: '0'")
}

func TestDownloadRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		target := filepath.Join(t.TempDir(), "download")
		started := time.Now()
		_, err := fetch(t.Context(), &action.Context{Container: &container.HostEnvironment{}}, &source{
			signedURL: func(context.Context, artifact.Artifact) (string, error) {
				assert.DirExists(t, target)
				return "https://example.test/artifact.zip", nil
			},
			download: func(context.Context, string, time.Duration) (*http.Response, error) {
				attempts++
				return nil, errors.New("connection failed")
			},
		}, artifact.Artifact{}, target, false)
		require.EqualError(t, err, "Unable to download and extract artifact: Artifact download failed after 5 retries.")
		assert.Equal(t, 5, attempts)
		assert.Equal(t, 25*time.Second, time.Since(started))
	})
}

func TestDownloadRejectsEscapingZipPaths(t *testing.T) {
	for _, name := range []string{"../escape.txt", "a/../../escape.txt", "/absolute.txt", `..\escape.txt`} {
		t.Run(name, func(t *testing.T) {
			archive := zipOf(t, zipEntry{name: name, body: "escape"})
			require.ErrorContains(t, extractZip(t.Context(), &action.Context{Container: &container.HostEnvironment{}}, bytes.NewReader(archive), int64(len(archive)), t.TempDir()), "Artifact entry path is not valid")
		})
	}
}
