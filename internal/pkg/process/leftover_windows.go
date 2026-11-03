// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func KillProcessesWithPaths(ctx context.Context, dirs []string, trackingID string) (int, error) {
	if len(dirs) == 0 || trackingID == "" {
		return 0, nil
	}

	pids, err := listProcessIDs()
	if err != nil {
		return 0, err
	}

	self := uint32(os.Getpid())
	var ownSession uint32
	if err := windows.ProcessIdToSessionId(self, &ownSession); err != nil {
		return 0, fmt.Errorf("look up own session: %w", err)
	}

	var killed int
	var errs []error
	for _, pid := range pids {
		if ctx.Err() != nil {
			break
		}
		if pid == self || pid == 0 || pid == 4 {
			continue
		}
		var session uint32
		if err := windows.ProcessIdToSessionId(pid, &session); err != nil || session != ownSession {
			continue
		}
		ok, err := terminateIfPathsMatch(pid, dirs, trackingID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ok {
			killed++
		}
	}

	return killed, errors.Join(errs...)
}

func terminateIfPathsMatch(pid uint32, dirs []string, trackingID string) (bool, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return false, nil
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	params, err := processParametersHandle(handle)
	if err != nil {
		return false, nil
	}
	matches := false
fields:
	for _, field := range []struct {
		offset  uintptr
		wow64   uintptr
		command bool
	}{
		{unsafe.Offsetof(windows.RTL_USER_PROCESS_PARAMETERS{}.CurrentDirectory), 0x24, false},
		{unsafe.Offsetof(windows.RTL_USER_PROCESS_PARAMETERS{}.ImagePathName), 0x38, false},
		{unsafe.Offsetof(windows.RTL_USER_PROCESS_PARAMETERS{}.CommandLine), 0x40, true},
	} {
		value, err := params.unicodeString(handle, field.offset, field.wow64)
		if err != nil {
			continue
		}
		for _, dir := range dirs {
			if !field.command && dirContainsPath(dir, value) || field.command && strings.Contains(strings.ToLower(value), strings.ToLower(strings.TrimRight(dir, `\/`)+`\`)) {
				matches = true
				break fields
			}
		}
	}
	if !matches {
		return false, nil
	}
	value, err := params.environmentVariable(handle, "RUNNER_TRACKING_ID")
	if err != nil || value != trackingID {
		return false, nil
	}
	if err := windows.TerminateProcess(handle, 1); err != nil {
		return false, fmt.Errorf("terminate pid %d: %w", pid, err)
	}
	return true, nil
}

// listProcessIDs returns the PIDs of every process in a Toolhelp snapshot.
func listProcessIDs() ([]uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(snap) }()

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))

	pids := make([]uint32, 0, 256)
	err = windows.Process32First(snap, &entry)
	for err == nil {
		pids = append(pids, entry.ProcessID)
		err = windows.Process32Next(snap, &entry)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return pids, err
	}
	return pids, nil
}

type remoteParameters struct {
	address uintptr
	wow64   bool
}

func processParametersHandle(handle windows.Handle) (remoteParameters, error) {
	var pbi windows.PROCESS_BASIC_INFORMATION
	var retLen uint32
	if err := windows.NtQueryInformationProcess(handle, windows.ProcessBasicInformation, unsafe.Pointer(&pbi), uint32(unsafe.Sizeof(pbi)), &retLen); err != nil {
		return remoteParameters{}, err
	}
	pebAddr := uintptr(unsafe.Pointer(pbi.PebBaseAddress))
	wow64 := false
	if unsafe.Sizeof(uintptr(0)) == 8 {
		var peb32 uintptr
		if err := windows.NtQueryInformationProcess(handle, windows.ProcessWow64Information, unsafe.Pointer(&peb32), uint32(unsafe.Sizeof(peb32)), &retLen); err != nil {
			return remoteParameters{}, err
		}
		if peb32 != 0 {
			pebAddr = peb32
			wow64 = true
		}
	}
	if pebAddr == 0 {
		return remoteParameters{}, errors.New("nil PEB base address")
	}
	params := remoteParameters{wow64: wow64}
	address, err := params.pointer(handle, pebAddr+params.offset(unsafe.Offsetof(windows.PEB{}.ProcessParameters), 0x10))
	if err != nil {
		return remoteParameters{}, err
	}
	if address == 0 {
		return remoteParameters{}, errors.New("nil process parameters")
	}
	params.address = address
	return params, nil
}

func (p remoteParameters) offset(native, wow64 uintptr) uintptr {
	if p.wow64 {
		return wow64
	}
	return native
}

func (p remoteParameters) pointer(handle windows.Handle, address uintptr) (uintptr, error) {
	var value uintptr
	size := unsafe.Sizeof(value)
	if p.wow64 {
		size = 4
	}
	var read uintptr
	if err := windows.ReadProcessMemory(handle, address, (*byte)(unsafe.Pointer(&value)), size, &read); err != nil {
		return 0, err
	}
	if read != size {
		return 0, errors.New("short pointer read")
	}
	return value, nil
}

func (p remoteParameters) unicodeString(handle windows.Handle, native, wow64 uintptr) (string, error) {
	address := p.address + p.offset(native, wow64)
	length, err := readRemoteU16(handle, address)
	if err != nil || length == 0 {
		return "", err
	}
	buffer, err := p.pointer(handle, address+p.offset(unsafe.Offsetof(windows.NTUnicodeString{}.Buffer), 4))
	if err != nil {
		return "", err
	}
	value := make([]uint16, (int(length)+1)/2)
	var read uintptr
	if err := windows.ReadProcessMemory(handle, buffer, (*byte)(unsafe.Pointer(&value[0])), uintptr(length), &read); err != nil {
		return "", err
	}
	if read != uintptr(length) {
		return "", errors.New("short string read")
	}
	return windows.UTF16ToString(value), nil
}

func (p remoteParameters) environmentVariable(handle windows.Handle, name string) (string, error) {
	address, err := p.pointer(handle, p.address+p.offset(unsafe.Offsetof(windows.RTL_USER_PROCESS_PARAMETERS{}.Environment), 0x48))
	if err != nil || address == 0 {
		return "", err
	}
	var entry []uint16
	for consumed := uintptr(0); consumed < 32*1024*1024; {
		var region windows.MemoryBasicInformation
		if err := windows.VirtualQueryEx(handle, address+consumed, &region, unsafe.Sizeof(region)); err != nil {
			return "", err
		}
		size := min(uintptr(4096), region.BaseAddress+region.RegionSize-address-consumed)
		if size < 2 {
			return "", errors.New("invalid environment region")
		}
		buffer := make([]uint16, size/2)
		var read uintptr
		if err := windows.ReadProcessMemory(handle, address+consumed, (*byte)(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)*2), &read); err != nil {
			return "", err
		}
		if read != uintptr(len(buffer)*2) {
			return "", errors.New("short environment read")
		}
		for _, value := range buffer {
			if value != 0 {
				entry = append(entry, value)
				continue
			}
			if len(entry) == 0 {
				return "", nil
			}
			key, value, ok := strings.Cut(windows.UTF16ToString(entry), "=")
			if ok && strings.EqualFold(key, name) {
				return value, nil
			}
			entry = entry[:0]
		}
		consumed += read
	}
	return "", errors.New("environment exceeds read limit")
}

// readRemoteU16 reads a single uint16 from another process.
func readRemoteU16(handle windows.Handle, addr uintptr) (uint16, error) {
	var v uint16
	var n uintptr
	if err := windows.ReadProcessMemory(handle, addr, (*byte)(unsafe.Pointer(&v)), unsafe.Sizeof(v), &n); err != nil {
		return 0, err
	}
	if n != unsafe.Sizeof(v) {
		return 0, errors.New("short uint16 read")
	}
	return v, nil
}
