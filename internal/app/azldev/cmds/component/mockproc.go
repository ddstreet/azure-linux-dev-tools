// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

// Required packages for rendering with the shared MockProcessor.
//
// Rendering needs rpmautospec (macro expansion), rpmdevtools (spectool), and git
// (required for rpmautospec to read commit history). python3-click is required
// by rpmautospec but not declared as an RPM dependency. Ecosystem macro
// packages (go-srpm-macros, etc.) are already present via @buildsys-build →
// azurelinux-rpm-config.
func mockPackagesForRender() []string {
	return []string{"rpmautospec", "rpmdevtools", "git", "python3-click"}
}
