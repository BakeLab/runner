// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package glob

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	posix   = Options{Workdir: "/work/cwd-with-[and]", Home: home("/home/home-with-[and]")}
	windows = Options{Workdir: `C:\work\cwd-with-[and]`, Home: home(`C:\Users\home-with-[and]`), Windows: true}
)

func home(dir string) func() (string, error) {
	return func() (string, error) { return dir, nil }
}

func implicit(o Options) Options {
	o.ImplicitDescendants = true
	return o
}

func TestHomeResolvedLazilyOnce(t *testing.T) {
	calls := 0
	o := Options{Workdir: "/w", Home: func() (string, error) { calls++; return "/h", nil }}
	_, err := Parse("~/a\n! ~/b", o)
	require.NoError(t, err)
	_, err = Parse("/a", o)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestMatch(t *testing.T) {
	tests := []struct {
		name     string
		patterns string
		opts     Options
		want     map[string]MatchKind
	}{
		{"negate markers toggle", "/a/*.txt\n!!/a/two.txt\n!/a/one.txt\n!!!/a/three.txt", posix, map[string]MatchKind{
			"/a/two.txt": MatchAll, "/a/one.txt": MatchNone, "/a/three.txt": MatchNone, "/a/x.txt": MatchAll,
		}},
		{"home is escaped", "~/m*", posix, map[string]MatchKind{
			"/home/home-with-[and]/match": MatchAll, "/home/home-with-[and]/not-match": MatchNone, "/home/home-with-a/match": MatchNone,
		}},
		{"workdir is escaped", "m*", posix, map[string]MatchKind{
			"/work/cwd-with-[and]/match": MatchAll, "/work/cwd-with-[and]/not-match": MatchNone,
		}},
		{"workdir is escaped on windows", "m*\nC:n*\n" + `\work\cwd-with-[[]and]\o*`, windows, map[string]MatchKind{
			`C:\work\cwd-with-[and]\match`: MatchAll, `C:\work\cwd-with-[and]\nope`: MatchAll, `C:\work\cwd-with-[and]\ok`: MatchAll, `C:\work\cwd-with-[and]\x`: MatchNone,
		}},
		{"globstar matches the directory before it", "/foo/bar/**", posix, map[string]MatchKind{
			"/": MatchNone, "/foo": MatchNone, "/foo/bar": MatchAll, "/foo/bar/baz": MatchAll,
		}},
		{"globstar matches root", "/**", posix, map[string]MatchKind{"/": MatchAll}},
		{"globstar matches root on windows", `C:\**`, windows, map[string]MatchKind{`C:\`: MatchAll}},
		{"case sensitive on posix", "/Foo/**/Baz", posix, map[string]MatchKind{
			"/Foo/Baz": MatchAll, "/Foo/bAZ": MatchNone, "/fOO/bar/Baz": MatchNone,
		}},
		{"case insensitive on windows", "C:/Foo/**/Baz", windows, map[string]MatchKind{
			`C:\Foo\Baz`: MatchAll, `c:\Foo\bAZ`: MatchAll, "C:/fOO/bar/bAZ": MatchAll, `C:\Foo\Bar`: MatchNone,
		}},
		{"leading dot segment", ".\n./foo\n.bar", posix, map[string]MatchKind{
			"/work/cwd-with-[and]": MatchAll, "/work/cwd-with-[and]/foo": MatchAll, "/work/cwd-with-[and]/.bar": MatchAll,
			"/work/cwd-with-[and]/bar": MatchNone, "/work/cwd-with-[and]bar": MatchNone,
		}},
		{"leading tilde segment", "~\n~/foo\n~bar", posix, map[string]MatchKind{
			"/home/home-with-[and]": MatchAll, "/home/home-with-[and]/foo": MatchAll, "/work/cwd-with-[and]/~bar": MatchAll,
			"/home/home-with-[and]/bar": MatchNone, "/home/home-with-[and]bar": MatchNone,
		}},
		{"windows relative roots", "C:\nC:foo\nD:\nD:bar\n" + `\work\cwd-with-[[]and]\baz`, windows, map[string]MatchKind{
			`C:\work\cwd-with-[and]`: MatchAll, `C:\work\cwd-with-[and]\foo`: MatchAll, `C:\foo`: MatchNone,
			`D:\`: MatchDirectory, `D:\bar`: MatchAll, `D:\foo`: MatchNone, `C:\work\cwd-with-[and]\baz`: MatchAll,
		}},
		{"windows unc", `\\srv\share\a\*`, windows, map[string]MatchKind{`\\srv\share\a\b`: MatchAll, `//SRV/share/a/b`: MatchAll, `\\srv\other\a\b`: MatchNone}},
		{"trailing separator matches directories only", "/foo/**/", posix, map[string]MatchKind{
			"/": MatchNone, "/foo/": MatchDirectory, "/foo/bar": MatchDirectory, "/foo/bar/baz": MatchDirectory,
		}},
		{"backslash is a separator on windows", `C:\foo\`, windows, map[string]MatchKind{`C:\foo`: MatchDirectory}},
		{"backslash escapes on posix", `/foo\*/b\[a]r`, posix, map[string]MatchKind{"/foo*/b[a]r": MatchAll, "/fooo/bar": MatchNone}},
		{"interleaved excludes", "/**/*.proj\n/**/README.txt\n!/**/solution2/**\n/**/*.sln\n!/**/proj2/README.txt", posix, map[string]MatchKind{
			"/solution1/proj1/proj1.proj": MatchAll, "/solution1/proj1/README.txt": MatchAll, "/solution1/proj2/README.txt": MatchNone,
			"/solution1/solution1.sln": MatchAll, "/solution2/proj1/proj1.proj": MatchNone, "/solution2/solution2.sln": MatchAll,
		}},
		{"exclude directories", "C:/foo/**\n!C:/foo/**/", windows, map[string]MatchKind{
			`C:\`: MatchNone, `C:\foo`: MatchFile, `C:\foo\bar`: MatchFile,
		}},
		{"dot is on, brace and extglob are off", "/a/*\n/b/{x,y}\n/c/+(x)\n/d/#e", posix, map[string]MatchKind{
			"/a/.hidden": MatchAll, "/a/.": MatchNone, "/b/{x,y}": MatchAll, "/b/x": MatchNone, "/c/+(x)": MatchAll, "/c/x": MatchNone, "/d/#e": MatchAll,
		}},
		{"character classes", "/a/[!x]\n/b/[a-c]z\n/c/[z-a]\n/d/[]]", posix, map[string]MatchKind{
			"/a/y": MatchAll, "/a/x": MatchNone, "/b/bz": MatchAll, "/b/dz": MatchNone, "/c/[z-a]": MatchAll, "/c/z": MatchNone, "/d/]": MatchAll,
		}},
		{"unclosed class splices undefined like minimatch 3", "C:/foo/b[\\!]r/b*", windows, map[string]MatchKind{
			"C:/foo/b[undefined/!]r/baz": MatchAll,
		}},
		{"implicit descendants", "/foo\n/bar/**\n/dir/\n!/foo/skip", implicit(posix), map[string]MatchKind{
			"/foo": MatchAll, "/foo/a/b": MatchAll, "/foo/skip": MatchNone, "/foo/skip/x": MatchNone,
			"/bar": MatchAll, "/dir": MatchDirectory, "/dir/file": MatchAll,
		}},
		{"no implicit descendants", "/foo", posix, map[string]MatchKind{"/foo": MatchAll, "/foo/a": MatchNone}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, err := Parse(tt.patterns, tt.opts)
			require.NoError(t, err)
			for p, want := range tt.want {
				assert.Equal(t, want, g.Match(p), p)
			}
		})
	}
}

func TestPartialMatch(t *testing.T) {
	tests := []struct {
		name     string
		patterns string
		opts     Options
		want     map[string]bool
	}{
		{"skips excludes", "/search1/foo/**\n/search2/bar/**\n!/search2/bar/**\n!/search3/baz/**", posix, map[string]bool{
			"/search1": true, "/search1/foo": true, "/search2": true, "/search2/bar": true, "/search3": false, "/search3/bar": false,
		}},
		{"root", "/foo/**", posix, map[string]bool{"/": true}},
		{"root on windows", "C:\\foo\\**\nd:\\foo\\**", windows, map[string]bool{`c:\`: true, `D:\`: true, `E:\`: false}},
		{"case", "/Foo/Bar/**/Baz", posix, map[string]bool{"/Foo": true, "/fOO": false}},
		{"case on windows", "C:/Foo/Bar/**/Baz", windows, map[string]bool{`C:\Foo`: true, `C:\fOO`: true, `C:\Other`: false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, err := Parse(tt.patterns, tt.opts)
			require.NoError(t, err)
			for p, want := range tt.want {
				assert.Equal(t, want, g.PartialMatch(p), p)
			}
		})
	}
}

func TestSearchPaths(t *testing.T) {
	tests := []struct {
		name     string
		patterns string
		opts     Options
		want     []string
	}{
		{"omits excludes", "/search1/foo/**\n/search2/bar/**\n!/search3/baz/**", posix, []string{"/search1/foo", "/search2/bar"}},
		{
			"omits descendants", "/search1/foo/**\n/search1/foo/bar/**\n/search2/foo/bar\n/search2/**\n/search3/foo/bar/**\n/search3/foo/bar/**", posix,
			[]string{"/search1/foo", "/search2", "/search3/foo/bar"},
		},
		{
			"omits descendants on windows", "C:\\Search1\\Foo\\**\nC:\\sEARCH1\\fOO\\bar\\**\nC:\\sEARCH1\\foo\\bar\nC:\\Search2\\**\nC:\\Search3\\Foo\\Bar\\**\nC:\\sEARCH3\\fOO\\bAR\\**", windows,
			[]string{`C:\Search1\Foo`, `C:\Search2`, `C:\Search3\Foo\Bar`},
		},
		{"skips comments and blank lines", "# /nope\n\n  /a/*  \n", posix, []string{"/a"}},
		{"splits carriage returns on windows", "C:/a/*\r\nC:/b/*\rC:/c/*", windows, []string{`C:\a`, `C:\b`, `C:\c`}},
		{
			"unescapes literal segments", "/foo/b[a]r/b*\n/x/b[*]r/b*\n/y/b[!]r/b*\n/z/b[]r/b*\n/q/b[aA]r/b*\n/e/b\\[a]r/b*\n/f/b[\\]]r/b*", posix,
			[]string{"/foo/bar", "/x/b*r", "/y/b!r", "/z/b[]r", "/q", "/e/b[a]r", "/f/b]r"},
		},
		{"unescapes literal segments on windows", "C:/foo/b\\[a]r/b*\nC:/x/b[?]r/b*", windows, []string{`C:\foo\b\ar`, `C:\x\b?r`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, err := Parse(tt.patterns, tt.opts)
			require.NoError(t, err)
			assert.Equal(t, tt.want, g.SearchPaths())
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name     string
		patterns string
		opts     Options
	}{
		{"empty after negation", "!", posix},
		{"dot segment", "/a/./b", posix},
		{"dot dot segment", "a/../b", posix},
		{"glob in unc root", `\\srv\sh*re\a`, windows},
		{"tilde without home", "~/a", Options{Workdir: "/w"}},
		{"relative workdir", "a", Options{Workdir: "w"}},
		{"drive relative with unc workdir", "C:a", Options{Workdir: `\\srv\share`, Windows: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.patterns, tt.opts)
			assert.Error(t, err)
		})
	}
}

func TestLeastCommonAncestor(t *testing.T) {
	tests := []struct {
		paths   []string
		windows bool
		want    string
	}{
		{[]string{"/a/b/c", "/a/b/d", "/a/b"}, false, "/a/b"},
		{[]string{"/foo", "/bar"}, false, "/"},
		{[]string{"/a"}, false, "/a"},
		{[]string{`C:\a\b`, `C:\a\c`}, true, `C:\a`},
		{[]string{`C:\a`, `c:\a\b`}, true, "."},
		{[]string{`C:\a`, `C:\b`}, true, "C:."},
		{[]string{`\\srv\share\a`, `\\srv\share\b`}, true, `\srv\share`},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, LeastCommonAncestor(tt.paths, tt.windows), tt.paths)
	}
}
