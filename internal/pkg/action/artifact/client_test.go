// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/artifacts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientRoundTrip(t *testing.T) {
	router := http.NewServeMux()
	artifacts.RegisterV4(router, t.TempDir())
	var blocks atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("comp") == "block" {
			blocks.Add(1)
		}
		router.ServeHTTP(writer, request)
	}))
	defer server.Close()
	client, err := New(server.URL, artifacts.LocalRuntimeToken("7", time.Hour), nil)
	require.NoError(t, err)
	for _, content := range [][]byte{nil, bytes.Repeat([]byte("payload"), 2*BlockSize/7+123)} {
		sum := sha256.Sum256(content)
		uploadURL, err := client.Create(t.Context(), "art", "text/plain", nil)
		require.NoError(t, err)
		uploaded, err := client.Upload(t.Context(), uploadURL, "text/plain", bytes.NewReader(content), 2, time.Minute)
		require.NoError(t, err)
		assert.Equal(t, int32((len(content)+BlockSize-1)/BlockSize), blocks.Swap(0))
		assert.Equal(t, Uploaded{Size: int64(len(content)), Digest: "sha256:" + hex.EncodeToString(sum[:])}, uploaded)
		id, err := client.Finalize(t.Context(), "art", uploaded.Size, uploaded.Digest)
		require.NoError(t, err)
		listed, err := client.List(t.Context(), "art")
		require.NoError(t, err)
		assert.Equal(t, []Artifact{{ID: id, Name: "art", Size: int64(len(content)), Digest: uploaded.Digest}}, listed)
		signed, err := client.SignedURL(t.Context(), "art")
		require.NoError(t, err)
		resp, err := client.Download(t.Context(), signed, time.Minute)
		require.NoError(t, err)
		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		assert.True(t, bytes.Equal(content, got))
		deleted, err := client.Delete(t.Context(), "art")
		require.NoError(t, err)
		assert.Equal(t, id, deleted)
		_, err = client.SignedURL(t.Context(), "art")
		assert.True(t, IsNotFound(err), err)
	}
}

func TestClientRetries(t *testing.T) {
	var calls atomic.Int32
	var blockIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			blockIDs = append(blockIDs, r.URL.Query().Get("blockid"))
			w.WriteHeader(http.StatusCreated)
			return
		}
		body, _ := io.ReadAll(r.Body)
		assert.NotContains(t, string(body), "mime_type", "older Gitea rejects the field")
		assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey"), "the runtime token is the bearer")
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			http.Error(w, "Error create or get artifact", http.StatusInternalServerError)
		case 3:
			w.WriteHeader(http.StatusFound)
			_, _ = io.WriteString(w, `{"ok":true,"signedUploadUrl":"http://blob/u?sig=x"}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":"invalid_argument","msg":"bad name"}`)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, artifacts.LocalRuntimeToken("7", time.Hour), nil)
	require.NoError(t, err)
	client.Retry = Retry{Attempts: 3}
	var logged []string
	client.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	uploadURL, err := client.Create(t.Context(), "art", "", nil)
	require.NoError(t, err)
	assert.Equal(t, "http://blob/u?sig=x", uploadURL)
	assert.Contains(t, logged[1], "Error create or get artifact")

	_, err = client.Create(t.Context(), "art", "", nil)
	require.ErrorContains(t, err, "(400) Bad Request: bad name")
	assert.Equal(t, int32(4), calls.Load(), "a 400 is not retried")

	for range 2 {
		_, err = client.Upload(t.Context(), server.URL+"/u?sig=secret", "application/zip", strings.NewReader("x"), 1, time.Minute)
		require.NoError(t, err)
	}
	assert.NotEqual(t, blockIDs[0], blockIDs[2], "each upload names its blocks anew")
	server.Close()
	_, err = client.Upload(t.Context(), server.URL+"/u?sig=secret", "application/zip", strings.NewReader("x"), 1, time.Minute)
	require.Error(t, err)
	assert.NotContains(t, err.Error()+strings.Join(logged, ""), "secret")
}

func TestTransferStalls(t *testing.T) {
	for _, test := range []struct{ name, want string }{
		{"api", "Client.Timeout"},
		{"failed upload", "(400) Bad Request"},
		{"source", "upload progress stalled"},
		{"commit", "upload progress stalled"},
		{"download", "did not respond"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_, _ = io.Copy(io.Discard, request.Body)
				if test.name == "failed upload" {
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				if test.name == "download" {
					_, _ = io.WriteString(writer, "partial")
					writer.(http.Flusher).Flush()
				}
				<-request.Context().Done()
			}))
			defer server.Close()
			client, err := New(server.URL, artifacts.LocalRuntimeToken("7", time.Hour), nil)
			require.NoError(t, err)
			client.Retry = Retry{Attempts: 1}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			switch test.name {
			case "api":
				assert.Equal(t, 3*time.Minute, client.api.Timeout)
				client.api.Timeout = time.Millisecond
				_, err = client.Create(ctx, "art", "", nil)
			case "download":
				var resp *http.Response
				resp, err = client.Download(ctx, server.URL, time.Millisecond)
				require.NoError(t, err)
				defer resp.Body.Close()
				_, err = io.ReadAll(resp.Body)
			case "commit":
				_, err = client.Upload(ctx, server.URL+"?sig=x", "text/plain", strings.NewReader(""), 1, time.Millisecond)
			default:
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				done := make(chan error, 1)
				stall := time.Millisecond
				if test.name == "failed upload" {
					stall = time.Minute
					go func() {
						_, err := writer.Write(make([]byte, BlockSize))
						done <- err
					}()
				}
				_, err = client.Upload(ctx, server.URL+"?sig=x", "text/plain", reader, 2, stall)
				if test.name == "failed upload" {
					require.NoError(t, <-done)
				}
			}
			require.ErrorContains(t, err, test.want)
		})
	}
}
