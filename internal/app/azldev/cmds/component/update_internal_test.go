// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"testing"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/testutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/lockfile"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testLockDir is the lock directory used by TestEnv's project layout.
const (
	testLockDir    = "/project/locks"
	testCommitHash = "abc123"
)

// newTestStore creates a lockfile.Store backed by the TestEnv's in-memory filesystem.
func newTestStore(t *testing.T, env *testutils.TestEnv) *lockfile.Store {
	t.Helper()

	return lockfile.NewStore(env.TestFS, testLockDir)
}

// makeResult builds an UpdateResult for testing saveComponentLocks.
//

func makeResult(name, commit string, config *projectconfig.ComponentConfig) UpdateResult {
	return UpdateResult{
		Component:      name,
		UpstreamCommit: commit,
		Changed:        true,
		config:         config,
		sourceIdentity: commit,
	}
}

// readLock loads a lock file from the store and returns it. Fails the test on error.
func readLock(t *testing.T, store *lockfile.Store) *lockfile.ComponentLock {
	t.Helper()

	lock, err := store.Get("curl")
	require.NoError(t, err, "reading lock for %q", "curl")

	return lock
}

// baseConfig returns a minimal upstream component config suitable for fingerprinting.
// The config has source type "upstream" which tells ComputeIdentity to expect a
// SourceIdentity (provided via the lock's UpstreamCommit).
func baseConfig(name string) *projectconfig.ComponentConfig {
	return &projectconfig.ComponentConfig{
		Name: name,
		Spec: projectconfig.SpecSource{
			SourceType: projectconfig.SpecSourceTypeUpstream,
		},
	}
}

func TestSaveComponentLocks_ComputesFingerprint(t *testing.T) {
	env := testutils.NewTestEnv(t)
	store := newTestStore(t, env)

	results := []UpdateResult{
		makeResult("curl", testCommitHash, baseConfig("curl")),
	}

	err := saveComponentLocks(env.Env, store, results, false)
	require.NoError(t, err)

	lock := readLock(t, store)
	assert.Equal(t, testCommitHash, lock.UpstreamCommit)
	assert.NotEmpty(t, lock.InputFingerprint, "fingerprint should be computed and stored")
	assert.Contains(t, lock.InputFingerprint, "sha256:", "fingerprint should have sha256 prefix")
}

func TestSaveComponentLocks_DetectsFingerprintChange(t *testing.T) {
	env := testutils.NewTestEnv(t)
	store := newTestStore(t, env)

	// First save — establishes baseline fingerprint.
	config1 := baseConfig("curl")
	results1 := []UpdateResult{makeResult("curl", testCommitHash, config1)}

	require.NoError(t, saveComponentLocks(env.Env, store, results1, false))

	fp1 := readLock(t, store).InputFingerprint
	require.NotEmpty(t, fp1)

	// Second save — same commit, but config changed (added build option).
	config2 := baseConfig("curl")
	config2.Build.With = []string{"feature_x"}

	results2 := []UpdateResult{
		{
			Component:      "curl",
			UpstreamCommit: testCommitHash,
			Changed:        false, // commit didn't change
			config:         config2,
			sourceIdentity: testCommitHash,
		},
	}

	require.NoError(t, saveComponentLocks(env.Env, store, results2, false))

	fp2 := readLock(t, store).InputFingerprint
	assert.NotEqual(t, fp1, fp2, "fingerprint should change when config changes")
	assert.True(t, results2[0].Changed, "Changed should be set to true by fingerprint diff")
}

func TestSaveComponentLocks_SkipsUnchanged(t *testing.T) {
	env := testutils.NewTestEnv(t)
	store := newTestStore(t, env)

	config := baseConfig("curl")

	// First save.
	results1 := []UpdateResult{makeResult("curl", testCommitHash, config)}
	require.NoError(t, saveComponentLocks(env.Env, store, results1, false))

	fp1 := readLock(t, store).InputFingerprint

	// Second save — identical commit and config. Changed starts as false.
	results2 := []UpdateResult{
		{
			Component:      "curl",
			UpstreamCommit: testCommitHash,
			Changed:        false,
			config:         config,
			sourceIdentity: testCommitHash,
		},
	}

	require.NoError(t, saveComponentLocks(env.Env, store, results2, false))

	assert.False(t, results2[0].Changed, "should remain unchanged when fingerprint matches")

	// Fingerprint should still be the same.
	fp2 := readLock(t, store).InputFingerprint
	assert.Equal(t, fp1, fp2)
}

func TestSaveComponentLocks_SkipsErrorAndSkipped(t *testing.T) {
	env := testutils.NewTestEnv(t)
	store := newTestStore(t, env)

	results := []UpdateResult{
		{Component: "errored", Error: "resolution failed", config: baseConfig("errored")},
		{Component: "skipped", Skipped: true, SkipReason: "local", config: baseConfig("skipped")},
	}

	err := saveComponentLocks(env.Env, store, results, false)
	require.NoError(t, err)

	// Neither should have lock files.
	exists1, _ := store.Exists("errored")
	assert.False(t, exists1)

	exists2, _ := store.Exists("skipped")
	assert.False(t, exists2)
}
