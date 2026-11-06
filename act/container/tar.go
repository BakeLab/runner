// Copyright 2026 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/filecollector"
)

// TarWriter takes directories and files named relative to the destination, owned by the job user.
type TarWriter struct {
	writer   *tar.Writer
	uid, gid int
	prefix   string
	dirs     map[string]bool
}

func (w *TarWriter) Write(data []byte) (int, error) {
	return w.writer.Write(data)
}

func (w *TarWriter) WriteHeader(hdr *tar.Header) error {
	name := strings.TrimSuffix(hdr.Name, "/")
	if !fs.ValidPath(name) {
		return fmt.Errorf("tar entry %q escapes the destination", hdr.Name)
	}
	if hdr.Typeflag != tar.TypeDir && hdr.Typeflag != tar.TypeReg {
		return fmt.Errorf("tar entry %q: unsupported type %q", hdr.Name, hdr.Typeflag)
	}
	for index := range len(name) {
		if dir := name[:index]; name[index] == '/' && !w.dirs[dir] {
			if err := w.write(&tar.Header{Typeflag: tar.TypeDir, Name: path.Join(w.prefix, dir), Mode: 0o755}); err != nil {
				return err
			}
			w.dirs[dir] = true
		}
	}
	if hdr.Typeflag == tar.TypeDir {
		w.dirs[name] = true
	}
	entry := *hdr
	entry.Name = path.Join(w.prefix, name)
	return w.write(&entry)
}

func (w *TarWriter) write(hdr *tar.Header) error {
	hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = w.uid, w.gid, "", "" // tar running as root prefers names over ids
	if hdr.Typeflag == tar.TypeDir {
		hdr.Name += "/"
	}
	return w.writer.WriteHeader(hdr)
}

func writeTarStream(dst io.Writer, uid, gid int, prefix string, parents []string, write func(*TarWriter) error) error {
	tw := &TarWriter{writer: tar.NewWriter(dst), uid: uid, gid: gid, prefix: prefix, dirs: map[string]bool{}}
	for _, name := range parents {
		if err := tw.write(&tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: 0o755}); err != nil {
			return err
		}
	}
	if err := write(tw); errors.Is(err, io.EOF) {
		return fmt.Errorf("tar stream: %v", err) // %v, as a wrapped io.EOF reads as the archive's end
	} else if err != nil {
		return err
	}
	return tw.writer.Close()
}

func pipeTar(uid, gid int, prefix string, parents []string, write func(*TarWriter) error) *io.PipeReader {
	reader, writer := io.Pipe()
	go func() { writer.CloseWithError(writeTarStream(writer, uid, gid, prefix, parents, write)) }()
	return reader
}

// startedArchive returns reader once its first byte arrives, so failing to start archiving errors here, not on read.
func startedArchive(reader *io.PipeReader) (io.ReadCloser, error) {
	buffered := bufio.NewReader(reader)
	if _, err := buffered.Peek(1); err != nil {
		reader.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{buffered, reader}, nil
}

func writeFilesTar(ctx context.Context, w io.Writer, uid, gid int, files ...*FileEntry) error {
	tw := tar.NewWriter(w)
	for _, file := range files {
		common.Logger(ctx).Debugf("Writing entry to tarball %s len:%d", file.Name, len(file.Body))
		if err := tw.WriteHeader(&tar.Header{
			Name: file.Name,
			Mode: file.Mode,
			Size: int64(len(file.Body)),
			Uid:  uid,
			Gid:  gid,
		}); err != nil {
			return err
		}
		if _, err := tw.Write([]byte(file.Body)); err != nil {
			return err
		}
	}
	return tw.Close()
}

// writeDirTar prefixes entries with dstPath for extraction at root.
func writeDirTar(ctx context.Context, w io.Writer, dstPath, srcPath string, useGitIgnore, skipGitDir bool, uid, gid int, executable bool) error {
	logger := common.Logger(ctx)
	tw := tar.NewWriter(w)
	srcPrefix := strings.TrimSuffix(filepath.Dir(srcPath), string(filepath.Separator)) + string(filepath.Separator)
	logger.Debugf("Stripping prefix:%s src:%s", srcPrefix, srcPath)

	if err := filepath.Walk(srcPath, (&filecollector.FileCollector{
		SrcPath:      srcPath,
		SrcPrefix:    srcPrefix,
		SkipGitDir:   skipGitDir,
		UseGitIgnore: useGitIgnore,
		Handler:      &filecollector.TarCollector{TarWriter: tw, UID: uid, GID: gid, DstDir: dstPath[1:], Executable: executable},
	}).CollectFiles(ctx)); err != nil {
		return err
	}
	return tw.Close()
}
