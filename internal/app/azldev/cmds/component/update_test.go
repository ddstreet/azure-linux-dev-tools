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
	"github.com/microsoft/azure-linux-dev-tools/internal/lockfile"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testLockDir = "/project/locks"

func TestNewUpdateCmd(t *testing.T) {
	cmd := componentcmds.NewUpdateCmd()
	require.NotNil(t, cmd)
	assert.Equal(t, "update", cmd.Use)
	assert.NotNil(t, cmd.RunE)
	assert.Nil(t, cmd.Flags().Lookup("bump"))
}

func TestNewUpdateCmd_Flags(t *testing.T) {
	cmd := componentcmds.NewUpdateCmd()

	allFlag := cmd.Flags().Lookup("all-components")
	require.NotNil(t, allFlag, "all-components flag should be registered")

	componentFlag := cmd.Flags().Lookup("component")
	require.NotNil(t, componentFlag, "component flag should be registered")
}

func TestUpdateCmd_NoComponents(t *testing.T) {
	testEnv := testutils.NewTestEnv(t)

	cmd := componentcmds.NewUpdateCmd()
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
// without writing a lock file. Used by update tests where the update command
// itself creates the lock.
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

	// Pre-create a lock file so the spec file is writable on memfs.
	require.NoError(t, fileutils.MkdirAll(env.TestFS, testLockDir))

	results, err := componentcmds.UpdateComponents(env.Env, &componentcmds.UpdateComponentOptions{
		ComponentFilter: components.ComponentFilter{IncludeAllComponents: true},
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.True(t, results[0].Changed)
	assert.Equal(t, commit, results[0].UpstreamCommit)

	store := lockfile.NewStore(env.TestFS, testLockDir)

	lock, loadErr := store.Get("curl")
	require.NoError(t, loadErr)
	assert.Equal(t, commit, lock.UpstreamCommit)
}

func TestUpdateComponents_ConfigOnlyChangeDoesNotChangeLock(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const commit = "abc123def456"

	setupMockGit(env, commit)
	addUpstreamComponent(env, "curl")

	require.NoError(t, fileutils.MkdirAll(env.TestFS, testLockDir))

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

	store := lockfile.NewStore(env.TestFS, testLockDir)
	lock, err := store.Get("curl")
	require.NoError(t, err)
	assert.Equal(t, commit, lock.UpstreamCommit)
}

// TestUpdateComponents_MultipleComponents tests update with multiple components.
func TestUpdateComponents_MultipleComponents(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const commit = "multi-commit-hash"

	setupMockGit(env, commit)
	addUpstreamComponent(env, "curl")
	addUpstreamComponent(env, "bash")

	require.NoError(t, fileutils.MkdirAll(env.TestFS, testLockDir))

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

	// Both should have lock files with resolved commits.
	store := lockfile.NewStore(env.TestFS, testLockDir)

	curlLock, err := store.Get("curl")
	require.NoError(t, err)
	bashLock, err := store.Get("bash")
	require.NoError(t, err)
	assert.Equal(t, commit, curlLock.UpstreamCommit)
	assert.Equal(t, commit, bashLock.UpstreamCommit)
}

func TestUpdateComponents_LocalComponentDoesNotWriteLock(t *testing.T) {
	env := testutils.NewTestEnv(t)

	env.Config.Components["local-pkg"] = projectconfig.ComponentConfig{
		Name: "local-pkg",
		Spec: projectconfig.SpecSource{
			SourceType: projectconfig.SpecSourceTypeLocal,
			Path:       "/project/specs/local-pkg/local-pkg.spec",
		},
	}

	require.NoError(t, fileutils.MkdirAll(env.TestFS, testLockDir))

	results, err := componentcmds.UpdateComponents(env.Env, &componentcmds.UpdateComponentOptions{
		ComponentFilter: components.ComponentFilter{IncludeAllComponents: true},
	})
	require.NoError(t, err)
	assert.Empty(t, results)

	store := lockfile.NewStore(env.TestFS, testLockDir)
	exists, err := store.Exists("local-pkg")
	require.NoError(t, err)
	assert.False(t, exists)
}

// TestUpdateComponents_AdvancesStaleLock is a regression test for the case
// where a pre-existing lock at commit A and an upstream that has moved to
// commit B must result in B being written (not A echoed back). Without
// clearing populated lock data before re-resolution, the source provider's
// locked-commit short-circuit in ResolveIdentity would return A and the
// lock would never advance.
func TestUpdateComponents_AdvancesStaleLock(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const initialCommit = "initial-aaa111"

	const advancedCommit = "advanced-bbb222"

	// Pre-populate a lock at initialCommit (simulates a previous update run).
	require.NoError(t, fileutils.MkdirAll(env.TestFS, testLockDir))

	preExistingLock := lockfile.New()
	preExistingLock.UpstreamCommit = initialCommit

	store := lockfile.NewStore(env.TestFS, testLockDir)
	require.NoError(t, store.Save("curl", preExistingLock))

	addUpstreamComponent(env, "curl")

	// Mock git now resolves to a NEW commit — upstream moved.
	setupMockGit(env, advancedCommit)

	results, err := componentcmds.UpdateComponents(env.Env, &componentcmds.UpdateComponentOptions{
		ComponentFilter: components.ComponentFilter{IncludeAllComponents: true},
	})
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Equal(t, advancedCommit, results[0].UpstreamCommit,
		"update must re-resolve and return the advanced commit, not echo the locked one")
	assert.True(t, results[0].Changed, "lock advanced from initial to advanced commit")
	assert.Equal(t, initialCommit, results[0].PreviousCommit,
		"PreviousCommit should track what was in the lock before update")

	// Verify the lock on disk was actually updated. Use a fresh store to
	// bypass the cache held by the pre-population store.
	freshStore := lockfile.NewStore(env.TestFS, testLockDir)

	updatedLock, loadErr := freshStore.Get("curl")
	require.NoError(t, loadErr)
	assert.Equal(t, advancedCommit, updatedLock.UpstreamCommit,
		"lock file on disk must contain the new commit")
}

// TestUpdateComponents_CheckOnly_StaleReturnsError verifies that --check-only
// returns a non-nil error (-> exit 1) when a component's lock is stale, and
// that no lock file is written.
func TestUpdateComponents_CheckOnly_StaleReturnsError(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const initialCommit = "initial-aaa111"

	const advancedCommit = "advanced-bbb222"

	// Pre-populate a lock at initialCommit, then move upstream forward.
	require.NoError(t, fileutils.MkdirAll(env.TestFS, testLockDir))
	preStore := lockfile.NewStore(env.TestFS, testLockDir)
	preLock := lockfile.New()
	preLock.UpstreamCommit = initialCommit
	require.NoError(t, preStore.Save("curl", preLock))

	addUpstreamComponent(env, "curl")
	setupMockGit(env, advancedCommit)

	results, err := componentcmds.UpdateComponents(env.Env, &componentcmds.UpdateComponentOptions{
		ComponentFilter: components.ComponentFilter{IncludeAllComponents: true},
		CheckOnly:       true,
	})
	require.Error(t, err, "stale lock must produce a non-nil error in --check-only mode")
	assert.Contains(t, err.Error(), "stale", "error message should mention staleness")
	assert.Contains(t, err.Error(), "curl", "error message should name the stale component")
	assert.Contains(t, err.Error(), "azldev component update -a",
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

	// Lock on disk must still hold the OLD commit -- --check-only must not write.
	freshStore := lockfile.NewStore(env.TestFS, testLockDir)
	lock, loadErr := freshStore.Get("curl")
	require.NoError(t, loadErr)
	assert.Equal(t, initialCommit, lock.UpstreamCommit,
		"--check-only must not modify the lock file on disk")
}

// TestUpdateComponents_CheckOnly_FreshReturnsNil verifies that --check-only
// returns nil (-> exit 0) when all locks are already fresh.
func TestUpdateComponents_CheckOnly_FreshReturnsNil(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const commit = "fresh-commit-aaa"

	setupMockGit(env, commit)
	addUpstreamComponent(env, "curl")
	require.NoError(t, fileutils.MkdirAll(env.TestFS, testLockDir))

	options := &componentcmds.UpdateComponentOptions{
		ComponentFilter: components.ComponentFilter{IncludeAllComponents: true},
	}

	// Phase 1: populate the lock with a real update run.
	_, err := componentcmds.UpdateComponents(env.Env, options)
	require.NoError(t, err)

	// Snapshot the on-disk lock state so we can assert nothing was rewritten.
	freshStore := lockfile.NewStore(env.TestFS, testLockDir)
	before, loadErr := freshStore.Get("curl")
	require.NoError(t, loadErr)

	// Phase 2: --check-only against the now-fresh lock. Must return nil.
	options.CheckOnly = true
	_, err = componentcmds.UpdateComponents(env.Env, options)
	require.NoError(t, err, "fresh locks must return nil error in --check-only mode")

	// Lock on disk must be byte-identical (no rewrite, no timestamp churn).
	freshStore = lockfile.NewStore(env.TestFS, testLockDir)
	after, loadErr := freshStore.Get("curl")
	require.NoError(t, loadErr)
	assert.Equal(t, before.UpstreamCommit, after.UpstreamCommit)
}

// TestUpdateComponents_CheckOnly_DetectsOrphans verifies that --check-only
// returns an error when an orphan lock file would be pruned by a normal run,
// and that the orphan is NOT actually deleted.
func TestUpdateComponents_CheckOnly_DetectsOrphans(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const commit = "fresh-commit-aaa"

	setupMockGit(env, commit)
	addUpstreamComponent(env, "curl")
	require.NoError(t, fileutils.MkdirAll(env.TestFS, testLockDir))

	// First, do a real update so curl's lock is fresh -- isolates the orphan as
	// the only thing --check-only should flag.
	_, err := componentcmds.UpdateComponents(env.Env, &componentcmds.UpdateComponentOptions{
		ComponentFilter: components.ComponentFilter{IncludeAllComponents: true},
	})
	require.NoError(t, err)

	// Plant an orphan lock AFTER the update -- a normal update would have
	// pruned it. The orphan does NOT correspond to any component in config.
	preStore := lockfile.NewStore(env.TestFS, testLockDir)
	orphanLock := lockfile.New()
	orphanLock.UpstreamCommit = "orphan-commit"
	require.NoError(t, preStore.Save("removed-pkg", orphanLock))

	// --check-only must report the orphan and not delete it.
	_, err = componentcmds.UpdateComponents(env.Env, &componentcmds.UpdateComponentOptions{
		ComponentFilter: components.ComponentFilter{IncludeAllComponents: true},
		CheckOnly:       true,
	})
	require.Error(t, err, "orphan lock must produce an error in --check-only mode")
	assert.Contains(t, err.Error(), "orphan")
	assert.Contains(t, err.Error(), "removed-pkg")

	// Orphan lock must still exist on disk.
	freshStore := lockfile.NewStore(env.TestFS, testLockDir)
	_, loadErr := freshStore.Get("removed-pkg")
	require.NoError(t, loadErr, "--check-only must not prune orphan locks")
}
