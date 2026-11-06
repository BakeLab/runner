// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifact

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// BlockSize is the toolkit's upload chunk.
const BlockSize = 8 << 20

type Uploaded struct {
	Size   int64
	Digest string // sha256:<hex> for Finalize
}

// Upload puts body to a signed URL as parallel Azure blocks, failing when upload progress stalls for stall.
func (c *Client) Upload(ctx context.Context, signedURL, contentType string, body io.Reader, concurrency int, stall time.Duration) (result Uploaded, err error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer func() {
		if ctx.Err() != nil {
			err = context.Cause(ctx)
		}
		cancel(nil)
	}()
	stall = max(stall, time.Millisecond)
	timer := time.AfterFunc(stall, func() { cancel(errors.New("upload progress stalled")) })
	defer timer.Stop()
	hash := sha256.New()
	reader := io.TeeReader(body, hash)
	group, blockCtx := errgroup.WithContext(ctx)
	if closer, ok := body.(io.ReadCloser); ok {
		stop := context.AfterFunc(blockCtx, func() { closer.Close() })
		defer stop()
	}
	buffers := make(chan []byte, concurrency)
	prefix := rand.Text()
	var ids []string
	var uploaded atomic.Int64
	for index := 0; ; index++ {
		var buf []byte
		if index < concurrency {
			buf = make([]byte, BlockSize)
		} else {
			select {
			case buf = <-buffers:
			case <-blockCtx.Done():
				return Uploaded{}, group.Wait()
			}
		}
		n, readErr := io.ReadFull(reader, buf)
		if errors.Is(readErr, io.EOF) {
			break
		} else if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			if blockCtx.Err() == nil {
				cancel(readErr)
			}
			if err := group.Wait(); err != nil {
				return Uploaded{}, err
			}
			return Uploaded{}, readErr
		}
		id := base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "%s-%08d", prefix, index)) // fixed width as Azure needs, unique as Gitea before 1.25.5 shares a run's blocks
		ids = append(ids, id)
		group.Go(func() error {
			defer func() { buffers <- buf }()
			if err := c.put(blockCtx, signedURL+"&comp=block&blockid="+url.QueryEscape(id), "x-ms-blob-type", "BlockBlob", buf[:n]); err != nil {
				return err
			}
			timer.Reset(stall)
			c.Logf("Uploaded bytes %d", uploaded.Add(int64(n)))
			return nil
		})
		if readErr != nil {
			break
		}
	}
	if err := group.Wait(); err != nil {
		return Uploaded{}, err
	}
	var list strings.Builder
	list.WriteString("<BlockList>")
	for _, id := range ids {
		list.WriteString("<Latest>" + id + "</Latest>")
	}
	list.WriteString("</BlockList>")
	if err := c.put(ctx, signedURL+"&comp=blocklist", "x-ms-blob-content-type", contentType, []byte(list.String())); err != nil {
		return Uploaded{}, err
	}
	return Uploaded{Size: uploaded.Load(), Digest: "sha256:" + hex.EncodeToString(hash.Sum(nil))}, nil
}

func (c *Client) put(ctx context.Context, signedURL, header, value string, body []byte) error {
	resp, err := c.Retry.do(ctx, c.Logf, "blob upload", func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, signedURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set(header, value)
		resp, err := c.blob.Do(req)
		return resp, scrub(err)
	})
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Download GETs a signed URL, a read failing when nothing arrives for stall.
func (c *Client) Download(ctx context.Context, signedURL string, stall time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
	if err != nil {
		cancel(nil)
		return nil, err
	}
	resp, err := c.blob.Do(req)
	if err != nil {
		cancel(nil)
		return nil, scrub(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel(nil)
		return nil, fmt.Errorf("unexpected HTTP response from blob storage: %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	resp.Body = &stallReader{body: resp.Body, timer: time.AfterFunc(stall, func() {
		cancel(fmt.Errorf("blob storage chunk did not respond in %s", stall))
	}), stall: stall, cancel: cancel, ctx: ctx}
	return resp, nil
}

func scrub(err error) error { // the signature of a signed URL is a credential
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		if parsed, parseErr := url.Parse(urlErr.URL); parseErr == nil {
			parsed.RawQuery = ""
			urlErr.URL = parsed.String()
		} else {
			urlErr.URL = "<unparsable URL>"
		}
	}
	return err
}

type stallReader struct {
	body   io.ReadCloser
	timer  *time.Timer
	stall  time.Duration
	cancel context.CancelCauseFunc
	ctx    context.Context
}

func (r *stallReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 {
		r.timer.Reset(r.stall)
	}
	if err != nil && !errors.Is(err, io.EOF) && context.Cause(r.ctx) != nil {
		err = context.Cause(r.ctx)
	}
	return n, err
}

func (r *stallReader) Close() error {
	r.timer.Stop()
	r.cancel(nil)
	return r.body.Close()
}
