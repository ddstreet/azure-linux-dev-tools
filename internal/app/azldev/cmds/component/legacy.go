// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/spf13/cobra"
)

const (
	legacyUpdateMessage = "azldev component update no longer does anything and should no longer be used."
)

func legacyOnAppInit(_ *azldev.App, parentCmd *cobra.Command) {
	parentCmd.AddCommand(
		newLegacyNoOpCmd("update", nil, legacyUpdateMessage),
	)
}

func newLegacyNoOpCmd(name string, aliases []string, message string) *cobra.Command {
	return &cobra.Command{
		Use:                name,
		Aliases:            aliases,
		Short:              message,
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			cmd.Println(message)
		},
	}
}
