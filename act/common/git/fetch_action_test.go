// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchAction(t *testing.T) {
	original := archiveRetryDelay
	archiveRetryDelay = func(_retryAfter string) time.Duration { return time.Nanosecond }
	t.Cleanup(func() { archiveRetryDelay = original })

	root := t.TempDir()
	remote := filepath.Join(root, "org", "action")
	require.NoError(t, os.CopyFS(remote, os.DirFS(filepath.Join(gitFixtureDir, "work"))))
	require.NoError(t, os.WriteFile(filepath.Join(remote, "action.yml"), []byte("name: action\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(remote, "run.sh"), []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.Symlink("action.yml", filepath.Join(remote, "link")))
	require.NoError(t, gitCmd("-C", remote, "add", "."))
	require.NoError(t, gitCmd("-C", remote, "update-index", "--chmod=+x", "run.sh"))
	require.NoError(t, gitCmd("-C", remote, "commit", "-m", "initial"))
	require.NoError(t, gitCmd("-C", remote, "tag", "v1"))
	expectedSha := gitRevParse(t, remote, "main")

	gitPath, err := exec.LookPath("git")
	require.NoError(t, err)
	backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	var mu sync.Mutex
	var archiveRequests []http.Header
	var mirror *httptest.Server
	archiveCache := map[string][]byte{}
	revisions := map[string]string{}
	requests, archives, failures := 0, "", 0
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		ref, ok := strings.CutPrefix(request.URL.Path, "/org/action/archive/")
		if !ok {
			backend.ServeHTTP(writer, request)
			return
		}
		archiveRequests = append(archiveRequests, request.Header.Clone())
		switch {
		case failures > 0:
			failures--
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		case archives == "none":
			http.NotFound(writer, request)
			return
		case archives == "unreachable":
			http.Redirect(writer, request, "http://127.0.0.1:1"+request.URL.Path, http.StatusFound)
			return
		case archives == "redirect" && request.Host != mirror.Listener.Addr().String():
			http.Redirect(writer, request, mirror.URL+request.URL.Path, http.StatusFound)
			return
		}
		sha, ok := revisions[ref]
		if !ok {
			var err error
			sha, err = gitOutput(request.Context(), remote, "rev-parse", strings.TrimSuffix(ref, ".tar.gz"))
			if err == nil {
				revisions[ref] = sha
			}
		}
		etag := fmt.Sprintf(`W/"%s-tar.gz-%t"`, sha, archives != "unprefixed")
		if request.Header.Get("If-None-Match") == etag {
			writer.WriteHeader(http.StatusNotModified)
			return
		}
		args := []string{"archive", "--format=tar.gz", sha}
		if archives != "unprefixed" {
			args = append(args, "--prefix=action/")
		}
		archiveKey := strings.Join(args, "\x00")
		archive, ok := archiveCache[archiveKey]
		if !ok {
			archive, _ = runGit(request.Context(), remote, nil, args...)
			archiveCache[archiveKey] = archive
		}
		writer.Header().Set("ETag", etag)
		_, _ = writer.Write(archive)
	})
	server := httptest.NewServer(handler)
	mirror = httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Cleanup(mirror.Close)
	serve := func(mode string, failing int) {
		mu.Lock()
		defer mu.Unlock()
		requests, archives, failures, archiveRequests = 0, mode, failing, nil
	}
	served := func() (int, []http.Header) {
		mu.Lock()
		defer mu.Unlock()
		return requests, append([]http.Header(nil), archiveRequests...)
	}
	newInput := func(ref string) NewGitCloneExecutorInput {
		return NewGitCloneExecutorInput{URL: server.URL + "/org/action", Ref: ref, Dir: t.TempDir(), Token: "secret"}
	}

	t.Run("shares identical fetches and isolates cancellation, panics and differing inputs", func(t *testing.T) {
		for _, scenario := range []string{"shared", "leader cancelled", "leader panics", "different token"} {
			t.Run(scenario, func(t *testing.T) {
				serve("prefixed", 0)
				input := newInput("main")
				started, release := make(chan struct{}), make(chan struct{})
				releaseFetches := sync.OnceFunc(func() { close(release) })
				t.Cleanup(releaseFetches)
				var once sync.Once
				var panicked atomic.Bool
				transport := actionFetchTransport(func(request *http.Request) (*http.Response, error) {
					once.Do(func() { close(started) })
					select {
					case <-request.Context().Done():
						return nil, request.Context().Err()
					case <-release:
					}
					if scenario == "leader panics" && panicked.CompareAndSwap(false, true) {
						panic("broken archive")
					}
					return http.DefaultTransport.RoundTrip(request)
				})
				input.Transport = &transport
				leaderCtx, cancelLeader := context.WithCancel(t.Context())
				defer cancelLeader()
				waiting := &actionFetchContext{Context: t.Context(), waiting: make(chan struct{})}
				followerInput := input
				expectedRequests := 1
				if scenario == "different token" {
					followerInput.Token, expectedRequests = "other", 2
				}
				leaderDone, followerDone := make(chan error, 1), make(chan error, 1)
				fetch := func(ctx context.Context, input NewGitCloneExecutorInput, done chan<- error) {
					var err error
					defer func() { done <- err }()
					defer common.RecoverPanic(&err)
					var tree, sha string
					if tree, sha, err = FetchAction(ctx, input); err == nil {
						assert.Equal(t, expectedSha, sha)
						assert.Equal(t, filepath.Join(input.Dir, sha), tree)
					}
				}
				go fetch(leaderCtx, input, leaderDone)
				<-started
				go fetch(waiting, followerInput, followerDone)
				<-waiting.waiting
				if scenario == "leader cancelled" {
					cancelLeader()
					require.ErrorIs(t, <-leaderDone, context.Canceled)
				}
				releaseFetches()
				switch scenario {
				case "leader cancelled":
				case "leader panics":
					require.ErrorContains(t, <-leaderDone, "panic: broken archive")
				default:
					require.NoError(t, <-leaderDone)
				}
				require.NoError(t, <-followerDone)
				_, headers := served()
				assert.Len(t, headers, expectedRequests)
				if scenario == "different token" {
					assert.NotEqual(t, headers[0].Get("Authorization"), headers[1].Get("Authorization"))
				}
			})
		}
	})

	t.Run("downloads, revalidates and follows a moved tag", func(t *testing.T) {
		serve("prefixed", 0)
		input := newInput("v1")
		tree, sha, err := FetchAction(t.Context(), input)
		require.NoError(t, err)
		assert.Equal(t, expectedSha, sha)
		assert.Equal(t, filepath.Join(input.Dir, sha), tree)
		assert.NoDirExists(t, filepath.Join(tree, ".git"))
		info, err := os.Stat(filepath.Join(tree, "run.sh"))
		require.NoError(t, err)
		if runtime.GOOS != "windows" {
			assert.EqualValues(t, 0o755, info.Mode().Perm())
		}
		link, err := os.Readlink(filepath.Join(tree, "link"))
		require.NoError(t, err)
		assert.Equal(t, "action.yml", link)

		again, _, err := FetchAction(t.Context(), input)
		require.NoError(t, err)
		assert.Equal(t, tree, again)

		require.NoError(t, gitCmd("-C", remote, "commit", "--allow-empty", "-m", "moved"))
		require.NoError(t, gitCmd("-C", remote, "tag", "--force", "v1"))
		expectedSha = gitRevParse(t, remote, "v1")
		mu.Lock()
		clear(revisions)
		mu.Unlock()
		moved, movedSha, err := FetchAction(t.Context(), input)
		require.NoError(t, err)
		assert.Equal(t, expectedSha, movedSha)
		assert.NotEqual(t, tree, moved)
		assert.FileExists(t, filepath.Join(tree, "action.yml"))

		input.OfflineMode = true
		_, offlineSha, err := FetchAction(t.Context(), input)
		require.NoError(t, err)
		assert.Equal(t, movedSha, offlineSha)
		require.NoError(t, os.Truncate(filepath.Join(moved, "action.yml"), 4)) // fetched again, offline or not
		_, _, err = FetchAction(t.Context(), input)
		require.NoError(t, err)
		require.NoError(t, os.Remove(moved+treeSizes)) // from an older runner, used as it is offline
		reused, _, err := FetchAction(t.Context(), input)
		require.NoError(t, err)
		assert.Equal(t, moved, reused)

		requests, headers := served()
		assert.Equal(t, 4, requests)
		require.Len(t, headers, 4)
		assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("token:secret")), headers[0].Get("Authorization"))
		assert.Empty(t, headers[0].Get("If-None-Match"))
		assert.Equal(t, fmt.Sprintf(`W/"%s-tar.gz-true"`, sha), headers[1].Get("If-None-Match"))
		assert.Equal(t, headers[1].Get("If-None-Match"), headers[2].Get("If-None-Match"))
	})

	t.Run("downloads a pinned commit once and rejects a short one", func(t *testing.T) {
		serve("prefixed", 0)
		sha := expectedSha
		input := newInput(sha)
		for range 2 {
			tree, got, err := FetchAction(t.Context(), input)
			require.NoError(t, err)
			assert.Equal(t, sha, got)
			assert.FileExists(t, filepath.Join(tree, "action.yml"))
		}
		_, _, err := FetchAction(t.Context(), newInput(sha[:7]))
		require.ErrorIs(t, err, ErrShortRef)
		_, headers := served()
		assert.Len(t, headers, 1)
		assert.NoDirExists(t, filepath.Join(input.Dir, "git"))
	})

	t.Run("downloads again over a tree whose files changed size or that has no record, not under a job copying it", func(t *testing.T) {
		sha := expectedSha
		for _, mode := range []string{"prefixed", "none"} {
			serve(mode, 0)
			input := newInput(sha)
			tree, _, err := FetchAction(t.Context(), input)
			require.NoError(t, err, mode)
			yml := filepath.Join(tree, "action.yml")
			require.NoError(t, os.WriteFile(filepath.Join(tree, "trampoline.js"), nil, 0o644)) // the runner's own, no damage
			_, _, err = FetchAction(t.Context(), input)
			require.NoError(t, err, mode)
			for _, damage := range []func() error{
				func() error { return os.Truncate(yml, 4) },
				func() error { return os.Remove(tree + treeSizes) },
			} {
				require.NoError(t, damage(), mode)
				_, _, err := FetchAction(t.Context(), input)
				require.NoError(t, err, mode)
				body, err := os.ReadFile(yml)
				require.NoError(t, err, mode)
				assert.Equal(t, "name: action\n", string(body), mode)
			}
			_, headers := served()
			assert.Len(t, headers, 3, mode)
		}

		serve("prefixed", 0)
		input := newInput(sha)
		tree, _, err := FetchAction(t.Context(), input)
		require.NoError(t, err)
		yml := filepath.Join(tree, "action.yml")
		require.NoError(t, os.Truncate(yml, 4))
		unlock, err := AcquireCloneLock(t.Context(), tree)
		require.NoError(t, err)
		release := sync.OnceFunc(unlock)
		defer release()
		waiting := make(chan struct{})
		lockTree = func(ctx context.Context, dir string) (func(), error) {
			close(waiting)
			return AcquireCloneLock(ctx, dir)
		}
		t.Cleanup(func() { lockTree = AcquireCloneLock })
		done := make(chan error, 1)
		go func() {
			_, _, err := FetchAction(t.Context(), input)
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("FetchAction replaced the tree without waiting for the job holding it: %v", err)
		case <-waiting:
		}
		info, err := os.Stat(yml)
		require.NoError(t, err)
		assert.Equal(t, int64(4), info.Size(), "the tree a job holds is left alone")
		release()
		require.NoError(t, <-done)
		body, err := os.ReadFile(yml)
		require.NoError(t, err)
		assert.Equal(t, "name: action\n", string(body))
	})

	t.Run("retries a failing download and drops the token on a redirect to another origin", func(t *testing.T) {
		serve("redirect", 2)
		input := newInput("main")
		_, sha, err := FetchAction(t.Context(), input)
		require.NoError(t, err)
		assert.Equal(t, expectedSha, sha)
		assert.NoDirExists(t, filepath.Join(input.Dir, "git"))
		_, headers := served()
		require.Len(t, headers, 4)
		assert.NotEmpty(t, headers[2].Get("Authorization"))
		assert.Empty(t, headers[3].Get("Authorization"))
	})

	t.Run("keeps an unprefixed archive whole, downloads where insteadOf points and exports a clone without an archive or from an unreachable host", func(t *testing.T) {
		for _, mode := range []string{"unprefixed", "none", "unreachable"} {
			serve(mode, 0)
			tree, sha, err := FetchAction(t.Context(), newInput("main"))
			require.NoError(t, err, mode)
			assert.Equal(t, expectedSha, sha, mode)
			assert.FileExists(t, filepath.Join(tree, "action.yml"), mode)
			assert.NoDirExists(t, filepath.Join(tree, ".git"), mode)
			_, headers := served()
			assert.Len(t, headers, 1, mode)
		}

		serve("prefixed", 0)
		config := filepath.Join(t.TempDir(), "gitconfig")
		require.NoError(t, os.WriteFile(config, []byte("[url \""+server.URL+"/org/\"]\n\tinsteadOf = https://example.invalid/org/\n"), 0o644))
		t.Setenv("GIT_CONFIG_GLOBAL", config)
		input := newInput("main")
		input.URL = "https://example.invalid/org/action"
		_, sha, err := FetchAction(t.Context(), input)
		require.NoError(t, err)
		assert.Equal(t, expectedSha, sha)
		_, headers := served()
		assert.Len(t, headers, 1)
		assert.NoDirExists(t, filepath.Join(input.Dir, "git"))
	})
}

type actionFetchTransport func(*http.Request) (*http.Response, error)

func (transport actionFetchTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

type actionFetchContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *actionFetchContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

func TestExtractTreeConfinesEntries(t *testing.T) {
	outside := t.TempDir()
	for name, entries := range map[string][]*tar.Header{
		"parent path":       {{Name: "../escape", Typeflag: tar.TypeReg}},
		"through a symlink": {{Name: "out", Typeflag: tar.TypeSymlink, Linkname: outside}, {Name: "out/escape", Typeflag: tar.TypeReg}},
	} {
		t.Run(name, func(t *testing.T) {
			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			require.NoError(t, writer.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": strings.Repeat("a", 40)}}))
			for _, entry := range entries {
				require.NoError(t, writer.WriteHeader(entry))
			}
			require.NoError(t, writer.Close())

			dir := t.TempDir()
			_, err := extractTree(t.Context(), &archive, dir, false)
			require.Error(t, err)
			assert.NoFileExists(t, filepath.Join(dir, "escape"))
			assert.NoFileExists(t, filepath.Join(outside, "escape"))
		})
	}
}

func TestExtractTreeCopiesDeferredLinks(t *testing.T) {
	originalSymlink, originalFallback := symlink, symlinkFallback
	symlink = func(_root *os.Root, _target, _name string) error { return errors.New("symlink privilege unavailable") }
	symlinkFallback = true
	t.Cleanup(func() { symlink, symlinkFallback = originalSymlink, originalFallback })

	for _, test := range []struct {
		name  string
		links [][2]string
		files []string
		text  bool
	}{
		{name: "file chain", links: [][2]string{{"index.js", "entry.js"}, {"entry.js", "dist/index.js"}}, files: []string{"index.js", "entry.js"}},
		{name: "directory chain", links: [][2]string{{"lib", "bundle"}, {"bundle", "dist"}, {"dist/entry.js", "index.js"}}, files: []string{"lib/index.js", "lib/entry.js", "lib/nested/index.js", "bundle/index.js", "bundle/entry.js", "bundle/nested/index.js"}},
		{name: "directory in target path", links: [][2]string{{"index.js", "bundle/nested/index.js"}, {"bundle", "dist"}}, files: []string{"index.js", "bundle/nested/index.js"}},
		{name: "parent after directory link", links: [][2]string{{"index.js", "bundle/../index.js"}, {"bundle", "dist/nested"}}, files: []string{"index.js", "bundle/index.js"}},
		{name: "outside", links: [][2]string{{"index.js", "../outside.js"}}, text: true},
		{name: "absolute", links: [][2]string{{"index.js", t.TempDir()}}, text: true},
		{name: "dangling", links: [][2]string{{"index.js", "missing.js"}}, text: true},
		{name: "dangling child of directory link", links: [][2]string{{"lib", "dist"}, {"dist/optional", "missing.js"}}, files: []string{"lib/index.js", "lib/nested/index.js"}},
		{name: "cycle", links: [][2]string{{"index.js", "entry.js"}, {"entry.js", "index.js"}}, text: true},
		{name: "directory cycle", links: [][2]string{{"dist/loop", "."}}, text: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			sha := strings.Repeat("a", 40)
			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			require.NoError(t, writer.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": sha}}))
			require.NoError(t, writer.WriteHeader(&tar.Header{Name: "dist", Typeflag: tar.TypeDir, Mode: 0o755}))
			require.NoError(t, writer.WriteHeader(&tar.Header{Name: "dist/nested", Typeflag: tar.TypeDir, Mode: 0o755}))
			for _, link := range test.links {
				require.NoError(t, writer.WriteHeader(&tar.Header{Name: link[0], Typeflag: tar.TypeSymlink, Linkname: link[1]}))
			}
			content := "console.log('action');\n"
			for _, name := range []string{"dist/index.js", "dist/nested/index.js"} {
				require.NoError(t, writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(content))}))
				_, err := writer.Write([]byte(content))
				require.NoError(t, err)
			}
			require.NoError(t, writer.Close())

			dir := t.TempDir()
			_, err := extractTree(t.Context(), &archive, dir, false)
			require.NoError(t, err)
			assert.True(t, treeIntact(dir, sha))
			if test.text {
				for _, link := range test.links {
					data, err := os.ReadFile(filepath.Join(dir, sha, link[0]))
					require.NoError(t, err)
					assert.Equal(t, link[1], string(data))
					info, err := os.Lstat(filepath.Join(dir, sha, link[0]))
					require.NoError(t, err)
					assert.True(t, info.Mode().IsRegular())
				}
			}
			for _, name := range test.files {
				data, err := os.ReadFile(filepath.Join(dir, sha, name))
				require.NoError(t, err)
				assert.Equal(t, content, string(data))
				info, err := os.Lstat(filepath.Join(dir, sha, name))
				require.NoError(t, err)
				assert.True(t, info.Mode().IsRegular())
				if runtime.GOOS != "windows" {
					assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
				}
				require.NoError(t, os.WriteFile(filepath.Join(dir, sha, name), nil, 0o644))
				assert.False(t, treeIntact(dir, sha), name)
				require.NoError(t, os.WriteFile(filepath.Join(dir, sha, name), []byte(content), 0o755))
			}
		})
	}
}

func TestTreeIntactFindsNamesThatAreNotUTF8(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("macOS and Windows store no name that is not UTF-8")
	}
	sha := strings.Repeat("a", 40)
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	require.NoError(t, writer.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": sha}}))
	require.NoError(t, writer.WriteHeader(&tar.Header{Name: "caf\xe9.js", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1, Format: tar.FormatGNU}))
	_, err := writer.Write([]byte("x"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	dir := t.TempDir()
	_, err = extractTree(t.Context(), &archive, dir, false)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(dir, sha, "caf\xe9.js"))
	assert.True(t, treeIntact(dir, sha))
}

func TestExtractTreeCreatesLinksInPlatformOrder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symlink privileges")
	}
	original, originalFallback := symlink, symlinkFallback
	var created []string
	symlink = func(root *os.Root, target, name string) error {
		created = append(created, name)
		return root.Symlink(target, name)
	}
	t.Cleanup(func() { symlink, symlinkFallback = original, originalFallback })

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	require.NoError(t, writer.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": strings.Repeat("a", 40)}}))
	for _, entry := range []*tar.Header{
		{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "b"},
		{Name: "b", Typeflag: tar.TypeSymlink, Linkname: "dist"},
		{Name: "dangling", Typeflag: tar.TypeSymlink, Linkname: "missing"},
		{Name: "dist", Typeflag: tar.TypeDir, Mode: 0o755},
	} {
		require.NoError(t, writer.WriteHeader(entry))
	}
	require.NoError(t, writer.Close())
	for _, test := range []struct {
		name     string
		fallback bool
		want     []string
	}{
		{name: "native", want: []string{"a", "b", "dangling"}},
		{name: "fallback", fallback: true, want: []string{"b", "a", "dangling"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			symlinkFallback = test.fallback
			created = nil
			_, err := extractTree(t.Context(), bytes.NewReader(archive.Bytes()), t.TempDir(), false)
			require.NoError(t, err)
			assert.Equal(t, test.want, created)
		})
	}
}

func TestCopyTreeLinkCancellationBetweenEntries(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	defer root.Close()
	require.NoError(t, root.Mkdir("source", 0o755))
	for _, name := range []string{"first", "second"} {
		_, err := writeTreeFile(root, filepath.Join("source", name), 0o644, strings.NewReader("content"))
		require.NoError(t, err)
	}
	sizes := map[string]int64{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := &treeCopyContext{Context: ctx, check: func() {
		if len(sizes) == 1 {
			cancel()
		}
	}}
	require.ErrorIs(t, copyTreeLink(observed, root, "copy", map[string]string{"copy": "source"}, nil, sizes), context.Canceled)
	assert.Len(t, sizes, 1)
}

type treeCopyContext struct {
	context.Context
	check func()
}

func (ctx *treeCopyContext) Err() error {
	ctx.check()
	return ctx.Context.Err()
}
