// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package common

import "strings"

// LookupFold finds name in values, ignoring case where the environment does, as GitHub's inputs always do.
func LookupFold[V any](values map[string]V, name string, caseInsensitive bool) (V, bool) {
	if value, ok := values[name]; ok || !caseInsensitive {
		return value, ok
	}
	for key, value := range values {
		if strings.EqualFold(key, name) {
			return value, true
		}
	}
	var zero V
	return zero, false
}
