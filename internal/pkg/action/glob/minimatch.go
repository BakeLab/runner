// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package glob

import (
	"errors"
	"slices"
	"unicode"
	"unicode/utf16"
)

// This ports the subset of minimatch 3.1.5 that @actions/glob 0.6.1 uses: options dot, nobrace,
// noext, nocomment and nonegate, with nocase on Windows. Like JS regexps without the u flag it
// works on UTF-16 code units.

const maxGlobstarRecursion = 200

type tokKind uint8

const (
	tokLit      tokKind = iota
	tokAny              // [^/]
	tokStar             // [^/]*?
	tokClass            // [...]
	tokNonEmpty         // (?=.)
	tokNotDot           // (?!(?:^|\/)\.{1,2}(?:$|\/))
)

type unitRange struct{ lo, hi uint16 }

type tok struct {
	kind   tokKind
	ch     uint16
	neg    bool
	ranges []unitRange
}

type mmSegment struct {
	globstar bool
	magic    bool
	nocase   bool
	literal  string
	toks     []tok
}

type classItem struct {
	ch      uint16
	escaped bool
}

func isReSpecial(c uint16) bool {
	switch c {
	case '(', ')', '.', '*', '{', '}', '+', '?', '[', ']', '^', '$', '\\', '!':
		return true
	}
	return false
}

func compileMinimatch(pattern string, nocase bool) ([]mmSegment, error) {
	if len(utf16.Encode([]rune(pattern))) > 64*1024 {
		return nil, errors.New("pattern is too long")
	}
	parts := splitRuns(jsTrim(pattern), '/')
	segs := make([]mmSegment, len(parts))
	for i, part := range parts {
		segs[i] = compileSegment(part, nocase)
	}
	return segs, nil
}

func compileSegment(s string, nocase bool) mmSegment {
	if s == "**" {
		return mmSegment{globstar: true}
	}
	if s == "" {
		return mmSegment{}
	}
	p := utf16.Encode([]rune(s))
	toks, magic := parseSegment(p, nocase)
	if !magic {
		return mmSegment{literal: globUnescape(s)}
	}
	return mmSegment{magic: true, nocase: nocase, toks: toks}
}

// globUnescape replaces \x with x like s.replace(/\\(.)/g, '$1'); JS . excludes line terminators.
func globUnescape(s string) string {
	u := utf16.Encode([]rune(s))
	out := make([]uint16, 0, len(u))
	for i := 0; i < len(u); i++ {
		if u[i] == '\\' && i+1 < len(u) && !isLineTerminator(u[i+1]) {
			i++
		}
		out = append(out, u[i])
	}
	return string(utf16.Decode(out))
}

// parseSegment ports minimatch's parse(), emitting tokens instead of a regexp source.
func parseSegment(p []uint16, nocase bool) (toks []tok, hasMagic bool) {
	hasMagic = nocase
	escaping := false
	var stateChar uint16
	inClass := false
	classStart := -1
	var class []classItem
	lit := func(c uint16) { toks = append(toks, tok{kind: tokLit, ch: c}) }
	clearStateChar := func() {
		switch stateChar {
		case 0:
			return
		case '*':
			toks = append(toks, tok{kind: tokStar})
			hasMagic = true
		case '?':
			toks = append(toks, tok{kind: tokAny})
			hasMagic = true
		default:
			lit(stateChar)
		}
		stateChar = 0
	}
	fallback := func(cs []uint16, closing bool) {
		lit('[')
		// parse() returns a non-array for "" and "**", so minimatch splices in the text "undefined".
		if len(cs) == 0 || (len(cs) == 2 && cs[0] == '*' && cs[1] == '*') {
			for _, c := range "undefined" {
				lit(uint16(c))
			}
			if closing {
				lit(']')
			}
			inClass = false
			return
		}
		subToks, subMagic := parseSegment(cs, nocase)
		toks = append(toks, subToks...)
		if closing {
			lit(']')
		}
		hasMagic = hasMagic || subMagic
		inClass = false
	}

	for i := range p {
		c := p[i]
		if escaping && isReSpecial(c) {
			if inClass {
				class = append(class, classItem{c, true})
			} else {
				lit(c)
			}
			escaping = false
			continue
		}
		switch c {
		case '\\':
			clearStateChar()
			escaping = true
		case '?', '*', '+', '@', '!':
			if inClass {
				if c == '!' && i == classStart+1 {
					c = '^'
				}
				class = append(class, classItem{c, false})
				continue
			}
			clearStateChar()
			stateChar = c
			clearStateChar() // noext
		case '(':
			if inClass {
				class = append(class, classItem{c, false})
			} else {
				lit(c)
			}
		case ')':
			if inClass {
				class = append(class, classItem{c, true})
			} else {
				lit(c)
			}
		case '|':
			if inClass {
				class = append(class, classItem{c, true})
			} else {
				lit(c)
			}
			escaping = false
		case '[':
			clearStateChar()
			if inClass {
				class = append(class, classItem{c, true})
				continue
			}
			inClass = true
			classStart = i
			class = class[:0]
		case ']':
			if i == classStart+1 || !inClass {
				if inClass {
					class = append(class, classItem{c, true})
				} else {
					lit(c)
				}
				escaping = false
				continue
			}
			t, ok := buildClass(class)
			if !ok {
				fallback(p[classStart+1:i], true)
				continue
			}
			toks = append(toks, t)
			hasMagic = true
			inClass = false
		default:
			clearStateChar()
			if inClass {
				class = append(class, classItem{c, !escaping && isReSpecial(c) && c != '^'})
			} else {
				lit(c)
			}
			escaping = false
		}
	}

	if inClass {
		fallback(p[classStart+1:], false)
	}
	clearStateChar()
	if escaping {
		lit('\\')
	}

	addPatternStart := len(toks) > 0 && (toks[0].kind == tokStar || toks[0].kind == tokAny || toks[0].kind == tokClass)
	if len(toks) > 0 && hasMagic {
		toks = append([]tok{{kind: tokNonEmpty}}, toks...)
	}
	if addPatternStart && p[0] != '.' {
		toks = append([]tok{{kind: tokNotDot}}, toks...)
	}
	return toks, hasMagic
}

// buildClass turns the class body minimatch emits into ranges, ok is false for an out of order range like [z-a], which matches literally.
func buildClass(items []classItem) (tok, bool) {
	t := tok{kind: tokClass}
	if len(items) > 0 && items[0].ch == '^' && !items[0].escaped {
		t.neg = true
		items = items[1:]
	}
	for i := 0; i < len(items); {
		lo := items[i].ch
		if i+2 < len(items) && items[i+1].ch == '-' && !items[i+1].escaped {
			hi := items[i+2].ch
			if lo > hi {
				return t, false
			}
			t.ranges = append(t.ranges, unitRange{lo, hi})
			i += 3
			continue
		}
		t.ranges = append(t.ranges, unitRange{lo, lo})
		i++
	}
	return t, true
}

func isLineTerminator(c uint16) bool {
	return c == '\n' || c == '\r' || c == 0x2028 || c == 0x2029
}

// canonicalize ports the ES Canonicalize() used by non-unicode case-insensitive regexps.
func canonicalize(c uint16) uint16 {
	if utf16.IsSurrogate(rune(c)) {
		return c
	}
	up := unicode.ToUpper(rune(c))
	if up > 0xffff || (c >= 128 && up < 128) {
		return c
	}
	return uint16(up)
}

func (t *tok) classMatch(c uint16, nocase bool) bool {
	in := func(v uint16) bool {
		for _, r := range t.ranges {
			if v >= r.lo && v <= r.hi {
				return true
			}
		}
		return false
	}
	hit := in(c)
	if !hit && nocase && !utf16.IsSurrogate(rune(c)) {
		want := canonicalize(c)
		for f := unicode.SimpleFold(rune(c)); f != rune(c); f = unicode.SimpleFold(f) {
			if f <= 0xffff && canonicalize(uint16(f)) == want && in(uint16(f)) {
				hit = true
				break
			}
		}
	}
	return hit != t.neg
}

func (s *mmSegment) match(f string) bool {
	if !s.magic {
		return f == s.literal
	}
	u := utf16.Encode([]rune(f))
	n := len(u) + 1
	memo := make([]int8, (len(s.toks)+1)*n)
	var rec func(ti, pos int) bool
	rec = func(ti, pos int) bool {
		if ti == len(s.toks) {
			return pos == len(u)
		}
		key := ti*n + pos
		if memo[key] != 0 {
			return memo[key] > 0
		}
		t := &s.toks[ti]
		hit := false
		switch t.kind {
		case tokLit:
			hit = pos < len(u) && (u[pos] == t.ch || (s.nocase && canonicalize(u[pos]) == canonicalize(t.ch))) && rec(ti+1, pos+1)
		case tokAny:
			hit = pos < len(u) && u[pos] != '/' && rec(ti+1, pos+1)
		case tokClass:
			hit = pos < len(u) && t.classMatch(u[pos], s.nocase) && rec(ti+1, pos+1)
		case tokStar:
			for j := pos; !hit; j++ {
				hit = rec(ti+1, j)
				if j == len(u) || u[j] == '/' {
					break
				}
			}
		case tokNonEmpty:
			hit = pos < len(u) && !isLineTerminator(u[pos]) && rec(ti+1, pos)
		case tokNotDot:
			hit = !dotSegmentAhead(u, pos) && rec(ti+1, pos)
		}
		memo[key] = -1
		if hit {
			memo[key] = 1
		}
		return hit
	}
	return rec(0, 0)
}

// dotSegmentAhead evaluates (?:^|\/)\.{1,2}(?:$|\/) at pos.
func dotSegmentAhead(u []uint16, pos int) bool {
	i := pos
	if pos != 0 {
		if i >= len(u) || u[i] != '/' {
			return false
		}
		i++
	}
	for dots := 0; dots < 2 && i < len(u) && u[i] == '.'; dots++ {
		i++
		if i == len(u) || u[i] == '/' {
			return true
		}
	}
	return false
}

// matchOne ports Minimatch.prototype.matchOne for the single pattern set nobrace produces.
func matchOne(file []string, pattern []mmSegment, partial bool) bool {
	for i := range pattern {
		if pattern[i].globstar {
			return matchGlobstar(file, pattern, partial, 0, 0)
		}
	}
	return matchOneFrom(file, pattern, partial, 0)
}

func clampSlice(s []string, from, to int) []string {
	to = min(to, len(s))
	from = min(from, to)
	return s[from:to]
}

func matchOneFrom(file []string, pattern []mmSegment, partial bool, fi int) bool {
	fl, pl, pi := len(file), len(pattern), 0
	for ; fi < fl && pi < pl; fi, pi = fi+1, pi+1 {
		if pattern[pi].globstar || !pattern[pi].match(file[fi]) {
			return false
		}
	}
	switch {
	case fi == fl && pi == pl:
		return true
	case fi == fl:
		return partial
	case pi == pl:
		return fi == fl-1 && file[fi] == ""
	}
	return false
}

func isDotOrDotDot(f string) bool {
	return f == "." || f == ".."
}

func matchGlobstar(file []string, pattern []mmSegment, partial bool, fileIndex, patternIndex int) bool {
	firstgs, lastgs := -1, -1
	for i := patternIndex; i < len(pattern); i++ {
		if pattern[i].globstar {
			firstgs = i
			break
		}
	}
	for i, seg := range slices.Backward(pattern) {
		if seg.globstar {
			lastgs = i
			break
		}
	}
	head := pattern[patternIndex:firstgs]
	var body, tail []mmSegment
	if partial {
		body = pattern[firstgs+1:]
	} else {
		body = pattern[firstgs+1 : max(lastgs, firstgs+1)]
		tail = pattern[lastgs+1:]
	}

	if len(head) > 0 {
		if !matchOneFrom(clampSlice(file, fileIndex, fileIndex+len(head)), head, partial, 0) {
			return false
		}
		fileIndex += len(head)
	}

	fileTailMatch := 0
	if len(tail) > 0 {
		if len(tail)+fileIndex > len(file) {
			return false
		}
		tailStart := len(file) - len(tail)
		if matchOneFrom(file, tail, partial, tailStart) {
			fileTailMatch = len(tail)
		} else {
			// a/**/* matching a/b/
			if file[len(file)-1] != "" || fileIndex+len(tail) == len(file) {
				return false
			}
			tailStart--
			if !matchOneFrom(file, tail, partial, tailStart) {
				return false
			}
			fileTailMatch = len(tail) + 1
		}
	}

	if len(body) == 0 {
		sawSome := fileTailMatch > 0
		for i := fileIndex; i < len(file)-fileTailMatch; i++ {
			sawSome = true
			if isDotOrDotDot(file[i]) {
				return false
			}
		}
		return partial || sawSome
	}

	type section struct {
		body  []mmSegment
		after int
	}
	sections := []section{{}}
	nonGsParts := 0
	nonGsPartsSums := []int{0}
	for _, b := range body {
		if b.globstar {
			nonGsPartsSums = append(nonGsPartsSums, nonGsParts)
			sections = append(sections, section{})
		} else {
			sections[len(sections)-1].body = append(sections[len(sections)-1].body, b)
			nonGsParts++
		}
	}
	idx := len(sections) - 1
	fileLength := len(file) - fileTailMatch
	for si := range sections {
		sections[si].after = fileLength - (nonGsPartsSums[idx] + len(sections[si].body))
		idx--
	}

	// minimatch returns false (no match), null (no match, stop trying) or true.
	const (
		triFalse = iota
		triNull
		triTrue
	)
	var bodySections func(fileIndex, bodyIndex, depth int, sawTail bool) int
	bodySections = func(fileIndex, bodyIndex, depth int, sawTail bool) int {
		if bodyIndex >= len(sections) {
			for i := fileIndex; i < len(file); i++ {
				sawTail = true
				if isDotOrDotDot(file[i]) {
					return triFalse
				}
			}
			if sawTail {
				return triTrue
			}
			return triFalse
		}
		bs := sections[bodyIndex]
		for ; fileIndex <= bs.after; fileIndex++ {
			m := matchOneFrom(clampSlice(file, 0, fileIndex+len(bs.body)), bs.body, partial, fileIndex)
			if m && depth < maxGlobstarRecursion {
				if sub := bodySections(fileIndex+len(bs.body), bodyIndex+1, depth+1, sawTail); sub != triFalse {
					return sub
				}
			}
			if fileIndex < len(file) && isDotOrDotDot(file[fileIndex]) {
				return triFalse
			}
		}
		if partial {
			return triTrue
		}
		return triNull
	}
	return bodySections(fileIndex, 0, 0, fileTailMatch > 0) == triTrue
}
