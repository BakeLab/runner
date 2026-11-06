// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package downloadartifact works like actions/download-artifact v8.
package downloadartifact

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/internal/pkg/action"
	"gitea.com/gitea/runner/internal/pkg/action/artifact"

	"golang.org/x/sync/errgroup"
)

var digestBehaviors = []string{"ignore", "info", "warn", "error"}

const downloadAttempts = 5

func Main(ctx context.Context, c *action.Context) error {
	if err := run(ctx, c); err != nil {
		return action.Failf("Unable to download artifact(s): %w", err)
	}
	return nil
}

func run(ctx context.Context, c *action.Context) error {
	c.ApplyDefaults("name", "", "artifact-ids", "", "path", "", "pattern", "", "merge-multiple", "false", "github-token", "",
		"repository", c.Github.Repository, "run-id", c.Github.RunID, "skip-decompress", "false", "digest-mismatch", "error")
	name, ids, pattern, token := c.Input("name"), c.Input("artifact-ids"), c.Input("pattern"), c.Input("github-token")
	mergeMultiple, err := c.BoolInput("merge-multiple")
	if err != nil {
		return err
	}
	skipDecompress, err := c.BoolInput("skip-decompress")
	if err != nil {
		return err
	}
	digestMismatch := cmp.Or(c.Input("digest-mismatch"), "error")
	if !slices.Contains(digestBehaviors, digestMismatch) {
		return action.Failf("Invalid value for 'digest-mismatch': '%s'. Valid options are: %s", digestMismatch, strings.Join(digestBehaviors, ", "))
	}
	dest := cmp.Or(c.Input("path"), c.Env["GITHUB_WORKSPACE"], c.Workspace)
	if strings.HasPrefix(dest, "~") {
		home, err := c.Home(ctx)
		if err != nil {
			return err
		}
		dest = home + dest[1:]
	}
	dest = c.Resolve(dest)
	if name != "" && ids != "" {
		return action.Failf("Inputs 'name' and 'artifact-ids' cannot be used together. Please specify only one.")
	}

	src, err := newSource(c, token)
	if err != nil {
		return err
	}
	artifacts, err := src.list(ctx, name)
	if err != nil {
		return err
	}
	slices.SortStableFunc(artifacts, func(a, b artifact.Artifact) int { return cmp.Compare(b.ID, a.ID) })
	seen := map[string]bool{}
	artifacts = slices.DeleteFunc(artifacts, func(a artifact.Artifact) bool {
		duplicate := seen[a.Name]
		seen[a.Name] = true
		return duplicate
	})
	if name == "" {
		c.Infof("Found %d artifact(s)", len(artifacts))
	}
	switch {
	case name != "":
		if len(artifacts) == 0 {
			return action.Failf("Artifact not found for name: %s\nPlease ensure that your artifact is not expired and the artifact was uploaded using a compatible version of toolkit/upload-artifact.", name)
		}
		artifacts = artifacts[:1]
	case ids != "":
		var wanted []int64
		for id := range strings.SplitSeq(ids, ",") {
			if id = strings.TrimSpace(id); id == "" {
				continue
			}
			parsed, err := action.ParseInt(id)
			if err != nil {
				return action.Failf("Invalid artifact ID: '%s'. Must be a number.", id)
			}
			wanted = append(wanted, int64(parsed))
		}
		if len(wanted) == 0 {
			return action.Failf("No valid artifact IDs provided in 'artifact-ids' input")
		}
		artifacts = slices.DeleteFunc(artifacts, func(a artifact.Artifact) bool { return !slices.Contains(wanted, a.ID) })
		if len(artifacts) == 0 {
			return action.Failf("None of the provided artifact IDs were found")
		}
	case pattern != "":
		artifacts = slices.DeleteFunc(artifacts, func(a artifact.Artifact) bool { return !matchName(pattern, a.Name) })
	}
	for _, art := range artifacts {
		if art.Name == "" || art.Name == "." || art.Name == ".." || strings.ContainsAny(art.Name, `/\`) || c.Windows() && !filepath.IsLocal(art.Name) {
			return action.Failf("Artifact name is not valid: %s", art.Name)
		}
	}

	mismatches := make([]string, len(artifacts))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(5)
	for i, art := range artifacts {
		target := dest
		if name == "" && !mergeMultiple && len(artifacts) > 1 {
			target = c.Join(dest, art.Name)
		}
		group.Go(func() error {
			mismatch, err := fetch(groupCtx, c, src, art, target, skipDecompress)
			if err != nil {
				return err
			}
			if mismatch {
				mismatches[i] = art.Name
				message := fmt.Sprintf("Artifact '%s' digest validation failed. Please verify the integrity of the artifact.", art.Name)
				switch digestMismatch {
				case "info":
					c.Infof("%s", message)
				case "warn":
					c.Warning("%s", message)
				}
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	mismatches = slices.DeleteFunc(mismatches, func(name string) bool { return name == "" })
	if len(mismatches) > 0 && digestMismatch == "error" {
		return action.Failf("Digest validation failed for artifact(s): %s. Use 'digest-mismatch: warn' to continue on mismatch.", strings.Join(mismatches, ", "))
	}
	c.Infof("Total of %d artifact(s) downloaded", len(artifacts))
	return c.SetOutputs(ctx, "download-path", dest)
}

func fetch(ctx context.Context, c *action.Context, src *source, art artifact.Artifact, target string, skipDecompress bool) (bool, error) {
	if err := c.Container.CopyTarStream(ctx, target, func(*container.TarWriter) error { return nil }); err != nil {
		return false, err
	}
	signed, err := src.signedURL(ctx, art)
	if err != nil {
		return false, err
	}
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		digest, err := fetchOnce(ctx, c, src, signed, target, skipDecompress)
		if err == nil {
			return art.Digest != "" && art.Digest != digest, nil
		}
		common.Logger(ctx).Debugf("Failed to download artifact after %d retries due to %v. Retrying in 5 seconds...", attempt, err)
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return false, action.Failf("Unable to download and extract artifact: Artifact download failed after %d retries.", downloadAttempts)
}
