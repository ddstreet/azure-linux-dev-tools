// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component_test

import (
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	componentcmds "github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/cmds/component"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/testutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupMockGitWithCounter(env *testutils.TestEnv, commitHash string) *atomic.Int32 {
	var gitCalls atomic.Int32

	env.CmdFactory.RegisterCommandInSearchPath("git")
	env.CmdFactory.RunHandler = func(cmd *exec.Cmd) error {
		gitCalls.Add(1)

		for _, arg := range cmd.Args {
			if arg == "clone" {
				return fileutils.MkdirAll(env.TestFS, cmd.Args[len(cmd.Args)-1])
			}
		}

		return nil
	}
	env.CmdFactory.RunAndGetOutputHandler = func(cmd *exec.Cmd) (string, error) {
		gitCalls.Add(1)

		args := strings.Join(cmd.Args, " ")
		if strings.Contains(args, "rev-parse") || strings.Contains(args, "rev-list") {
			return commitHash, nil
		}

		return "", nil
	}

	return &gitCalls
}

func TestUpdateAlwaysResolvesUpstreamComponent(t *testing.T) {
	env := testutils.NewTestEnv(t)

	gitCalls := setupMockGitWithCounter(env, "aabbccdd11223344")
	addUpstreamComponent(env, "curl")

	options := &componentcmds.RefreshUpstreamCommitOptions{
		ComponentFilter: components.ComponentFilter{IncludeAllComponents: true},
	}

	_, err := componentcmds.RefreshUpstreamCommits(env.Env, options)
	require.NoError(t, err)
	require.Positive(t, gitCalls.Load())

	gitCalls.Store(0)

	results, err := componentcmds.RefreshUpstreamCommits(env.Env, options)
	require.NoError(t, err)
	assert.Positive(t, gitCalls.Load(), "repeated updates must re-resolve upstream state")

	for _, result := range results {
		if result.Component == "curl" {
			assert.False(t, result.Changed)
		}
	}
}
