// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component_test

import (
	"os/exec"
	"strings"
	"testing"

	componentcmds "github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/cmds/component"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/testutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/upstreamcommit"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testUpstreamCommitsDir = "/project/base/upstream-commits"

func TestNewUpdateUpstreamCommitCmd(t *testing.T) {
	cmd := componentcmds.NewUpdateUpstreamCommitCmd()
	require.NotNil(t, cmd)
	assert.Equal(t, "update-upstream-commit", cmd.Use)
	assert.NotNil(t, cmd.RunE)
	assert.Nil(t, cmd.Flags().Lookup("bump"))
}

func TestNewUpdateUpstreamCommitCmd_Flags(t *testing.T) {
	cmd := componentcmds.NewUpdateUpstreamCommitCmd()

	allFlag := cmd.Flags().Lookup("all-components")
	require.NotNil(t, allFlag, "all-components flag should be registered")

	componentFlag := cmd.Flags().Lookup("component")
	require.NotNil(t, componentFlag, "component flag should be registered")
}

func TestUpdateUpstreamCommitCmd_NoComponents(t *testing.T) {
	testEnv := testutils.NewTestEnv(t)

	cmd := componentcmds.NewUpdateUpstreamCommitCmd()
	cmd.SetArgs([]string{"nonexistent-component"})

	err := cmd.ExecuteContext(testEnv.Env)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "component not found")
}

// setupMockGit configures the test environment's CmdFactory to simulate git operations.
// git clone: creates a destination directory.
// git rev-parse / git rev-list: returns the provided commit hash.
// All other git commands succeed silently.
func setupMockGit(env *testutils.TestEnv, commitHash string) {
	env.CmdFactory.RegisterCommandInSearchPath("git")

	env.CmdFactory.RunHandler = func(cmd *exec.Cmd) error {
		args := cmd.Args

		// git clone: create a minimal repo structure in the destination dir.
		for idx, arg := range args {
			if arg == "clone" {
				// Last arg is the destination directory.
				destDir := args[len(args)-1]

				return fileutils.MkdirAll(env.TestFS, destDir)
			}

			// git checkout: no-op.
			if arg == "checkout" {
				return nil
			}

			// git -C <dir> rev-list: return the commit hash (for snapshot resolution).
			if arg == "rev-list" || (idx > 0 && args[idx-1] == "-C" && strings.Contains(strings.Join(args, " "), "rev-list")) {
				return nil
			}
		}

		return nil
	}

	env.CmdFactory.RunAndGetOutputHandler = func(cmd *exec.Cmd) (string, error) {
		// git rev-parse HEAD: return the configured commit hash.
		if strings.Contains(strings.Join(cmd.Args, " "), "rev-parse") {
			return commitHash, nil
		}

		// git log / git rev-list --before: return the commit hash.
		if strings.Contains(strings.Join(cmd.Args, " "), "rev-list") {
			return commitHash, nil
		}

		return "", nil
	}
}

// addUpstreamComponent adds an upstream component to the test config
// without pre-populating generated commit TOML.
func addUpstreamComponent(env *testutils.TestEnv, name string) {
	env.Config.Components[name] = projectconfig.ComponentConfig{
		Name: name,
		Spec: projectconfig.SpecSource{
			SourceType: projectconfig.SpecSourceTypeUpstream,
		},
	}
}

// TestUpdateComponents_WritesCommit exercises the full update pipeline.
func TestUpdateComponents_WritesCommit(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const commit = "abc123def456"

	setupMockGit(env, commit)
	addUpstreamComponent(env, "curl")

	require.NoError(t, fileutils.MkdirAll(env.TestFS, testUpstreamCommitsDir))

	results, err := componentcmds.UpdateComponents(env.Env, &componentcmds.UpdateComponentOptions{
		ComponentFilter: components.ComponentFilter{IncludeAllComponents: true},
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.True(t, results[0].Changed)
	assert.Equal(t, commit, results[0].UpstreamCommit)

	store := upstreamcommit.NewStore(env.TestFS, testUpstreamCommitsDir)

	savedCommit, exists, loadErr := store.Get("curl")
	require.NoError(t, loadErr)
	assert.True(t, exists)
	assert.Equal(t, commit, savedCommit)
}

func TestUpdateComponents_ConfigOnlyChangeDoesNotChangeCommitTOML(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const commit = "abc123def456"

	setupMockGit(env, commit)
	addUpstreamComponent(env, "curl")

	require.NoError(t, fileutils.MkdirAll(env.TestFS, testUpstreamCommitsDir))

	options := &componentcmds.UpdateComponentOptions{
		ComponentFilter: components.ComponentFilter{IncludeAllComponents: true},
	}

	results, err := componentcmds.UpdateComponents(env.Env, options)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.True(t, results[0].Changed)

	modifiedConfig := env.Config.Components["curl"]
	modifiedConfig.Build.With = []string{"ssl"}
	env.Config.Components["curl"] = modifiedConfig

	results, err = componentcmds.UpdateComponents(env.Env, options)
	require.NoError(t, err)
	assert.Empty(t, results)

	store := upstreamcommit.NewStore(env.TestFS, testUpstreamCommitsDir)
	savedCommit, exists, err := store.Get("curl")
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Equal(t, commit, savedCommit)
}

// TestUpdateComponents_MultipleComponents tests update with multiple components.
func TestUpdateComponents_MultipleComponents(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const commit = "multi-commit-hash"

	setupMockGit(env, commit)
	addUpstreamComponent(env, "curl")
	addUpstreamComponent(env, "bash")

	require.NoError(t, fileutils.MkdirAll(env.TestFS, testUpstreamCommitsDir))

	results, err := componentcmds.UpdateComponents(env.Env, &componentcmds.UpdateComponentOptions{
		ComponentFilter: components.ComponentFilter{IncludeAllComponents: true},
	})
	require.NoError(t, err)

	// Should have results for both (may include skipped too).
	var changedNames []string

	for _, r := range results {
		if r.Changed {
			changedNames = append(changedNames, r.Component)
		}
	}

	assert.Contains(t, changedNames, "curl")
	assert.Contains(t, changedNames, "bash")

	store := upstreamcommit.NewStore(env.TestFS, testUpstreamCommitsDir)

	curlCommit, curlExists, err := store.Get("curl")
	require.NoError(t, err)
	bashCommit, bashExists, err := store.Get("bash")
	require.NoError(t, err)
	assert.True(t, curlExists)
	assert.True(t, bashExists)
	assert.Equal(t, commit, curlCommit)
	assert.Equal(t, commit, bashCommit)
}

func TestUpdateComponents_LocalComponentDoesNotWriteCommitTOML(t *testing.T) {
	env := testutils.NewTestEnv(t)

	env.Config.Components["local-pkg"] = projectconfig.ComponentConfig{
		Name: "local-pkg",
		Spec: projectconfig.SpecSource{
			SourceType: projectconfig.SpecSourceTypeLocal,
			Path:       "/project/specs/local-pkg/local-pkg.spec",
		},
	}

	require.NoError(t, fileutils.MkdirAll(env.TestFS, testUpstreamCommitsDir))

	results, err := componentcmds.UpdateComponents(env.Env, &componentcmds.UpdateComponentOptions{
		ComponentFilter: components.ComponentFilter{IncludeAllComponents: true},
	})
	require.NoError(t, err)
	assert.Empty(t, results)

	store := upstreamcommit.NewStore(env.TestFS, testUpstreamCommitsDir)
	_, exists, err := store.Get("local-pkg")
	require.NoError(t, err)
	assert.False(t, exists)
}

// TestUpdateComponents_AdvancesStaleCommit is a regression test for the case
// where a generated pin is at commit A and the snapshot resolves to
// commit B must result in B being written (not A echoed back). Without
// clearing the configured pin before re-resolution, source resolution would
// return A and the generated TOML would never advance.
func TestUpdateComponents_AdvancesStaleCommit(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const initialCommit = "initial-aaa111"

	const advancedCommit = "advanced-bbb222"

	require.NoError(t, fileutils.MkdirAll(env.TestFS, testUpstreamCommitsDir))
	store := upstreamcommit.NewStore(env.TestFS, testUpstreamCommitsDir)
	require.NoError(t, store.Save("curl", initialCommit))

	addUpstreamComponent(env, "curl")

	// Mock git now resolves to a NEW commit — upstream moved.
	setupMockGit(env, advancedCommit)

	results, err := componentcmds.UpdateComponents(env.Env, &componentcmds.UpdateComponentOptions{
		ComponentFilter: components.ComponentFilter{IncludeAllComponents: true},
	})
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Equal(t, advancedCommit, results[0].UpstreamCommit,
		"update must re-resolve and return the advanced commit, not echo the configured one")
	assert.True(t, results[0].Changed, "generated commit advanced")
	assert.Equal(t, initialCommit, results[0].PreviousCommit,
		"PreviousCommit should track the prior generated TOML")

	freshStore := upstreamcommit.NewStore(env.TestFS, testUpstreamCommitsDir)
	updatedCommit, exists, loadErr := freshStore.Get("curl")
	require.NoError(t, loadErr)
	assert.True(t, exists)
	assert.Equal(t, advancedCommit, updatedCommit)
}

// TestUpdateComponents_CheckOnly_StaleReturnsError verifies that --check-only
// returns a non-nil error when a generated commit TOML is stale without writing.
func TestUpdateComponents_CheckOnly_StaleReturnsError(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const initialCommit = "initial-aaa111"

	const advancedCommit = "advanced-bbb222"

	require.NoError(t, fileutils.MkdirAll(env.TestFS, testUpstreamCommitsDir))
	preStore := upstreamcommit.NewStore(env.TestFS, testUpstreamCommitsDir)
	require.NoError(t, preStore.Save("curl", initialCommit))

	addUpstreamComponent(env, "curl")
	setupMockGit(env, advancedCommit)

	results, err := componentcmds.UpdateComponents(env.Env, &componentcmds.UpdateComponentOptions{
		ComponentFilter: components.ComponentFilter{IncludeAllComponents: true},
		CheckOnly:       true,
	})
	require.Error(t, err, "stale TOML must produce a non-nil error in --check-only mode")
	assert.Contains(t, err.Error(), "stale", "error message should mention staleness")
	assert.Contains(t, err.Error(), "curl", "error message should name the stale component")
	assert.Contains(t, err.Error(), "azldev component update-upstream-commit -a",
		"-a-scoped run should suggest the same -a invocation to refresh")

	// Results slice must be returned alongside the error so structured
	// consumers (e.g. -O json) retain per-component data on stale runs.
	require.NotEmpty(t, results, "results must be returned even when stale")

	var foundCurl bool

	for _, r := range results {
		if r.Component == "curl" {
			foundCurl = true

			assert.True(t, r.Changed, "stale curl must surface as Changed in returned results")
		}
	}

	assert.True(t, foundCurl, "stale curl must appear in returned results slice")

	freshStore := upstreamcommit.NewStore(env.TestFS, testUpstreamCommitsDir)
	savedCommit, exists, loadErr := freshStore.Get("curl")
	require.NoError(t, loadErr)
	assert.True(t, exists)
	assert.Equal(t, initialCommit, savedCommit)
}

// TestUpdateComponents_CheckOnly_FreshReturnsNil verifies that --check-only
// returns nil when all generated commit TOMLs are fresh.
func TestUpdateComponents_CheckOnly_FreshReturnsNil(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const commit = "fresh-commit-aaa"

	setupMockGit(env, commit)
	addUpstreamComponent(env, "curl")
	require.NoError(t, fileutils.MkdirAll(env.TestFS, testUpstreamCommitsDir))

	options := &componentcmds.UpdateComponentOptions{
		ComponentFilter: components.ComponentFilter{IncludeAllComponents: true},
	}

	// Phase 1: populate the generated TOML with a real update run.
	_, err := componentcmds.UpdateComponents(env.Env, options)
	require.NoError(t, err)

	freshStore := upstreamcommit.NewStore(env.TestFS, testUpstreamCommitsDir)
	before, beforeExists, loadErr := freshStore.Get("curl")
	require.NoError(t, loadErr)
	require.True(t, beforeExists)

	// Phase 2: --check-only against the now-fresh TOML. Must return nil.
	options.CheckOnly = true
	_, err = componentcmds.UpdateComponents(env.Env, options)
	require.NoError(t, err, "fresh TOMLs must return nil error in --check-only mode")

	// The configured commit must remain unchanged.
	freshStore = upstreamcommit.NewStore(env.TestFS, testUpstreamCommitsDir)
	after, afterExists, loadErr := freshStore.Get("curl")
	require.NoError(t, loadErr)
	require.True(t, afterExists)
	assert.Equal(t, before, after)
}

// TestUpdateComponents_CheckOnly_DetectsOrphans verifies that --check-only
// returns an error when an orphan generated TOML would be pruned by a normal run,
// and that the orphan is NOT actually deleted.
func TestUpdateComponents_CheckOnly_DetectsOrphans(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const commit = "fresh-commit-aaa"

	setupMockGit(env, commit)
	addUpstreamComponent(env, "curl")
	require.NoError(t, fileutils.MkdirAll(env.TestFS, testUpstreamCommitsDir))

	// First, do a real update so curl's TOML is fresh -- isolates the orphan as
	// the only thing --check-only should flag.
	_, err := componentcmds.UpdateComponents(env.Env, &componentcmds.UpdateComponentOptions{
		ComponentFilter: components.ComponentFilter{IncludeAllComponents: true},
	})
	require.NoError(t, err)

	// Plant an orphan TOML AFTER the update -- a normal update would have
	// pruned it. The orphan does NOT correspond to any component in config.
	preStore := upstreamcommit.NewStore(env.TestFS, testUpstreamCommitsDir)
	require.NoError(t, preStore.Save("removed-pkg", "orphan-commit"))

	// --check-only must report the orphan and not delete it.
	_, err = componentcmds.UpdateComponents(env.Env, &componentcmds.UpdateComponentOptions{
		ComponentFilter: components.ComponentFilter{IncludeAllComponents: true},
		CheckOnly:       true,
	})
	require.Error(t, err, "orphan TOML must produce an error in --check-only mode")
	assert.Contains(t, err.Error(), "orphan")
	assert.Contains(t, err.Error(), "removed-pkg")

	freshStore := upstreamcommit.NewStore(env.TestFS, testUpstreamCommitsDir)
	_, exists, loadErr := freshStore.Get("removed-pkg")
	require.NoError(t, loadErr)
	assert.True(t, exists, "--check-only must not prune orphan TOMLs")
}
