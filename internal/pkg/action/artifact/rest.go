// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifact

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type REST struct {
	base  *url.URL
	token string
	api   *http.Client
	blob  *http.Client
	Retry Retry
	Logf  func(format string, args ...any)
}

// NewREST takes the instance as the runner reaches it, never the job's GITHUB_API_URL, so the token only goes there.
func NewREST(instance, token string, transport http.RoundTripper) (*REST, error) {
	parsed, err := url.Parse(instance)
	if err != nil {
		return nil, err
	}
	return &REST{
		base:  parsed.JoinPath("api", "v1"),
		token: token,
		api: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // the redirect target is signed and must not get the token
		}},
		blob:  &http.Client{Transport: blobTransport{instance: parsed.Host, transport: transport}},
		Retry: DefaultRetry,
		Logf:  func(string, ...any) {},
	}, nil
}

func (r *REST) get(ctx context.Context, what string, endpoint *url.URL) (*http.Response, error) {
	return r.Retry.do(ctx, r.Logf, what, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "token "+r.token)
		req.Header.Set("Accept", "application/json")
		return r.api.Do(req)
	})
}

func (r *REST) List(ctx context.Context, owner, repo string, runID int64, name string) ([]Artifact, error) {
	if name == "" {
		r.Logf("Fetching artifact list for workflow run %d in repository %s/%s", runID, owner, repo)
	}
	var list []Artifact
	seen := 0
	for page := 1; ; page++ {
		endpoint := r.base.JoinPath("repos", owner, repo, "actions", "runs", strconv.FormatInt(runID, 10), "artifacts")
		query := url.Values{"page": {strconv.Itoa(page)}, "limit": {"50"}}
		if name != "" {
			query.Set("name", name)
		}
		endpoint.RawQuery = query.Encode()
		resp, err := r.get(ctx, "list artifacts", endpoint)
		if err != nil {
			return nil, err
		}
		var body struct {
			TotalCount int `json:"total_count"`
			Artifacts  []struct {
				ID          int64  `json:"id"`
				Name        string `json:"name"`
				SizeInBytes int64  `json:"size_in_bytes"`
				Expired     bool   `json:"expired"`
				Digest      string `json:"digest"`
			} `json:"artifacts"`
		}
		err = json.UnmarshalRead(resp.Body, &body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("list artifacts: %w", err)
		}
		for _, listed := range body.Artifacts {
			if !listed.Expired {
				list = append(list, Artifact{ID: listed.ID, Name: listed.Name, Size: listed.SizeInBytes, Digest: listed.Digest})
			}
		}
		if seen += len(body.Artifacts); len(body.Artifacts) == 0 || seen >= body.TotalCount {
			return list, nil
		}
	}
}

func (r *REST) DownloadURL(ctx context.Context, owner, repo string, id int64) (string, error) {
	resp, err := r.get(ctx, "download artifact", r.base.JoinPath("repos", owner, repo, "actions", "artifacts", strconv.FormatInt(id, 10), "zip"))
	if err != nil {
		return "", fmt.Errorf("unable to download artifact: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return "", fmt.Errorf("unable to download artifact. Unexpected status: %d", resp.StatusCode)
	}
	location, err := resp.Location()
	if err != nil {
		return "", errors.New("unable to redirect to artifact download url")
	}
	return location.String(), nil
}

func (r *REST) Download(ctx context.Context, signedURL string, stall time.Duration) (*http.Response, error) {
	return (&Client{blob: r.blob}).Download(ctx, signedURL, stall)
}
