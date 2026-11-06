// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package action holds the native actions run by `uses: builtin:<name>`.
package action

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/model"
)

type Context struct {
	Container       container.ExecutionsEnvironment
	Github          *model.GithubContext
	Inputs          map[string]string
	HostWorkdir     string
	Workspace       string // HostWorkdir as seen inside Container
	BindWorkdir     bool
	NoSkipCheckout  bool
	UseGitIgnore    bool
	InsecureSkipTLS bool
	ClientCertFile  string
	ClientKeyFile   string
	CAFile          string // the runner CA as seen inside Container
	Env             map[string]string
	ResultsURL      string // the results service as the runner reaches it
	RuntimeToken    string
	Transport       http.RoundTripper // carries the instance's TLS settings, nil for the default
	Infof           func(format string, args ...any)
	Warning         func(format string, args ...any)
}

func (c *Context) ApplyDefaults(pairs ...string) {
	var valid, unexpected []string
	for i := 0; i+1 < len(pairs); i += 2 {
		valid = append(valid, pairs[i])
		if _, set := c.Inputs[pairs[i]]; !set && pairs[i+1] != "" {
			c.Inputs[pairs[i]] = pairs[i+1]
		}
	}
	for name := range c.Inputs {
		if !slices.Contains(valid, name) {
			unexpected = append(unexpected, name)
		}
	}
	if len(unexpected) > 0 {
		slices.Sort(unexpected)
		c.Warning("Unexpected input(s) '%s', valid inputs are ['%s']", strings.Join(unexpected, "', '"), strings.Join(valid, "', '"))
	}
}

// Input is the trimmed input, as @actions/core getInput returns it.
func (c *Context) Input(name string) string {
	return strings.TrimSpace(c.Inputs[name])
}

// BoolInput accepts what @actions/core getBooleanInput does.
func (c *Context) BoolInput(name string) (bool, error) {
	switch c.Input(name) {
	case "true", "True", "TRUE":
		return true, nil
	case "false", "False", "FALSE":
		return false, nil
	}
	return false, Failf("Input does not meet YAML 1.2 \"Core Schema\" specification: %s\nSupport boolean input list: `true | True | TRUE | false | False | FALSE`", name)
}

func (c *Context) SetOutputs(ctx context.Context, pairs ...string) error {
	output := c.Env["GITHUB_OUTPUT"]
	if output == "" {
		return nil
	}
	var body strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		if strings.ContainsAny(pairs[i+1], "\r\n") {
			return fmt.Errorf("output %s spans several lines", pairs[i])
		}
		body.WriteString(pairs[i] + "=" + pairs[i+1] + "\n")
	}
	return c.Container.Copy(c.Dir(output), &container.FileEntry{Name: c.Base(output), Mode: 0o666, Body: body.String()})(ctx)
}

func (c *Context) Host() bool {
	_, host := c.Container.(*container.HostEnvironment)
	return host
}

func (c *Context) Windows() bool {
	return c.Host() && runtime.GOOS == "windows"
}

func (c *Context) Dir(p string) string {
	if c.Windows() {
		return filepath.Dir(p)
	}
	return path.Dir(p)
}

func (c *Context) Base(p string) string {
	if c.Windows() {
		return filepath.Base(p)
	}
	return path.Base(p)
}

// Home is the job user's home directory, what os.homedir() returns to a JavaScript action.
func (c *Context) Home(ctx context.Context) (string, error) {
	if home := c.Env["HOME"]; home != "" {
		return home, nil
	}
	if home := c.Env["USERPROFILE"]; c.Windows() && home != "" {
		return home, nil
	}
	if c.Host() {
		return os.UserHomeDir()
	}
	var out strings.Builder
	stdout, stderr := c.Container.ReplaceLogWriter(&out, &out)
	err := c.Container.Exec([]string{"sh", "-c", `printf %s "$HOME"`}, nil, "", "")(ctx)
	c.Container.ReplaceLogWriter(stdout, stderr)
	if err != nil {
		return "", fmt.Errorf("cannot expand ~ without HOME: %w", err)
	}
	if out.Len() == 0 {
		return "", errors.New("cannot expand ~, the job user has no HOME")
	}
	return out.String(), nil
}

// Failf is fmt.Errorf for messages worded as the JavaScript action words them, which the error string lint rejects.
func Failf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

// ParseInt reads a leading integer the way JavaScript's parseInt does.
func ParseInt(s string) (int, error) {
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	end := 0
	if end < len(s) && (s[end] == '-' || s[end] == '+') {
		end++
	}
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	return strconv.Atoi(s[:end])
}
