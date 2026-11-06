// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package downloadartifact

import (
	"archive/tar"
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/internal/pkg/action"

	"github.com/bmatcuk/doublestar/v4"
)

func fetchOnce(ctx context.Context, c *action.Context, src *source, signed, target string, skipDecompress bool) (string, error) {
	resp, err := src.download(ctx, signed, 30*time.Second)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	file, err := os.CreateTemp("", "artifact-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), resp.Body)
	if err != nil {
		return "", err
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if isZip(resp, signed) && !skipDecompress {
		return digest, extractZip(ctx, c, file, size, target)
	}
	fileName := rawFileName(resp.Header.Get("Content-Disposition"))
	if fileName == ".." || strings.ContainsAny(fileName, `/\`) || c.Windows() && !filepath.IsLocal(fileName) {
		return "", action.Failf("Artifact filename is not valid: %s", fileName)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return digest, err
	}
	return digest, c.Container.CopyTarStream(ctx, target, func(tw *container.TarWriter) error {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: fileName, Mode: 0o644, Size: size, ModTime: time.Now()}); err != nil {
			return err
		}
		_, err := io.Copy(tw, file)
		return err
	})
}

var zipTypes = []string{"application/zip", "application/x-zip-compressed", "application/zip-compressed"}

func isZip(resp *http.Response, signed string) bool {
	mediaType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	signedURL, _ := url.Parse(signed)
	return slices.Contains(zipTypes, mediaType) || signedURL != nil && strings.HasSuffix(strings.ToLower(signedURL.Path), ".zip")
}

func rawFileName(disposition string) string {
	_, params, err := mime.ParseMediaType(disposition)
	name := params["filename"] // filename* arrives decoded in here
	if err != nil || name == "" {
		return "artifact"
	}
	if unescaped, err := url.PathUnescape(name); err == nil && !strings.Contains(strings.ToLower(disposition), "filename*") {
		name = unescaped
	}
	if name = path.Base(name); name == "/" || name == "." {
		return "artifact"
	}
	return name
}

func extractZip(ctx context.Context, c *action.Context, file io.ReaderAt, size int64, target string) error {
	reader, err := zip.NewReader(file, size)
	if err != nil {
		return err
	}
	now := time.Now()
	return c.Container.CopyTarStream(ctx, target, func(tw *container.TarWriter) error {
		for _, entry := range reader.File {
			name := path.Clean(strings.ReplaceAll(entry.Name, `\`, "/"))
			if name == "." {
				continue
			}
			if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || c.Windows() && !filepath.IsLocal(name) {
				return action.Failf("Artifact entry path is not valid: %s", entry.Name)
			}
			header := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(entry.UncompressedSize64), ModTime: now}
			if entry.FileInfo().IsDir() {
				header.Typeflag, header.Name, header.Mode, header.Size = tar.TypeDir, name+"/", 0o755, 0
			}
			if err := tw.WriteHeader(header); err != nil {
				return err
			}
			body, err := entry.Open()
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, body)
			if err := errors.Join(err, body.Close()); err != nil {
				return err
			}
		}
		return nil
	})
}

func matchName(pattern, name string) bool {
	if strings.HasPrefix(pattern, "#") {
		return false
	}
	negate := false
	for strings.HasPrefix(pattern, "!") {
		negate, pattern = !negate, pattern[1:]
	}
	matched, err := doublestar.Match(pattern, name)
	if err != nil {
		matched = pattern == name
	}
	if strings.HasPrefix(name, ".") && !strings.HasPrefix(pattern, ".") {
		matched = false
	}
	return matched != negate
}
