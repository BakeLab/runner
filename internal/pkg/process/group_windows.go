// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package process

import (
	"errors"
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

// Group holds tracked step processes in a job-scoped Windows Job Object.
type Group struct {
	mu  sync.Mutex
	job windows.Handle
}

// NewGroup creates the job object. Closing it is what terminates the leftovers,
// so the caller must Close it when the job ends.
func NewGroup() (*Group, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	return &Group{job: job}, nil
}

// Assign adds a started process to the job; anything it spawns afterwards joins
// too. Call before NewKiller so the step's job nests inside this one. Nil-safe.
func (g *Group) Assign(p *os.Process) error {
	if g == nil || p == nil {
		return nil
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job == 0 {
		return nil
	}

	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(h) }()

	return windows.AssignProcessToJobObject(g.job, h)
}

// Close terminates every process still assigned to the job and drops its handle, nil-safe and idempotent.
func (g *Group) Close() error {
	if g == nil {
		return nil
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job == 0 {
		return nil
	}

	h := g.job
	g.job = 0
	return errors.Join(windows.TerminateJobObject(h, 1), windows.CloseHandle(h))
}
