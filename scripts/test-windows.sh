#!/usr/bin/env bash
set -euo pipefail

image="${WINDOWS_TEST_IMAGE:?}"
timeout="${WINDOWS_TEST_TIMEOUT:-165}"
name="gitea-runner-windows-test-$$"
work="$(mktemp -d)"

cleanup() {
  docker rm -fv "$name" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

if [ "$(docker info -f '{{.Architecture}}')" != x86_64 ]; then
  echo "error: the Windows test VM needs an amd64 docker host" >&2
  exit 1
fi

echo "==> Building Windows test binaries"
oem="$work/oem"
mkdir -p "$oem/bin" "$oem/src" "$work/shared"
module="$(go list -m)"
packages="$(GOOS=windows go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./...)"
GOOS=windows GOARCH=amd64 go test -c -ldflags='-s -w' -o "$oem/bin/" $packages
for package in $packages; do
  echo "${package##*/}.test.exe ${package#"$module"/}"
done > "$oem/tests.txt"
git ls-files -z --cached --others --exclude-standard | while IFS= read -r -d '' file; do
  if [ -e "$file" ]; then printf '%s\0' "$file"; fi
done | tar --null -T - -cf - | tar -xf - -C "$oem/src"
git -C "$oem/src" init -q -b main # a repository like the CI checkout, for the tests that read the github context from it
git -C "$oem/src" add -Af .
git -C "$oem/src" -c user.name=test -c user.email=test@localhost -c commit.gpgsign=false -c core.hooksPath=/dev/null commit -qm test
git -C "$oem/src" remote add origin https://gitea.com/gitea/runner
cp scripts/test-windows-setup.ps1 scripts/test-windows.ps1 "$oem/"
sed 's/$/\r/' > "$oem/install.bat" <<'EOF' # a policy turns off real-time scanning, AMSI blocks scripts that do it
@echo off
set log=\\host.lan\Data\install.log
echo ==^> install.bat started at %TIME% >> %log%
reg.exe add "HKLM\SOFTWARE\Policies\Microsoft\Windows Defender\Real-Time Protection" /v DisableRealtimeMonitoring /t REG_DWORD /d 1 /f > nul
powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\OEM\test-windows-setup.ps1 >> %log% 2>&1
echo ==^> setup exited with %ERRORLEVEL% >> %log%
if %ERRORLEVEL% equ 0 C:\pwsh\pwsh.exe -NoProfile -ExecutionPolicy Bypass -File C:\OEM\test-windows.ps1 >> %log% 2>&1
if not exist \\host.lan\Data\exitcode (echo 1)> \\host.lan\Data\exitcode
shutdown.exe /s /t 600
EOF

kvm=Y cores="$(nproc)"
if ! docker run --rm --privileged --entrypoint sh "$image" -c 'test -w /dev/kvm'; then
  kvm=N cores=4 # more emulated vCPUs slow Windows down
  echo "==> The docker host has no usable /dev/kvm, Windows runs emulated and much slower"
fi

echo "==> Installing Windows"
docker create --rm --name "$name" --privileged -e VERSION=2025 -e EDITION=core -e KVM="$kvm" -e CPU_CORES="$cores" -e RAM_SIZE=8G \
  -e DISK_SIZE=32G -e DISK_CACHE=unsafe -e DISK_IO=threads --entrypoint timeout "$image" "$((timeout + 10))m" /usr/bin/tini -s /run/entry.sh >/dev/null # the timeout stops the VM even when this script is killed before its cleanup runs
docker cp "$oem" "$name:/oem"
docker cp "$work/shared" "$name:/shared"
docker start "$name" >/dev/null
docker logs -f "$name" &
while sleep 10 && docker container inspect "$name" >/dev/null 2>&1; do
  if ((SECONDS % 300 < 10)); then
    echo "==> $((SECONDS / 60))m elapsed, VM disk $(docker exec "$name" du -sh /storage | cut -f1)"
  fi
done &
docker exec "$name" sh -c 'cd /shared && touch install.log summary.txt && tail -qf install.log summary.txt' &

if ! timeout "${timeout}m" docker exec "$name" sh -c 'until [ -f /shared/exitcode ]; do sleep 10; done'; then
  echo "Windows stopped or did not finish the tests within $timeout minutes" >&2
  exit 1
fi
results="$work/results"
docker cp "$name:/shared" "$results"

tr -d '\r' < "$results/summary.txt" | while read -r result package _; do
  if [ "$result" = FAIL ]; then
    echo "::group::$package"
    cat "$results/${package//\//-}.log"
    echo "::endgroup::"
  fi
done
if [ -f "$results/error.txt" ]; then
  cat "$results/error.txt" >&2
fi
echo "==> Slowest tests"
find "$results" -name '*.log' -exec cat {} + | tr -d '\r' | sed -nE 's/^--- (PASS|FAIL): (\S+) \(([0-9.]+)s\)$/\3 \2/p' | sort -rn | awk 'NR <= 20'
echo "==> Packages"
tr -d '\r' < "$results/summary.txt"
exit "$(tr -d '\r\n' < "$results/exitcode")"
