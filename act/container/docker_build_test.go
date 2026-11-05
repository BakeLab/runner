// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd || freebsd))

package container

import (
	"archive/tar"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDockerBuildLabelsImageWithItsTagAndReportsDaemonError(t *testing.T) {
	var labels string
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("API-Version", "1.47")
		if strings.HasSuffix(request.URL.Path, "/build") {
			labels = request.URL.Query().Get("labels")
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(writer, `{"message":"build rejected"}`)
		}
	}))
	t.Cleanup(daemon.Close)
	t.Setenv("DOCKER_HOST", daemon.URL)
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("DOCKER_AUTH_CONFIG", "")

	require.ErrorContains(t, NewDockerBuildExecutor(NewDockerBuildExecutorInput{
		ImageTag: "action:test", Dockerfile: "Dockerfile", BuildContext: strings.NewReader(""),
	})(t.Context()), "build rejected")
	require.JSONEq(t, `{"com.gitea.runner.image":"action:test"}`, labels)
}

func TestCreateBuildContextFollowsDockerignoreAndSymlinkedRootButKeepsDockerfile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "build"), 0o755))
	for name, content := range map[string]string{
		".dockerignore": "*.log\nbuild\n!build/keep.txt\nDockerfile", "Dockerfile": "FROM scratch", "a.txt": "a",
		"a.log": "log", "build/drop.txt": "drop", "build/keep.txt": "keep",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}
	require.NoError(t, os.Mkdir(filepath.Join(dir, "empty"), 0o755))

	contexts := []string{dir}
	if link := filepath.Join(t.TempDir(), "link"); os.Symlink(dir, link) == nil {
		contexts = append(contexts, link, link+string(filepath.Separator))
	}

	for _, contextDir := range contexts {
		buildContext, err := createBuildContext(t.Context(), contextDir, "Dockerfile")
		require.NoError(t, err)
		var names []string
		for reader := tar.NewReader(buildContext); ; {
			header, err := reader.Next()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			names = append(names, header.Name)
		}
		require.NoError(t, buildContext.Close())
		require.Equal(t, []string{".dockerignore", "a.txt", "build/keep.txt", "empty/", "Dockerfile"}, names, contextDir)
	}
}
