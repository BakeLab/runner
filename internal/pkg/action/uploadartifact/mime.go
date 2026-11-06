// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package uploadartifact

import (
	"cmp"
	"mime"
	"path"
)

func mimeTypeOf(name string) string {
	return cmp.Or(mime.TypeByExtension(path.Ext(name)), "application/octet-stream")
}
