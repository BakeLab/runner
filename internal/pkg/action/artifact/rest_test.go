// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifact

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRESTListsEveryPageAndSkipsExpired(t *testing.T) {
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/repos/own%20er/repo/actions/runs/12/artifacts", r.URL.EscapedPath())
		assert.Equal(t, "token secret", r.Header.Get("Authorization"))
		assert.Empty(t, r.URL.Query().Get("per_page"))
		pages = append(pages, r.URL.Query().Get("page")+"/"+r.URL.Query().Get("limit"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		const maxResponseItems = 20
		var entries []string
		for id := (page-1)*maxResponseItems + 1; id <= min(page*maxResponseItems, 53); id++ {
			entries = append(entries, fmt.Sprintf(`{"id":%d,"name":"a%d","size_in_bytes":%d,"expired":%t}`, id, id, id, id == 2))
		}
		_, _ = io.WriteString(w, `{"total_count":53,"artifacts":[`+strings.Join(entries, ",")+`]}`)
	}))
	defer server.Close()
	rest, err := NewREST(server.URL, "secret", nil)
	require.NoError(t, err)
	list, err := rest.List(t.Context(), "own er", "repo", 12, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"1/50", "2/50", "3/50"}, pages)
	assert.Len(t, list, 52)
	assert.Equal(t, Artifact{ID: 3, Name: "a3", Size: 3}, list[1])
}
