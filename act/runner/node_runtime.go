// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/act/lookpath"

	"gitea.dev/actionslib/pkg/model"
)

type nodeRelease struct {
	version string
	hashes  map[string]string
}

var nodeReleases = map[model.ActionRunsUsing]nodeRelease{
	model.ActionRunsUsingNode12: {"12.22.12", map[string]string{
		"darwin-x64":  "32927913ed549ce01685a6f9f4697567a64592c7fd1e9a845ac8a10efa1475e6",
		"linux-x64":   "ff92a45c4d03e8e270bec1ab337b8fff6e9de293dabfe7e8936a41f2fb0b202e",
		"linux-arm64": "91aefa690914b7f24250f3c0b560b42c6d306315d40009c96b5a6940115895fe",
	}},
	model.ActionRunsUsingNode16: {"16.20.2", map[string]string{
		"darwin-arm64": "6a5c4108475871362d742b988566f3fe307f6a67ce14634eb3fbceb4f9eea88c",
		"darwin-x64":   "d7a46eaf2b57ffddeda16ece0d887feb2e31a91ad33f8774da553da0249dc4a6",
		"linux-x64":    "c9193e6c414891694759febe846f4f023bf48410a6924a8b1520c46565859665",
		"linux-arm64":  "b6945fcc9ad220386bb814bfae7137189fd17297f2959a744105e1bee006035a",
	}},
	model.ActionRunsUsingNode20: {"20.20.2", map[string]string{
		"darwin-arm64": "466e05f3477c20dfb723054dfebffe55bc74660ee77f612166fca121dacb65b6",
		"darwin-x64":   "8be6f5e4bb128c82774f8a0b8d7a1cc1365a7977d9657cece0ca647b3fe04e61",
		"linux-x64":    "19e56f0825510207dd904f087fe52faa0a4eb6b2aab5f0ea7a33830d04888b8b",
		"linux-arm64":  "47ef73d543ecf6eb19435f6c03a0ac4809b3bf0dd6b26c7c571efc2a6572a74d",
	}},
	model.ActionRunsUsingNode24: {"24.22.2", map[string]string{
		"darwin-arm64": "aa9549349e47dffaa42dcf95d6e06ade8c4bdafc980979883b88767670df385a",
		"darwin-x64":   "d5e336d588c45be0c37dbd52cc29895593009ca4997a77a1fd02815131ed0e52",
		"linux-x64":    "361718c444e49af2ca5a110c83ba6a8901143f23ab285ebbef0341f391f482a9",
		"linux-arm64":  "95c264150aceb7f3437a29fd6984924f5f8d1511ea3cbd6c508b83f6c555dae2",
	}},
}

const nodeDistURL = "https://nodejs.org/dist"

const nodeProbe = `PATH="$1"; export PATH; if command -v node >/dev/null 2>&1; then node -p 'process.versions.node.split(".")[0]'; else printf missing; fi`

func (rc *RunContext) execNodeAction(ctx context.Context, using model.ActionRunsUsing, script string, env map[string]string) error {
	rc.ApplyExtraPath(ctx, &env)
	executable, err := rc.nodeRuntime(ctx, using, env)
	if err != nil {
		return fmt.Errorf("prepare %s action runtime: %w", using, err)
	}
	args := nodeActionCommand(script)
	args[0] = executable
	if executable != "node" {
		env = maps.Clone(env)
		searchPath, ok := env["PATH"]
		if !ok {
			searchPath = rc.JobContainer.DefaultPathVariable()
		}
		env["PATH"] = path.Dir(executable) + ":" + searchPath
	}
	return rc.JobContainer.Exec(args, env, "", "")(ctx)
}

func (rc *RunContext) nodeRuntime(ctx context.Context, using model.ActionRunsUsing, env map[string]string) (string, error) {
	if common.Dryrun(ctx) {
		return "node", nil
	}
	if rc.IsHostEnv() {
		return rc.hostNodeRuntime(ctx, using, env, hostNodeSource{
			goos: runtime.GOOS, goarch: runtime.GOARCH, client: http.DefaultClient, baseURL: nodeDistURL, releases: nodeReleases,
		})
	}
	job := rc.topLevelRunContext()
	if executable := job.nodeRuntimes[using]; executable != "" {
		return executable, nil
	}
	searchPath, ok := env["PATH"]
	if !ok {
		searchPath = rc.JobContainer.DefaultPathVariable()
	}
	major, err := nodeOutput(ctx, rc.JobContainer, []string{"sh", "-c", nodeProbe, "node-probe", searchPath})
	if err != nil {
		return "", fmt.Errorf("probe container Node.js: %w", err)
	}
	if major == strings.TrimPrefix(string(using), "node") {
		return "node", nil
	}
	arch, err := nodeOutput(ctx, rc.JobContainer, []string{"uname", "-m"})
	if err != nil {
		return "", fmt.Errorf("detect container architecture: %w", err)
	}
	switch arch {
	case "x86_64":
		arch = "x64"
	case "aarch64":
		arch = "arm64"
	}
	platform := "linux-" + arch
	release, ok := nodeReleases[using]
	if !ok || release.hashes[platform] == "" {
		return "", fmt.Errorf("no managed %s runtime for Linux %s; install the declared Node.js major in the job image", using, arch)
	}
	common.Logger(ctx).Infof("Installing Node.js %s for %s actions", release.version, using)
	dest := path.Join(rc.JobContainer.GetActPath(), "node", string(using))
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := installNodeRuntime(ctx, rc.JobContainer, http.DefaultClient, nodeDistURL, release, platform, dest); err != nil {
		return "", err
	}
	executable := path.Join(dest, "node")
	version, err := nodeOutput(ctx, rc.JobContainer, []string{executable, "--version"})
	if err != nil {
		return "", fmt.Errorf("managed Node.js cannot run in the job image (Linux official binaries require glibc); install a compatible %s runtime: %w", using, err)
	}
	if version != "v"+release.version {
		return "", fmt.Errorf("managed Node.js returned version %q, expected v%s", version, release.version)
	}
	if job.nodeRuntimes == nil {
		job.nodeRuntimes = make(map[model.ActionRunsUsing]string)
	}
	job.nodeRuntimes[using] = executable
	return executable, nil
}

func nodeOutput(ctx context.Context, job container.ExecutionsEnvironment, args []string) (string, error) {
	output, err := job.ExecOutput(ctx, args)
	if err != nil {
		return "", err
	}
	defer output.Close()
	data, err := io.ReadAll(io.LimitReader(output, 4097))
	if err != nil {
		return "", err
	}
	if len(data) > 4096 {
		return "", errors.New("node probe output exceeds 4096 bytes")
	}
	return strings.TrimSpace(string(data)), nil
}

func installNodeRuntime(ctx context.Context, job container.ExecutionsEnvironment, client *http.Client, baseURL string, release nodeRelease, platform, dest string) error {
	name := "node-v" + release.version + "-" + platform
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v"+release.version+"/"+name+".tar.gz", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download Node.js: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download Node.js: HTTP %d", resp.StatusCode)
	}
	archive, err := os.CreateTemp("", "runner-node-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	hash := sha256.New()
	const maxArchive = 100 << 20
	size, err := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(resp.Body, maxArchive+1))
	if err != nil {
		return fmt.Errorf("download Node.js archive: %w", err)
	}
	if size > maxArchive {
		return errors.New("node archive exceeds 100 MiB")
	}
	if hex.EncodeToString(hash.Sum(nil)) != release.hashes[platform] {
		return errors.New("node archive SHA256 mismatch")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	compressed, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("node archive is missing bin/node")
		}
		if err != nil {
			return err
		}
		if header.Name != name+"/bin/node" {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > 150<<20 {
			return errors.New("node archive has an invalid bin/node")
		}
		return job.CopyTarStream(ctx, dest, func(writer *container.TarWriter) error {
			if err := writer.WriteHeader(&tar.Header{Name: "node", Mode: 0o755, Size: header.Size, Typeflag: tar.TypeReg}); err != nil {
				return err
			}
			_, err := io.CopyN(writer, reader, header.Size)
			return err
		})
	}
}

// hostNodeSource describes where host jobs get managed Node.js runtimes.
type hostNodeSource struct {
	goos, goarch string
	client       *http.Client
	baseURL      string
	releases     map[model.ActionRunsUsing]nodeRelease
}

// hostNodeRuntime keeps using node from PATH unless a pinned official build exists
// for this host and PATH lacks the declared major. Managed runtimes are cached under
// the action cache dir and shared by jobs and runners on the host.
func (rc *RunContext) hostNodeRuntime(ctx context.Context, using model.ActionRunsUsing, env map[string]string, source hostNodeSource) (string, error) {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[source.goarch]
	platform := source.goos + "-" + arch
	release, ok := source.releases[using]
	if (source.goos != "linux" && source.goos != "darwin") || arch == "" || !ok || release.hashes[platform] == "" {
		return "node", nil
	}
	job := rc.topLevelRunContext()
	if executable := job.nodeRuntimes[using]; executable != "" {
		return executable, nil
	}
	found, major := hostNodeMajor(ctx, env)
	if major == strings.TrimPrefix(string(using), "node") {
		return "node", nil
	}
	logger := common.Logger(ctx)
	if found == "" {
		logger.Infof("No node in PATH; using managed Node.js %s for %s actions", release.version, using)
	} else {
		logger.Infof("%s reports Node.js major %q; using managed Node.js %s for %s actions", found, major, release.version, using)
	}
	executable, err := ensureHostNodeRuntime(ctx, source.client, source.baseURL, filepath.Join(rc.ActionCacheDir(), "node", string(using)), release, platform)
	if err != nil {
		if found != "" {
			logger.Warnf("Cannot provision Node.js %s for %s actions, using %s: %v", release.version, using, found, err)
			return "node", nil
		}
		return "", err
	}
	if job.nodeRuntimes == nil {
		job.nodeRuntimes = make(map[model.ActionRunsUsing]string)
	}
	job.nodeRuntimes[using] = executable
	return executable, nil
}

// hostNodeMajor resolves node from the step PATH like host execution does and
// reports its major version, or an empty major when it cannot be run.
func hostNodeMajor(ctx context.Context, env map[string]string) (string, string) {
	found, err := lookpath.LookPath2("node", env)
	if err != nil {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, found, "-p", `process.versions.node.split(".")[0]`)
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	output, err := cmd.Output()
	if err != nil {
		common.Logger(ctx).Debugf("probe %s: %v", found, err)
		return found, ""
	}
	return found, strings.TrimSpace(string(output))
}

func hostNodeVersion(ctx context.Context, executable string) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for attempt := 1; ; attempt++ {
		cmd := exec.CommandContext(ctx, executable, "--version")
		cmd.Env = []string{}
		output, err := cmd.Output()
		// A process forked elsewhere in the runner while the binary was being
		// written can briefly hold it open for writing (golang/go#22315).
		if errors.Is(err, syscall.ETXTBSY) && attempt < 20 {
			select {
			case <-ctx.Done():
				return ""
			case <-time.After(50 * time.Millisecond):
				continue
			}
		}
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(output))
	}
}

// ensureHostNodeRuntime installs a verified runtime under root without locks: each
// installer stages into a private directory and renames it into place, so the
// versioned directory only ever appears complete and concurrent installers converge
// on whichever rename wins.
func ensureHostNodeRuntime(ctx context.Context, client *http.Client, baseURL, root string, release nodeRelease, platform string) (string, error) {
	want := "v" + release.version
	dir := filepath.Join(root, want+"-"+platform)
	executable := filepath.Join(dir, "node")
	if hostNodeVersion(ctx, executable) == want {
		return executable, nil
	}
	if _, err := os.Lstat(dir); err == nil {
		// Another installer may have just renamed a complete runtime into place.
		if hostNodeVersion(ctx, executable) == want {
			return executable, nil
		}
		// Replacing it could break jobs running it, so leave that to the operator.
		return "", fmt.Errorf("managed Node.js at %s is unusable; remove that directory to reinstall it", dir)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(root, ".install-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	common.Logger(ctx).Infof("Installing Node.js %s into %s", release.version, dir)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := installNodeRuntime(ctx, &container.HostEnvironment{}, client, baseURL, release, platform, staging); err != nil {
		return "", err
	}
	if version := hostNodeVersion(ctx, filepath.Join(staging, "node")); version != want {
		return "", fmt.Errorf("managed Node.js returned version %q, expected %s (Linux official binaries require glibc)", version, want)
	}
	if err := os.Chmod(staging, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(staging, dir); err != nil {
		if hostNodeVersion(ctx, executable) == want {
			return executable, nil
		}
		return "", fmt.Errorf("install Node.js runtime: %w", err)
	}
	return executable, nil
}
