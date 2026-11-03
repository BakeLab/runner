// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package process

import "golang.org/x/sys/windows"

func UTF8Console() { // per step, as GitHub's runner runs chcp 65001 before its steps
	_ = windows.SetConsoleCP(65001)
	_ = windows.SetConsoleOutputCP(65001)
}
