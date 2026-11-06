// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package glob

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

// pattern ports @actions/glob internal-pattern.ts.
type pattern struct {
	negate            bool
	searchPath        string
	segments          []string
	trailingSeparator bool
	implicit          bool
	rootLiteral       string
	mm                []mmSegment
	w                 flavour
}

type parseEnv struct {
	w    flavour
	cwd  string
	home func() (string, error)
}

func newPattern(s string, implicit bool, env parseEnv) (*pattern, error) {
	w := env.w
	p := &pattern{implicit: implicit, w: w}
	s = jsTrim(s)
	for strings.HasPrefix(s, "!") {
		p.negate = !p.negate
		s = jsTrim(s[1:])
	}

	s, err := fixupPattern(s, env)
	if err != nil {
		return nil, err
	}
	p.segments = w.splitPath(s)
	p.trailingSeparator = strings.HasSuffix(w.normalizeSeparators(s), w.sep())
	s = w.safeTrimTrailingSeparator(s)

	var searchSegments []string
	for _, seg := range p.segments {
		lit := getLiteral(seg, w)
		if lit == "" {
			break
		}
		searchSegments = append(searchSegments, lit)
	}
	p.searchPath = w.joinSegments(searchSegments)
	p.rootLiteral = searchSegments[0]

	if w.windows {
		s = strings.ReplaceAll(s, `\`, "/")
	}
	p.mm, err = compileMinimatch(s, w.windows)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func fixupPattern(s string, env parseEnv) (string, error) {
	w := env.w
	if s == "" {
		return "", errors.New("pattern cannot be empty")
	}
	segments := w.splitPath(s)
	for i, seg := range segments {
		if lit := getLiteral(seg, w); (lit == "." && i != 0) || lit == ".." {
			return "", fmt.Errorf("invalid pattern '%s': relative pathing '.' and '..' is not allowed", s)
		}
	}
	if w.hasRoot(s) && getLiteral(segments[0], w) == "" {
		return "", fmt.Errorf("invalid pattern '%s': root segment must not contain globs", s)
	}

	s = w.normalizeSeparators(s)
	sep := w.sep()
	var root string
	var err error
	switch {
	case s == "." || strings.HasPrefix(s, "."+sep):
		s = globEscape(env.cwd, w) + s[1:]
	case s == "~" || strings.HasPrefix(s, "~"+sep):
		var home string
		if env.home != nil {
			if home, err = env.home(); err != nil {
				return "", err
			}
		}
		if home == "" {
			return "", errors.New("unable to determine HOME directory")
		}
		if home = w.normalizeSeparators(home); !w.hasAbsoluteRoot(home) {
			return "", fmt.Errorf("expected HOME directory to be a rooted path, actual '%s'", home)
		}
		s = globEscape(home, w) + s[1:]
	case w.windows && (reDriveOnly.MatchString(s) || reDriveNoSep.MatchString(s)):
		if root, err = w.ensureAbsoluteRoot(`C:\dummy-root`, s[:2], env.cwd); err != nil {
			return "", err
		}
		if len(s) > 2 && !strings.HasSuffix(root, `\`) {
			root += `\`
		}
		s = globEscape(root, w) + s[2:]
	case w.windows && (s == `\` || reRelRootOnly.MatchString(s)):
		if root, err = w.ensureAbsoluteRoot(`C:\dummy-root`, `\`, env.cwd); err != nil {
			return "", err
		}
		if !strings.HasSuffix(root, `\`) {
			root += `\`
		}
		s = globEscape(root, w) + s[1:]
	default:
		if s, err = w.ensureAbsoluteRoot(globEscape(env.cwd, w), s, env.cwd); err != nil {
			return "", err
		}
	}
	return w.normalizeSeparators(s), nil
}

func (p *pattern) match(itemPath string) MatchKind {
	w := p.w
	if p.segments[len(p.segments)-1] == "**" {
		itemPath = w.normalizeSeparators(itemPath)
		// Without the trailing separator minimatch would not match the directory preceding the globstar.
		if !strings.HasSuffix(itemPath, w.sep()) && !p.implicit {
			itemPath += w.sep()
		}
	} else {
		itemPath = w.safeTrimTrailingSeparator(itemPath)
	}
	if w.windows {
		itemPath = strings.ReplaceAll(itemPath, `\`, "/")
	}
	if !matchOne(splitRuns(itemPath, '/'), p.mm, false) {
		return MatchNone
	}
	if p.trailingSeparator {
		return MatchDirectory
	}
	return MatchAll
}

func (p *pattern) partialMatch(itemPath string) bool {
	w := p.w
	itemPath = w.safeTrimTrailingSeparator(itemPath)
	// The toolkit tests roots with an unanchored regexp because minimatch mishandles them.
	if w.dirname(itemPath) == itemPath {
		if w.windows {
			return strings.Contains(strings.ToUpper(itemPath), strings.ToUpper(p.rootLiteral))
		}
		return strings.Contains(itemPath, p.rootLiteral)
	}
	sep := byte('/')
	if w.windows {
		sep = '\\'
	}
	return matchOne(splitRuns(itemPath, sep), p.mm, true)
}

// globEscape ports Pattern.globEscape.
func globEscape(s string, w flavour) string {
	if !w.windows {
		s = strings.ReplaceAll(s, `\`, `\\`)
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '[' && closesBeforeSlash(s[i+1:]) {
			b.WriteString("[[]")
			continue
		}
		b.WriteByte(s[i])
	}
	s = strings.ReplaceAll(b.String(), "?", "[?]")
	return strings.ReplaceAll(s, "*", "[*]")
}

// closesBeforeSlash reports whether s matches the lookahead `[^/]+\]`.
func closesBeforeSlash(s string) bool {
	for i := 0; i < len(s) && s[i] != '/'; i++ {
		if s[i] == ']' && i > 0 {
			return true
		}
	}
	return false
}

// getLiteral ports Pattern.getLiteral, returning "" when the segment contains a glob.
func getLiteral(segment string, w flavour) string {
	seg := utf16.Encode([]rune(segment))
	var lit []uint16
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case c == '\\' && !w.windows && i+1 < len(seg):
			i++
			lit = append(lit, seg[i])
			continue
		case c == '*' || c == '?':
			return ""
		case c == '[' && i+1 < len(seg):
			var set []uint16
			closed := -1
			for i2 := i + 1; i2 < len(seg); i2++ {
				c2 := seg[i2]
				if c2 == '\\' && !w.windows && i2+1 < len(seg) {
					i2++
					set = append(set, seg[i2])
					continue
				} else if c2 == ']' {
					closed = i2
					break
				}
				set = append(set, c2)
			}
			if closed >= 0 {
				if len(set) > 1 {
					return ""
				}
				if len(set) == 1 {
					lit = append(lit, set...)
					i = closed
					continue
				}
			}
		}
		lit = append(lit, c)
	}
	return string(utf16.Decode(lit))
}

func isJSWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// jsTrim matches String.prototype.trim, which differs from strings.TrimSpace on U+0085 and U+FEFF.
func jsTrim(s string) string {
	return strings.TrimFunc(s, isJSWhitespace)
}

// splitRuns splits like JS s.split(/c+/).
func splitRuns(s string, c byte) []string {
	parts := []string{}
	start := 0
	for i := 0; i < len(s); {
		if s[i] != c {
			i++
			continue
		}
		parts = append(parts, s[start:i])
		for i < len(s) && s[i] == c {
			i++
		}
		start = i
	}
	return append(parts, s[start:])
}
