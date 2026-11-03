// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"context"
	"path"
	"strings"
)

func JoinPath(elem ...string) string { // path.Join would collapse the leading // of a UNC act path on a Windows host
	if len(elem) > 0 && strings.HasPrefix(elem[0], "//") {
		return "/" + path.Join(elem...)
	}
	return path.Join(elem...)
}

type ExecutionsEnvironment interface {
	Container
	ToContainerPath(string) string
	GetActPath() string
	GetPathVariableName() string
	DefaultPathVariable() string
	JoinPathVariable(...string) string
	GetRunnerContext(ctx context.Context) map[string]any
	// On windows PATH and Path are the same key
	IsEnvironmentCaseInsensitive() bool
}
