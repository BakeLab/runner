// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/lookpath"
)

// The action still resolves its own version and architecture against this cache.
const cacheSystemPython = `
import fcntl, os, platform, shutil, struct, sys, sysconfig, venv
from pathlib import Path

if sys.implementation.name != "cpython" or sys.version_info < (3, 14) or sys.version_info.releaselevel != "final":
    sys.exit(0)
arch = {"x86_64": "x64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine().lower())
if not arch or struct.calcsize("P") != 8:
    sys.exit(0)
if sysconfig.get_config_var("Py_GIL_DISABLED"):
    arch += "-freethreaded"
version = platform.python_version()
root = Path(sys.argv[1]) / "Python"
root.mkdir(parents=True, exist_ok=True)
with (root / ".system-python.lock").open("a") as lock:
    fcntl.flock(lock, fcntl.LOCK_EX)
    dest = root / version / arch
    marker = dest.with_name(arch + ".complete")
    if marker.exists() or dest.exists():
        sys.exit(0)
    try:
        venv.EnvBuilder(with_pip=True, symlinks=True).create(dest)
        marker.touch()
    except BaseException:
        shutil.rmtree(dest, ignore_errors=True)
        raise
    print("Cached system Python " + version + " (" + arch + ") for actions/setup-python")
`

func (rc *RunContext) prepareSetupPython(ctx context.Context, remote *remoteAction, env map[string]string) {
	if rc.Config.NoActionPatch || !rc.IsHostEnv() || runtime.GOOS != "linux" || remote == nil ||
		remote.Org != "actions" || remote.Repo != "setup-python" || remote.Path != "" || common.Dryrun(ctx) {
		return
	}
	if err := cacheHostPython(ctx, env); err != nil {
		common.Logger(ctx).Warnf("Cannot cache system Python for actions/setup-python: %v", err)
	}
}

func cacheHostPython(ctx context.Context, env map[string]string) error {
	if env["RUNNER_TOOL_CACHE"] == "" {
		return nil
	}
	python, err := lookpath.LookPath2("python3", env)
	if err != nil {
		return nil // setup-python can still install a downloadable interpreter
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "-c", cacheSystemPython, env["RUNNER_TOOL_CACHE"])
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	if message := strings.TrimSpace(string(output)); message != "" {
		common.Logger(ctx).Infof("%s", message)
	}
	return nil
}
