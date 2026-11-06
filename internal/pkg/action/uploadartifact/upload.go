// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package uploadartifact works like actions/upload-artifact v7.
package uploadartifact

import (
	"archive/zip"
	"cmp"
	"compress/flate"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gitea.com/gitea/runner/internal/pkg/action"
	"gitea.com/gitea/runner/internal/pkg/action/artifact"
	"gitea.com/gitea/runner/internal/pkg/action/glob"
)

type inputs struct {
	name, path, ifNoFilesFound          string
	retentionDays, compressionLevel     int
	overwrite, includeHidden, archiveIt bool
}

func Main(ctx context.Context, c *action.Context) error {
	c.ApplyDefaults("name", "artifact", "path", "", "if-no-files-found", "warn", "retention-days", "", "compression-level", "6",
		"overwrite", "false", "include-hidden-files", "false", "archive", "true")
	in, err := readInputs(c)
	if err != nil {
		return err
	}
	g, err := glob.Parse(in.path, glob.Options{
		Workdir:             c.Workspace,
		Home:                func() (string, error) { return c.Home(ctx) },
		Windows:             c.Windows(),
		ImplicitDescendants: true,
	})
	if err != nil {
		return err
	}
	files, err := selectFiles(ctx, c, g, in.includeHidden)
	if err != nil {
		return err
	}
	collected := &collected{files: files, root: glob.LeastCommonAncestor(g.SearchPaths(), c.Windows())}
	if len(files) == 1 && files[0].path == collected.root {
		collected.root = c.Dir(collected.root)
	}
	if len(collected.files) == 0 {
		message := fmt.Sprintf("No files were found with the provided path: %s. No artifacts will be uploaded.", in.path)
		switch in.ifNoFilesFound {
		case "warn":
			c.Warning("%s", message)
		case "error":
			return action.Failf("%s", message)
		default:
			c.Infof("%s", message)
		}
		return nil
	}
	if !in.archiveIt && len(collected.files) > 1 {
		return action.Failf("When 'archive' is set to false, only a single file can be uploaded. Found %d files to upload.", len(collected.files))
	}
	return upload(ctx, c, in, collected)
}

func readInputs(c *action.Context) (*inputs, error) {
	in := &inputs{name: c.Input("name"), path: c.Input("path"), ifNoFilesFound: c.Input("if-no-files-found"), compressionLevel: 6}
	if in.path == "" {
		return nil, action.Failf("Input required and not supplied: path")
	}
	var err error
	if in.overwrite, err = c.BoolInput("overwrite"); err != nil {
		return nil, err
	}
	if in.includeHidden, err = c.BoolInput("include-hidden-files"); err != nil {
		return nil, err
	}
	if in.archiveIt, err = c.BoolInput("archive"); err != nil {
		return nil, err
	}
	if in.ifNoFilesFound != "warn" && in.ifNoFilesFound != "error" && in.ifNoFilesFound != "ignore" {
		return nil, action.Failf("Unrecognized if-no-files-found input. Provided: %s. Available options: warn,error,ignore", in.ifNoFilesFound)
	}
	if days := c.Input("retention-days"); days != "" {
		if in.retentionDays, err = action.ParseInt(days); err != nil {
			return nil, action.Failf("Invalid retention-days")
		}
	}
	if level := c.Input("compression-level"); level != "" {
		if in.compressionLevel, err = action.ParseInt(level); err != nil {
			return nil, action.Failf("Invalid compression-level")
		}
		if in.compressionLevel < 0 || in.compressionLevel > 9 {
			return nil, action.Failf("Invalid compression-level. Valid values are 0-9")
		}
	}
	return in, nil
}

type collected struct {
	files []found
	root  string
}

func (got *collected) write(ctx context.Context, c *action.Context, in *inputs, output io.Writer) error {
	if !in.archiveIt {
		return readFiles(ctx, c, got.files, func(file found) error {
			_, err := io.Copy(output, file.body)
			return err
		})
	}
	deflater, err := flate.NewWriter(nil, in.compressionLevel)
	if err != nil {
		return err
	}
	writer := zip.NewWriter(output)
	writer.RegisterCompressor(zip.Deflate, func(output io.Writer) (io.WriteCloser, error) {
		deflater.Reset(output)
		return deflater, nil
	})
	if err := readFiles(ctx, c, got.files, func(file found) error {
		header := &zip.FileHeader{
			Name:   strings.TrimLeft(strings.ReplaceAll(strings.TrimPrefix(file.path, got.root), `\`, "/"), "/"),
			Method: zip.Deflate, Modified: file.mtime.UTC(),
		}
		if in.compressionLevel == 0 {
			header.Method = zip.Store
		}
		header.SetMode(file.mode & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky))
		entry, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		_, err = io.Copy(entry, file.body)
		return err
	}); err != nil {
		return err
	}
	return writer.Close()
}

const invalidPathChars = "\":<>|*?\r\n"

func upload(ctx context.Context, c *action.Context, in *inputs, got *collected) error {
	client, err := artifact.New(c.ResultsURL, c.RuntimeToken, c.Transport)
	if err != nil {
		return err
	}
	client.Logf = c.Infof
	if in.overwrite {
		if id, err := client.Delete(ctx, in.name); err == nil {
			c.Infof("Artifact '%s' (ID: %d) deleted", in.name, id)
		} else if !artifact.IsNotFound(err) {
			c.Infof("Unable to delete artifact: %v", err) // best effort, as the JavaScript action
		}
	}
	name, fileName, mimeType := in.name, in.name+".zip", ""
	if !in.archiveIt {
		name, fileName, mimeType = c.Base(got.files[0].path), c.Base(got.files[0].path), mimeTypeOf(got.files[0].path)
	}
	if name == "" {
		return action.Failf("Provided artifact name input during validation is empty")
	}
	if strings.ContainsAny(name, invalidPathChars+`\/`) {
		return action.Failf("The artifact name is not valid: %s", name)
	}
	if in.archiveIt {
		for _, file := range got.files {
			if rel := strings.TrimPrefix(file.path, got.root); strings.ContainsAny(rel, invalidPathChars) {
				return action.Failf("The path for one of the files in artifact is not valid: %s", rel)
			}
		}
	}
	signed, err := client.Create(ctx, name, mimeType, expiresAt(c, in.retentionDays))
	if err != nil {
		return err
	}
	concurrency, err := uploadConcurrency(c)
	if err != nil {
		return err
	}
	stall, err := uploadTimeout(c)
	if err != nil {
		return err
	}
	c.Infof("Uploading artifact: %s", fileName)
	reader, writer := io.Pipe()
	defer reader.Close()
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		writer.CloseWithError(got.write(streamCtx, c, in, writer))
	}()
	uploaded, err := client.Upload(ctx, signed, cmp.Or(mimeType, "application/zip"), reader, concurrency, stall)
	reader.CloseWithError(err)
	cancel()
	<-done
	if err != nil {
		return err
	}
	id, err := client.Finalize(ctx, name, uploaded.Size, uploaded.Digest)
	if err != nil {
		return err
	}
	c.Infof("Artifact %s has been successfully uploaded! Final size is %d bytes. Artifact ID is %d", in.name, uploaded.Size, id)
	ghc := c.Github
	link := strings.TrimSuffix(ghc.ServerURL, "/") + "/" + ghc.Repository + "/actions/runs/" + ghc.RunID + "/artifacts/" + url.PathEscape(name)
	if ghc.RunAttempt != "" {
		link += "?attempt=" + url.QueryEscape(ghc.RunAttempt)
	}
	c.Infof("Artifact download URL: %s", link)
	return c.SetOutputs(ctx, "artifact-id", strconv.FormatInt(id, 10), "artifact-digest", strings.TrimPrefix(uploaded.Digest, "sha256:"), "artifact-url", link)
}

func expiresAt(c *action.Context, days int) *time.Time {
	if days == 0 {
		return nil
	}
	if limit, err := action.ParseInt(c.Github.RetentionDays); err == nil && limit != 0 && limit < days {
		c.Warning("Retention days cannot be greater than the maximum allowed retention set within the repository. Using %d instead.", limit)
		days = limit
	}
	expires := time.Now().AddDate(0, 0, days)
	return &expires
}

func uploadConcurrency(c *action.Context) (int, error) {
	override := c.Env["ACTIONS_ARTIFACT_UPLOAD_CONCURRENCY"]
	if override == "" {
		return 5, nil
	}
	concurrency, err := action.ParseInt(override)
	if err != nil || concurrency < 1 {
		return 0, action.Failf("Invalid value set for ACTIONS_ARTIFACT_UPLOAD_CONCURRENCY env variable")
	}
	return concurrency, nil
}

func uploadTimeout(c *action.Context) (time.Duration, error) {
	value := c.Env["ACTIONS_ARTIFACT_UPLOAD_TIMEOUT_MS"]
	if value == "" {
		return 5 * time.Minute, nil
	}
	ms, err := action.ParseInt(value)
	if err != nil {
		return 0, action.Failf("Invalid value set for ACTIONS_ARTIFACT_UPLOAD_TIMEOUT_MS env variable")
	}
	return time.Duration(ms) * time.Millisecond, nil
}
