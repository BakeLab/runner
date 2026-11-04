// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package process

import (
	"bytes"
	"encoding/binary"
	"errors"

	"golang.org/x/sys/unix"
)

func listPIDs() ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(procs))
	for _, proc := range procs {
		pids = append(pids, int(proc.Proc.P_pid))
	}
	return pids, nil
}

// environment skips kern.procargs2's argc, executable path and arguments to the NUL-separated environment.
func environment(pid int) ([]byte, error) {
	args, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	if len(args) < 4 {
		return nil, errors.New("short kern.procargs2")
	}
	_, rest, _ := bytes.Cut(args[4:], []byte{0})
	rest = bytes.TrimLeft(rest, "\x00")
	for range binary.NativeEndian.Uint32(args) {
		_, rest, _ = bytes.Cut(rest, []byte{0})
	}
	return rest, nil
}
