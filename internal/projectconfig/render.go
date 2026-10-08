// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package projectconfig

// ComponentRenderConfig encapsulates configuration for rendering a component.
type ComponentRenderConfig struct {
	// SkipFileFilter disables the post-render filter that normally removes
	// files not referenced by Source or Patch tags in the rendered spec.
	SkipFileFilter bool `toml:"skip-file-filter,omitempty" json:"skipFileFilter,omitempty" jsonschema:"title=Skip file filter,description=Disable post-render file filtering for specs with unexpandable macros in Source/Patch tags"`
}
