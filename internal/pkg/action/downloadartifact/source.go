// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package downloadartifact

import (
	"context"
	"net/http"
	"strings"
	"time"

	"gitea.com/gitea/runner/internal/pkg/action"
	"gitea.com/gitea/runner/internal/pkg/action/artifact"
)

type source struct {
	list      func(context.Context, string) ([]artifact.Artifact, error)
	signedURL func(context.Context, artifact.Artifact) (string, error)
	download  func(context.Context, string, time.Duration) (*http.Response, error)
}

func newSource(c *action.Context, token string) (*source, error) {
	if token == "" {
		client, err := artifact.New(c.ResultsURL, c.RuntimeToken, c.Transport)
		if err != nil {
			return nil, err
		}
		client.Logf = c.Infof
		return &source{
			list: client.List,
			signedURL: func(ctx context.Context, art artifact.Artifact) (string, error) {
				return client.SignedURL(ctx, art.Name)
			},
			download: client.Download,
		}, nil
	}
	parts := append(strings.Split(c.Input("repository"), "/"), "")
	owner, repo := parts[0], parts[1]
	if owner == "" || repo == "" {
		return nil, action.Failf("Invalid repository: '%s'. Must be in format owner/repo", c.Input("repository"))
	}
	runID, err := action.ParseInt(c.Input("run-id"))
	if err != nil || runID <= 0 { // Gitea lists the artifacts of every run for those
		return nil, action.Failf("Invalid run-id: '%s'. Must be a number.", c.Input("run-id"))
	}
	rest, err := artifact.NewREST(c.ResultsURL, token, c.Transport)
	if err != nil {
		return nil, err
	}
	rest.Logf = c.Infof
	return &source{
		list: func(ctx context.Context, name string) ([]artifact.Artifact, error) {
			return rest.List(ctx, owner, repo, int64(runID), name)
		},
		signedURL: func(ctx context.Context, art artifact.Artifact) (string, error) {
			return rest.DownloadURL(ctx, owner, repo, art.ID)
		},
		download: rest.Download,
	}, nil
}
