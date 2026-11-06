// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/internal/pkg/action"
	"gitea.com/gitea/runner/internal/pkg/action/checkout"
	"gitea.com/gitea/runner/internal/pkg/action/downloadartifact"
	"gitea.com/gitea/runner/internal/pkg/action/uploadartifact"

	"gitea.dev/actionslib/pkg/model"
)

var builtinActions = map[string]func(context.Context, *action.Context) error{
	"checkout":          checkout.Main,
	"download-artifact": downloadartifact.Main,
	"upload-artifact":   uploadartifact.Main,
}

type stepActionBuiltin struct {
	Step       *model.Step
	RunContext *RunContext
	run        func(context.Context, *action.Context) error
	env        map[string]string
}

func (sab *stepActionBuiltin) pre() common.Executor { return common.NewPipelineExecutor() }

func (sab *stepActionBuiltin) main() common.Executor {
	sab.env = map[string]string{}
	return runStepExecutor(sab, stepStageMain, func(ctx context.Context) error {
		if common.Dryrun(ctx) {
			return nil
		}

		printRunActionHeader(ctx, sab.Step, nil, sab.env, sab.RunContext)
		rawLogger := common.Logger(ctx).With(rawOutputField, true)

		rc := sab.RunContext
		rc.ApplyExtraPath(ctx, &sab.env)
		inputs := make(map[string]string, len(sab.Step.With))
		for name, value := range sab.Step.With {
			inputs[strings.ToLower(name)] = value // input names are case-insensitive on GitHub
		}
		return sab.run(ctx, &action.Context{
			Container:       rc.JobContainer,
			Github:          getStepGithubContext(ctx, sab),
			Inputs:          inputs,
			HostWorkdir:     rc.Config.Workdir,
			Workspace:       rc.JobContainer.ToContainerPath(rc.Config.Workdir),
			BindWorkdir:     rc.Config.BindWorkdir,
			NoSkipCheckout:  rc.Config.NoSkipCheckout,
			UseGitIgnore:    rc.Config.UseGitIgnore,
			InsecureSkipTLS: rc.Config.InsecureSkipTLS,
			ClientCertFile:  rc.Config.ClientCertFile,
			ClientKeyFile:   rc.Config.ClientKeyFile,
			CAFile:          rc.jobCAFile(),
			Env:             sab.env,
			ResultsURL:      rc.Config.ResultsURL,
			RuntimeToken:    cmp.Or(rc.runtimeToken, rc.topLevelRunContext().Config.Secrets["ACTIONS_RUNTIME_TOKEN"]), // composite contexts have no secrets
			Transport:       rc.Config.InstanceTransport,
			Infof:           rawLogger.Infof,
			Warning: func(format string, args ...any) {
				rawLogger.Infof("%s", formatAnnotation("warning", nil, fmt.Sprintf(format, args...)))
			},
		})
	})
}

func (sab *stepActionBuiltin) post() common.Executor { return common.NewPipelineExecutor() }

func (sab *stepActionBuiltin) getRunContext() *RunContext { return sab.RunContext }

func (sab *stepActionBuiltin) getStepModel() *model.Step { return sab.Step }

func (sab *stepActionBuiltin) getEnv() *map[string]string { return &sab.env }

func (sab *stepActionBuiltin) getIfExpression(_ context.Context, _ stepStage) string {
	return sab.Step.If.Value
}
