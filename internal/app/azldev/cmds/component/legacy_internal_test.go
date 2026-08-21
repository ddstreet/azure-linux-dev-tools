// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLegacyUpdateCommandIsHiddenNoOp(t *testing.T) {
	parent := &cobra.Command{Use: "component"}
	legacyOnAppInit(nil, parent)

	var output bytes.Buffer
	parent.SetOut(&output)
	parent.SetErr(&output)
	parent.SetArgs([]string{"update", "-a", "--bump", "curl"})

	require.NoError(t, parent.Execute())
	assert.Equal(t, legacyUpdateMessage+"\n", output.String())

	command, _, err := parent.Find([]string{"update"})
	require.NoError(t, err)
	assert.True(t, command.Hidden)
	assert.True(t, command.DisableFlagParsing)
}
