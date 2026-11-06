// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package uploadartifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/internal/pkg/action"
	"gitea.com/gitea/runner/internal/pkg/action/glob"
)

type found struct {
	path, source string
	mode         fs.FileMode
	mtime        time.Time
	body         io.Reader
}

type treeEntry struct {
	name, target string
	dir          bool
}

type tree struct {
	real    string
	entries map[string][]treeEntry
}

const listScript = `test -e "$1" || exit 1
[ -d "$1" ] || { printf 'f\0'; exit 0; }
cd "$1" || exit 2
shift
pwd -P; printf '\0'
find . "$@" -type d ! -name . -print0; printf '\0'
find . "$@" -type l -exec sh -c 'for l; do printf "%s\0" "$l"; readlink "$l"; printf "\0"; done' sh {} +; printf '\0'
find . "$@" ! -type d ! -type l -print0`

func list(ctx context.Context, c *action.Context, source string, includeHidden bool) (*tree, error) {
	if c.Host() {
		info, err := os.Stat(source)
		if err != nil {
			return nil, err
		}
		resolved, err := filepath.EvalSymlinks(source)
		if err != nil || !info.IsDir() {
			return &tree{real: resolved}, err
		}
		entries, err := os.ReadDir(resolved)
		result := &tree{real: resolved, entries: map[string][]treeEntry{".": nil}}
		for _, entry := range entries {
			result.entries["."] = append(result.entries["."], treeEntry{name: entry.Name(), dir: entry.IsDir()})
		}
		return result, err
	}
	command := []string{"sh", "-c", listScript, "sh", source}
	if !includeHidden {
		command = append(command, "!", "-name", ".", "-name", ".*", "-prune", "-o")
	}
	output, err := c.Container.ExecOutput(ctx, command)
	if err != nil {
		return nil, err
	}
	defer output.Close()
	data, err := io.ReadAll(output)
	if err != nil {
		return nil, err
	}
	if err := output.Close(); err != nil {
		return nil, err
	}
	if string(data) == "f\x00" {
		return &tree{real: source}, nil
	}
	fields := strings.Split(string(data), "\x00")
	if len(fields) < 4 {
		return nil, fmt.Errorf("invalid directory listing for %s", source)
	}
	result := &tree{real: strings.TrimSuffix(fields[0], "\n"), entries: map[string][]treeEntry{".": nil}}
	section := 0
	for index := 1; index < len(fields)-1; index++ {
		name := fields[index]
		if name == "" {
			section++
			continue
		}
		entry := treeEntry{name: path.Base(name), dir: section == 0}
		switch section {
		case 0:
			result.entries[path.Clean(name)] = nil
		case 1:
			index++
			if index >= len(fields)-1 {
				return nil, fmt.Errorf("invalid symlink listing for %s", source)
			}
			entry.target = strings.TrimSuffix(fields[index], "\n")
		}
		result.entries[path.Dir(name)] = append(result.entries[path.Dir(name)], entry)
	}
	for _, entries := range result.entries {
		slices.SortFunc(entries, func(left, right treeEntry) int { return strings.Compare(left.name, right.name) })
	}
	return result, nil
}

func selectFiles(ctx context.Context, c *action.Context, globber *glob.Globber, includeHidden bool) ([]found, error) {
	var files []found
	join := path.Join
	if c.Windows() {
		join = filepath.Join
	}
	var walk func(string, string, []string, *tree, string) error
	walk = func(virtual, source string, chain []string, listed *tree, relative string) error {
		match := globber.Match(virtual)
		if match == glob.MatchNone && !globber.PartialMatch(virtual) || !includeHidden && strings.HasPrefix(c.Base(virtual), ".") {
			return nil
		}
		if listed == nil {
			var err error
			listed, err = list(ctx, c, source, includeHidden)
			var exit container.ExitCodeError
			if errors.Is(err, fs.ErrNotExist) || errors.As(err, &exit) && exit == 1 {
				return nil
			} else if err != nil {
				return err
			}
			relative = "."
		}
		if listed.entries == nil {
			if match&glob.MatchFile != 0 {
				files = append(files, found{path: virtual, source: listed.real})
			}
			return nil
		}
		resolved := join(listed.real, relative)
		if slices.Contains(chain, resolved) {
			return nil
		}
		chain = append(slices.Clip(chain), resolved)
		for _, entry := range listed.entries[relative] {
			child := join(virtual, entry.name)
			source := join(resolved, entry.name)
			var subtree *tree
			switch {
			case entry.target != "":
				source = entry.target
				if !path.IsAbs(source) {
					source = path.Join(resolved, source)
				}
			case entry.dir && !c.Host():
				subtree = listed
			case !entry.dir && !c.Host():
				subtree = &tree{real: source}
			}
			if err := walk(child, source, chain, subtree, path.Join(relative, entry.name)); err != nil {
				return err
			}
		}
		return nil
	}
	for _, root := range globber.SearchPaths() {
		if err := walk(root, root, nil, nil, "."); err != nil {
			return nil, err
		}
	}
	return files, nil
}
