// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package artifact talks to the v4 results service of the job's run.
package artifact

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Artifact struct {
	ID     int64  `json:"databaseId,string"`
	Name   string `json:"name"`
	Size   int64  `json:"size,string"`
	Digest string `json:"digest"`
}

type Retry struct {
	Attempts   int
	Base       time.Duration
	Multiplier float64
	MaxWait    time.Duration
}

var DefaultRetry = Retry{Attempts: 5, Base: 8 * time.Second, Multiplier: 1.5, MaxWait: 2 * time.Minute}

type Client struct {
	base      *url.URL
	token     string
	ids       backendIDsMessage
	api, blob *http.Client
	Retry     Retry
	Logf      func(format string, args ...any)
}

// New reaches base through transport, which carries the instance's TLS settings, and signed URLs elsewhere directly.
func New(base, token string, transport http.RoundTripper) (*Client, error) {
	if base == "" {
		return nil, errors.New("no artifact service is configured for this runner")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("ACTIONS_RUNTIME_TOKEN is not set")
	}
	ids, err := backendIDs(token)
	if err != nil {
		return nil, err
	}
	return &Client{
		base: parsed, token: token, ids: ids,
		api:   &http.Client{Transport: transport, Timeout: 3 * time.Minute},
		blob:  &http.Client{Transport: blobTransport{instance: parsed.Host, transport: transport}},
		Retry: DefaultRetry,
		Logf:  func(string, ...any) {},
	}, nil
}

// backendIDs reads the unverified scp claim of a runtime token, as the toolkit does.
func backendIDs(token string) (backendIDsMessage, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return backendIDsMessage{}, errors.New("the runtime token is not a JWT, artifacts v4 need Gitea 1.22 or later")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return backendIDsMessage{}, err
	}
	var claims struct {
		Scp string `json:"scp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return backendIDsMessage{}, err
	}
	for scope := range strings.FieldsSeq(claims.Scp) {
		if fields := strings.Split(scope, ":"); fields[0] == "Actions.Results" && len(fields) == 3 {
			return backendIDsMessage{fields[1], fields[2]}, nil
		}
	}
	return backendIDsMessage{}, errors.New("the runtime token has no Actions.Results scope")
}

type blobTransport struct { // the instance's TLS settings only for the instance, as a signed URL can point at object storage
	instance  string
	transport http.RoundTripper
}

func (t blobTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == t.instance && t.transport != nil {
		return t.transport.RoundTrip(req)
	}
	return http.DefaultTransport.RoundTrip(req)
}

type StatusError struct {
	Status int
	Msg    string
}

func (e *StatusError) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("(%d) %s", e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("(%d) %s: %s", e.Status, http.StatusText(e.Status), e.Msg)
}

func IsNotFound(err error) bool {
	var status *StatusError
	return errors.As(err, &status) && status.Status == http.StatusNotFound
}

func (r Retry) do(ctx context.Context, logf func(string, ...any), what string, send func() (*http.Response, error)) (*http.Response, error) {
	var waited time.Duration
	for attempt := 0; ; attempt++ {
		resp, err := send()
		var wait time.Duration
		if err == nil {
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return resp, nil
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			err = &StatusError{Status: resp.StatusCode, Msg: twirpMessage(body)}
			switch resp.StatusCode {
			case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			default:
				return nil, err
			}
			if seconds, parseErr := strconv.Atoi(resp.Header.Get("Retry-After")); parseErr == nil && seconds > 0 && resp.StatusCode == http.StatusTooManyRequests {
				wait = time.Duration(seconds) * time.Second
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt+1 >= r.Attempts {
			return nil, fmt.Errorf("%s failed after %d attempts: %w", what, r.Attempts, err)
		}
		if wait == 0 {
			wait = time.Duration(float64(r.Base) * math.Pow(r.Multiplier, float64(attempt)))
		}
		if waited+wait > r.MaxWait {
			return nil, fmt.Errorf("%s: a retry wait of %s would exceed %s: %w", what, wait, r.MaxWait, err)
		}
		logf("Attempt %d of %d failed with error: %v. Retrying request in %d ms...", attempt+1, r.Attempts, err, wait.Milliseconds())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		waited += wait
	}
}

// twirpMessage is the msg of a twirp error, the message of a Gitea API one, or the body Gitea answers in plain text.
func twirpMessage(body []byte) string {
	var twirpErr struct {
		Msg     string `json:"msg"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &twirpErr) == nil && cmp.Or(twirpErr.Msg, twirpErr.Message) != "" {
		return cmp.Or(twirpErr.Msg, twirpErr.Message)
	}
	return strings.TrimSpace(string(body))
}

type ( // the ArtifactService messages in the protobuf JSON mapping, requests with the proto names the toolkit sends
	backendIDsMessage struct {
		WorkflowRunBackendID    string `json:"workflow_run_backend_id"`
		WorkflowJobRunBackendID string `json:"workflow_job_run_backend_id"`
	}
	namedRequest struct {
		backendIDsMessage
		Name string `json:"name"`
	}
	createRequest struct {
		namedRequest
		ExpiresAt *time.Time `json:"expires_at,omitempty"`
		Version   int32      `json:"version"`
		MimeType  string     `json:"mime_type,omitempty"` // Gitea before 1.26 rejects the field
	}
	finalizeRequest struct {
		namedRequest
		Size int64  `json:"size,string"`
		Hash string `json:"hash"`
	}
	listRequest struct {
		backendIDsMessage
		NameFilter string `json:"name_filter,omitempty"`
	}
	idResponse struct {
		ArtifactID int64 `json:"artifactId,string"`
	}
)

func call[Resp any](ctx context.Context, c *Client, method string, req any) (Resp, error) {
	var resp Resp
	body, err := json.Marshal(req)
	if err != nil {
		return resp, err
	}
	endpoint := c.base.JoinPath("/twirp/github.actions.results.api.v1.ArtifactService", method).String()
	httpResp, err := c.Retry.do(ctx, c.Logf, method, func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
		return c.api.Do(httpReq)
	})
	if err != nil {
		return resp, fmt.Errorf("failed to %s: %w", method, err)
	}
	defer httpResp.Body.Close()
	err = json.UnmarshalRead(httpResp.Body, &resp)
	return resp, err
}

func (c *Client) Create(ctx context.Context, name, mimeType string, expiresAt *time.Time) (string, error) {
	resp, err := call[struct {
		SignedUploadURL string `json:"signedUploadUrl"`
	}](ctx, c, "CreateArtifact", createRequest{namedRequest{c.ids, name}, expiresAt, 7, mimeType})
	return resp.SignedUploadURL, err
}

func (c *Client) Finalize(ctx context.Context, name string, size int64, digest string) (int64, error) {
	resp, err := call[idResponse](ctx, c, "FinalizeArtifact", finalizeRequest{namedRequest{c.ids, name}, size, digest})
	return resp.ArtifactID, err
}

func (c *Client) List(ctx context.Context, name string) ([]Artifact, error) {
	resp, err := call[struct {
		Artifacts []Artifact `json:"artifacts"`
	}](ctx, c, "ListArtifacts", listRequest{c.ids, name})
	return resp.Artifacts, err
}

func (c *Client) SignedURL(ctx context.Context, name string) (string, error) {
	resp, err := call[struct {
		SignedURL string `json:"signedUrl"`
	}](ctx, c, "GetSignedArtifactURL", namedRequest{c.ids, name})
	return resp.SignedURL, err
}

func (c *Client) Delete(ctx context.Context, name string) (int64, error) {
	resp, err := call[idResponse](ctx, c, "DeleteArtifact", namedRequest{c.ids, name})
	return resp.ArtifactID, err
}
