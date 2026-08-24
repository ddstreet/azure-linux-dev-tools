// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component_test

import (
	"testing"

	componentcmds "github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/cmds/component"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewChangedCmd(t *testing.T) {
	cmd := componentcmds.NewChangedCmd()
	require.NotNil(t, cmd)
	assert.Equal(t, "changed", cmd.Use)
	assert.NotNil(t, cmd.RunE)
}

func TestNewChangedCmd_Flags(t *testing.T) {
	cmd := componentcmds.NewChangedCmd()

	fromFlag := cmd.Flags().Lookup("from")
	require.NotNil(t, fromFlag, "--from flag should be registered")

	toFlag := cmd.Flags().Lookup("to")
	require.NotNil(t, toFlag, "--to flag should be registered")
	assert.Equal(t, "HEAD", toFlag.DefValue, "--to should default to HEAD")

	includeUnchangedFlag := cmd.Flags().Lookup("include-unchanged")
	require.NotNil(t, includeUnchangedFlag, "--include-unchanged flag should be registered")

	componentFlag := cmd.Flags().Lookup("component")
	require.NotNil(t, componentFlag, "--component filter flag should be registered")

	allComponentsFlag := cmd.Flags().Lookup("all-components")
	require.NotNil(t, allComponentsFlag, "--all-components flag should be registered")

	assert.Nil(t, cmd.Flags().Lookup("upstream-commits-dir"),
		"--upstream-commits-dir should not be registered")
}

func TestNewChangedCmd_FromRequired(t *testing.T) {
	cmd := componentcmds.NewChangedCmd()
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "from")
}
