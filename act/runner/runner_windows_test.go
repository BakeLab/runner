// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"strings"
	"testing"

	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/act/common/logging/logtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunEventHostEnvironmentUTF8(t *testing.T) {
	handler := logtest.New()
	t.Cleanup(func() {
		if t.Failed() {
			for _, record := range handler.AllRecords() {
				stepIDs, _ := record.Attr("stepID")
				t.Logf("stepID=%v: %s", stepIDs, record.Message)
			}
		}
	})
	table := TestJobFileInfo{
		workdir:      workdir,
		workflowPath: "windows-utf8",
		eventName:    "push",
		platforms:    map[string]string{"windows-latest": "-self-hosted"},
	}
	table.runTest(WithJobLoggerFactory(t.Context(), factoryFunc(func() logging.Handler { return handler })), t, &Config{Env: testCheckoutEnv()})
	var outputSteps []string
	for _, record := range handler.AllRecords() {
		if strings.TrimRight(record.Message, "\r\n") == "你好，世界" {
			stepIDs, ok := record.Attr("stepID")
			require.True(t, ok)
			outputSteps = append(outputSteps, stepIDs.([]string)...)
		}
	}
	assert.Equal(t, []string{"cmd", "pwsh", "powershell"}, outputSteps)
}
