// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"testing"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/testutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/lockfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testLockDir = "/project/locks"

func TestSaveComponentLocks_WritesChangedCommit(t *testing.T) {
	env := testutils.NewTestEnv(t)
	store := lockfile.NewStore(env.TestFS, testLockDir)
	results := []UpdateResult{{
		Component:      "curl",
		UpstreamCommit: "abc123",
		Changed:        true,
	}}

	require.NoError(t, saveComponentLocks(store, results, false))

	lock, err := store.Get("curl")
	require.NoError(t, err)
	assert.Equal(t, "abc123", lock.UpstreamCommit)
}

func TestSaveComponentLocks_SkipsUnchangedAndFailed(t *testing.T) {
	env := testutils.NewTestEnv(t)
	store := lockfile.NewStore(env.TestFS, testLockDir)
	results := []UpdateResult{
		{Component: "unchanged"},
		{Component: "errored", Changed: true, Error: "resolution failed"},
		{Component: "skipped", Changed: true, Skipped: true, SkipReason: "cancelled"},
	}

	require.NoError(t, saveComponentLocks(store, results, false))

	for _, componentName := range []string{"unchanged", "errored", "skipped"} {
		exists, err := store.Exists(componentName)
		require.NoError(t, err)
		assert.False(t, exists)
	}
}
