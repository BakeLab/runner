// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package glob

import (
	"errors"
	"regexp"
	"strings"
)

// flavour selects Windows or POSIX path semantics; the helpers port @actions/glob internal-path-helper.ts.
type flavour struct{ windows bool }

var (
	reUNCPrefix    = regexp.MustCompile(`^\\\\+[^\\]`)
	reBackslashRun = regexp.MustCompile(`\\\\+`)
	reSlashRun     = regexp.MustCompile(`//+`)
	reDriveRoot    = regexp.MustCompile(`^[A-Za-z]:\\$`)
	reDriveAbs     = regexp.MustCompile(`^[A-Za-z]:\\`)
	reDrive        = regexp.MustCompile(`^[A-Za-z]:`)
	reDriveOnly    = regexp.MustCompile(`^[A-Za-z]:$`)
	reDriveRel     = regexp.MustCompile(`^[A-Za-z]:[^\\/]|^[A-Za-z]:$`)
	reRelRoot      = regexp.MustCompile(`^\\$|^\\[^\\]`)
	reRelRootOnly  = regexp.MustCompile(`^\\[^\\]`)
	reDriveNoSep   = regexp.MustCompile(`^[A-Za-z]:[^\\]`)
	reUNCRoot      = regexp.MustCompile(`^\\\\[^\\]+(\\[^\\]+)?$`)
	reUNCRootSlash = regexp.MustCompile(`^\\\\[^\\]+\\[^\\]+\\$`)
)

func (w flavour) sep() string {
	if w.windows {
		return `\`
	}
	return "/"
}

func (w flavour) isSep(c byte) bool {
	return c == '/' || (w.windows && c == '\\')
}

func (w flavour) normalizeSeparators(p string) string {
	if w.windows {
		p = strings.ReplaceAll(p, "/", `\`)
		prefix := ""
		if reUNCPrefix.MatchString(p) {
			prefix = `\`
		}
		return prefix + reBackslashRun.ReplaceAllLiteralString(p, `\`)
	}
	return reSlashRun.ReplaceAllLiteralString(p, "/")
}

func (w flavour) safeTrimTrailingSeparator(p string) string {
	if p == "" {
		return ""
	}
	p = w.normalizeSeparators(p)
	if !strings.HasSuffix(p, w.sep()) || p == w.sep() || (w.windows && reDriveRoot.MatchString(p)) {
		return p
	}
	return p[:len(p)-1]
}

func (w flavour) hasAbsoluteRoot(p string) bool {
	p = w.normalizeSeparators(p)
	if w.windows {
		return strings.HasPrefix(p, `\\`) || reDriveAbs.MatchString(p)
	}
	return strings.HasPrefix(p, "/")
}

func (w flavour) hasRoot(p string) bool {
	p = w.normalizeSeparators(p)
	if w.windows {
		return strings.HasPrefix(p, `\`) || reDrive.MatchString(p)
	}
	return strings.HasPrefix(p, "/")
}

func (w flavour) dirname(p string) string {
	p = w.safeTrimTrailingSeparator(p)
	if w.windows && reUNCRoot.MatchString(p) {
		return p
	}
	result := w.nodeDirname(p)
	if w.windows && reUNCRootSlash.MatchString(result) {
		result = w.safeTrimTrailingSeparator(result)
	}
	return result
}

// ensureAbsoluteRoot resolves Windows drive and root relative paths against cwd like process.cwd().
func (w flavour) ensureAbsoluteRoot(root, itemPath, cwd string) (string, error) {
	if root == "" || itemPath == "" {
		return "", errors.New("ensureAbsoluteRoot parameters must not be empty")
	}
	if w.hasAbsoluteRoot(itemPath) {
		return itemPath, nil
	}
	if w.windows {
		if reDriveRel.MatchString(itemPath) {
			if !reDriveAbs.MatchString(cwd) {
				return "", errors.New("expected current directory to start with an absolute drive root, actual '" + cwd + "'")
			}
			if strings.EqualFold(itemPath[:1], cwd[:1]) {
				if len(itemPath) == 2 {
					return itemPath[:1] + `:\` + cwd[3:], nil
				}
				if !strings.HasSuffix(cwd, `\`) {
					cwd += `\`
				}
				return itemPath[:1] + `:\` + cwd[3:] + itemPath[2:], nil
			}
			return itemPath[:1] + `:\` + itemPath[2:], nil
		} else if reRelRoot.MatchString(w.normalizeSeparators(itemPath)) {
			if !reDriveAbs.MatchString(cwd) {
				return "", errors.New("expected current directory to start with an absolute drive root, actual '" + cwd + "'")
			}
			return cwd[:1] + `:\` + itemPath[1:], nil
		}
	}
	if !w.hasAbsoluteRoot(root) {
		return "", errors.New("ensureAbsoluteRoot parameter 'root' must have an absolute root")
	}
	if !strings.HasSuffix(root, "/") && (!w.windows || !strings.HasSuffix(root, `\`)) {
		root += w.sep()
	}
	return root + itemPath, nil
}

// splitPath ports internal-path.ts Path(string).segments.
func (w flavour) splitPath(p string) []string {
	p = w.safeTrimTrailingSeparator(p)
	if !w.hasRoot(p) {
		return strings.Split(p, w.sep())
	}
	var segments []string
	remaining := p
	for dir := w.dirname(remaining); dir != remaining; dir = w.dirname(remaining) {
		segments = append([]string{w.nodeBasename(remaining)}, segments...)
		remaining = dir
	}
	return append([]string{remaining}, segments...)
}

// joinSegments ports internal-path.ts Path(string[]).toString().
func (w flavour) joinSegments(segments []string) string {
	result := w.normalizeSeparators(segments[0])
	if w.hasRoot(result) {
		result = w.safeTrimTrailingSeparator(result)
	}
	skipSlash := strings.HasSuffix(result, w.sep()) || (w.windows && reDriveOnly.MatchString(result))
	for _, s := range segments[1:] {
		if skipSlash {
			skipSlash = false
		} else {
			result += w.sep()
		}
		result += w.normalizeSeparators(s)
	}
	return result
}

func isDeviceRoot(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// nodeDirname ports Node's path.posix.dirname and path.win32.dirname.
func (w flavour) nodeDirname(p string) string {
	n := len(p)
	if n == 0 {
		return "."
	}
	if !w.windows {
		hasRoot := p[0] == '/'
		end := -1
		matchedSlash := true
		for i := n - 1; i >= 1; i-- {
			if p[i] == '/' {
				if !matchedSlash {
					end = i
					break
				}
			} else {
				matchedSlash = false
			}
		}
		switch {
		case end == -1 && hasRoot:
			return "/"
		case end == -1:
			return "."
		case hasRoot && end == 1:
			return "//"
		}
		return p[:end]
	}
	if n == 1 {
		if w.isSep(p[0]) {
			return p
		}
		return "."
	}
	rootEnd, offset := -1, 0
	if w.isSep(p[0]) {
		rootEnd, offset = 1, 1
		if w.isSep(p[1]) {
			j := 2
			last := j
			for j < n && !w.isSep(p[j]) {
				j++
			}
			if j < n && j != last {
				last = j
				for j < n && w.isSep(p[j]) {
					j++
				}
				if j < n && j != last {
					last = j
					for j < n && !w.isSep(p[j]) {
						j++
					}
					if j == n {
						return p
					}
					if j != last {
						rootEnd, offset = j+1, j+1
					}
				}
			}
		}
	} else if isDeviceRoot(p[0]) && p[1] == ':' {
		rootEnd = 2
		if n > 2 && w.isSep(p[2]) {
			rootEnd = 3
		}
		offset = rootEnd
	}
	end := -1
	matchedSlash := true
	for i := n - 1; i >= offset; i-- {
		if w.isSep(p[i]) {
			if !matchedSlash {
				end = i
				break
			}
		} else {
			matchedSlash = false
		}
	}
	if end == -1 {
		if rootEnd == -1 {
			return "."
		}
		end = rootEnd
	}
	return p[:end]
}

// nodeBasename ports Node's path.posix.basename and path.win32.basename without the ext argument.
func (w flavour) nodeBasename(p string) string {
	start := 0
	if w.windows && len(p) >= 2 && isDeviceRoot(p[0]) && p[1] == ':' {
		start = 2
	}
	from, end := start, -1
	matchedSlash := true
	for i := len(p) - 1; i >= start; i-- {
		if w.isSep(p[i]) {
			if !matchedSlash {
				from = i + 1
				break
			}
		} else if end == -1 {
			matchedSlash = false
			end = i + 1
		}
	}
	if end == -1 {
		return ""
	}
	return p[from:end]
}
