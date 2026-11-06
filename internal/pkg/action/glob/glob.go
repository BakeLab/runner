// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package glob ports the pattern semantics of the JavaScript @actions/glob 0.6.1 package, as
// bundled in actions/upload-artifact, without walking a filesystem. Malformed bracket expressions
// and some non-ASCII case folding on Windows can match differently.
package glob

import (
	"fmt"
	"path"
	"strings"
	"sync"
)

type Options struct {
	Workdir             string                 // absolute; relative patterns resolve against it
	Home                func() (string, error) // replaces ~, called once and only when a pattern uses it
	Windows             bool                   // Windows path flavour
	ImplicitDescendants bool
}

type MatchKind int

const (
	MatchNone      MatchKind = 0
	MatchDirectory MatchKind = 1
	MatchFile      MatchKind = 2
	MatchAll                 = MatchDirectory | MatchFile
)

type Globber struct {
	patterns    []*pattern
	searchPaths []string
}

// Parse parses newline separated patterns like DefaultGlobber.create.
func Parse(patterns string, o Options) (*Globber, error) {
	w := flavour{o.Windows}
	env := parseEnv{w: w, cwd: w.normalizeSeparators(o.Workdir)}
	if o.Home != nil {
		env.home = sync.OnceValues(o.Home)
	}
	if env.cwd == "" || !w.hasAbsoluteRoot(env.cwd) {
		return nil, fmt.Errorf("workdir '%s' is not an absolute path", o.Workdir)
	}
	if o.Windows {
		patterns = strings.ReplaceAll(patterns, "\r\n", "\n")
		patterns = strings.ReplaceAll(patterns, "\r", "\n")
	}

	var parsed []*pattern
	for line := range strings.SplitSeq(patterns, "\n") {
		line = jsTrim(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, err := newPattern(line, false, env)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, p)
	}

	g := &Globber{searchPaths: searchPaths(parsed, w)}
	for _, p := range parsed {
		g.patterns = append(g.patterns, p)
		if o.ImplicitDescendants && (p.trailingSeparator || p.segments[len(p.segments)-1] != "**") {
			s := jsTrim(w.joinSegments(append(append([]string(nil), p.segments...), "**")))
			if p.negate {
				s = "!" + s
			}
			implicit, err := newPattern(s, true, env)
			if err != nil {
				return nil, err
			}
			g.patterns = append(g.patterns, implicit)
		}
	}
	return g, nil
}

// SearchPaths returns the literal prefix of each include pattern, without duplicates and
// paths nested under another search path.
func (g *Globber) SearchPaths() []string {
	return append([]string(nil), g.searchPaths...)
}

// Match ORs in each include pattern and masks out each exclude pattern, in order.
func (g *Globber) Match(p string) MatchKind {
	result := MatchNone
	for _, pat := range g.patterns {
		if pat.negate {
			result &^= pat.match(p)
		} else {
			result |= pat.match(p)
		}
	}
	return result
}

// PartialMatch reports whether an include pattern may match descendants of p.
func (g *Globber) PartialMatch(p string) bool {
	for _, pat := range g.patterns {
		if !pat.negate && pat.partialMatch(p) {
			return true
		}
	}
	return false
}

func searchPaths(patterns []*pattern, w flavour) []string {
	key := func(p *pattern) string {
		if w.windows {
			return strings.ToUpper(p.searchPath)
		}
		return p.searchPath
	}
	const candidate, included = 1, 2
	seen := map[string]int{}
	for _, p := range patterns {
		if !p.negate {
			seen[key(p)] = candidate
		}
	}
	var result []string
	for _, p := range patterns {
		if p.negate {
			continue
		}
		k := key(p)
		if seen[k] == included {
			continue
		}
		foundAncestor := false
		for temp, parent := k, w.dirname(k); parent != temp; temp, parent = parent, w.dirname(parent) {
			if seen[parent] != 0 {
				foundAncestor = true
				break
			}
		}
		if !foundAncestor {
			result = append(result, p.searchPath)
			seen[k] = included
		}
	}
	return result
}

// LeastCommonAncestor ports getMultiPathLCA from upload-artifact's search.ts, which becomes the
// artifact root when there are several search paths. Segments compare case-sensitively there,
// even on Windows.
func LeastCommonAncestor(paths []string, windows bool) string {
	switch len(paths) {
	case 0:
		return ""
	case 1:
		return paths[0]
	}
	w := flavour{windows}
	sep := w.sep()
	split := make([][]string, len(paths))
	smallest := -1
	for i, p := range paths {
		if w.windows {
			p = w.normalizeSeparators(p)
		} else {
			p = path.Clean(p)
		}
		split[i] = strings.Split(p, sep)
		if smallest < 0 || len(split[i]) < smallest {
			smallest = len(split[i])
		}
	}

	var common []string
	if strings.HasPrefix(paths[0], sep) {
		common = append(common, sep)
	}
	for i := 0; i < smallest; i++ {
		same := true
		for _, s := range split[1:] {
			if s[i] != split[0][i] {
				same = false
				break
			}
		}
		if !same {
			break
		}
		common = append(common, split[0][i])
	}
	return w.nodeJoin(common)
}

// nodeJoin ports Node's path.join for the already normalized segments LeastCommonAncestor produces.
func (w flavour) nodeJoin(parts []string) string {
	var nonEmpty []string
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	if len(nonEmpty) == 0 {
		return "."
	}
	joined := strings.Join(nonEmpty, w.sep())
	if !w.windows {
		return path.Clean(joined)
	}
	first := nonEmpty[0]
	needsReplace, slashCount := true, 0
	if w.isSep(first[0]) {
		slashCount++
		if len(first) > 1 && w.isSep(first[1]) {
			slashCount++
			if len(first) > 2 {
				if w.isSep(first[2]) {
					slashCount++
				} else {
					needsReplace = false
				}
			}
		}
	}
	if needsReplace {
		for slashCount < len(joined) && w.isSep(joined[slashCount]) {
			slashCount++
		}
		if slashCount >= 2 {
			joined = `\` + joined[slashCount:]
		}
	}
	// path.win32.normalize turns a bare drive into the drive relative "C:.".
	if reDriveOnly.MatchString(joined) {
		return joined + "."
	}
	return joined
}
