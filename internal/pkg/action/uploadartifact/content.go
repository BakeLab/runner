// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package uploadartifact

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"gitea.com/gitea/runner/internal/pkg/action"
)

const fetchScript = `cd / && if command -v gzip >/dev/null 2>&1; then exec tar -czhf - -- "$@"; fi; exec tar -chf - -- "$@"`

func readFiles(ctx context.Context, c *action.Context, files []found, visit func(found) error) error {
	if c.Host() {
		for _, file := range files {
			body, err := os.Open(file.source)
			if err != nil {
				return err
			}
			info, err := body.Stat()
			if err == nil {
				file.body, file.mode, file.mtime = body, info.Mode(), info.ModTime()
				err = visit(file)
			}
			body.Close()
			if err != nil {
				return err
			}
		}
		return nil
	}
	for len(files) > 0 {
		command := []string{"sh", "-c", fetchScript, "sh"}
		selected := map[string][]found{}
		size, count := 0, 0
		for _, file := range files {
			name := "./" + strings.TrimPrefix(file.source, "/")
			if size+len(name)+1 >= 32*1024 {
				if count == 0 {
					return fmt.Errorf("artifact path exceeds container command limit: %s", file.path)
				}
				break
			}
			count++
			if _, exists := selected[path.Clean(name)]; !exists {
				command = append(command, name)
				size += len(name) + 1
			}
			selected[path.Clean(name)] = append(selected[path.Clean(name)], file)
		}
		output, err := c.Container.ExecOutput(ctx, command)
		if err != nil {
			return err
		}
		retry, err := readArchive(output, selected, visit)
		closeErr := output.Close()
		if err != nil {
			return err
		} else if closeErr != nil {
			return closeErr
		}
		files = append(retry, files[count:]...)
	}
	return nil
}

func readArchive(output io.Reader, selected map[string][]found, visit func(found) error) ([]found, error) {
	buffered := bufio.NewReader(output)
	var content io.Reader = buffered
	if magic, _ := buffered.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		compressed, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, err
		}
		defer compressed.Close()
		compressed.Multistream(false) // bsdtar pads its gzip output with zeros to the block size
		content = compressed
	}
	reader := tar.NewReader(content)
	var retry []found
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		name := path.Clean(header.Name)
		files := selected[name]
		delete(selected, name)
		if header.Typeflag == tar.TypeLink {
			retry = append(retry, files...)
			continue
		}
		if len(files) == 0 {
			continue
		}
		retry = append(retry, files[1:]...)
		file := files[0]
		file.body, file.mode, file.mtime = reader, header.FileInfo().Mode(), header.ModTime
		if err := visit(file); err != nil {
			return nil, err
		}
	}
	_, err := io.Copy(io.Discard, content)
	if err == nil {
		_, err = io.Copy(io.Discard, buffered)
	}
	return retry, err
}
