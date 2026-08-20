// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"reflect"
	"strings"
	"testing"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/sources"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/stretchr/testify/assert"
)

// TestHasExplicitComponentSelection pins the NEW-1 fix: only an exact name or
// spec path is "explicit". A glob pattern selects broadly and must not defeat
// --include-bare / --shared=omit (it carries no more intent than -a).
func TestHasExplicitComponentSelection(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		filter components.ComponentFilter
		want   bool
	}{
		{"exact name", components.ComponentFilter{ComponentNamePatterns: []string{"curl"}}, true},
		{"spec path", components.ComponentFilter{SpecPaths: []string{"specs/curl/curl.spec"}}, true},
		{"star glob", components.ComponentFilter{ComponentNamePatterns: []string{"*"}}, false},
		{"prefix glob", components.ComponentFilter{ComponentNamePatterns: []string{"lib*"}}, false},
		{"char-class glob", components.ComponentFilter{ComponentNamePatterns: []string{"cur[lp]"}}, false},
		{"question glob", components.ComponentFilter{ComponentNamePatterns: []string{"cur?"}}, false},
		{"glob plus exact", components.ComponentFilter{ComponentNamePatterns: []string{"*", "curl"}}, true},
		{"nothing", components.ComponentFilter{}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, hasExplicitComponentSelection(&tc.filter))
		})
	}
}

// TestCollectCustomizationsEmitsEveryKind complements the reflection-based
// collector coverage by invoking collectCustomizations on a config with every
// customizable field populated and asserting each expected Kind appears.
func TestCollectCustomizationsEmitsEveryKind(t *testing.T) {
	t.Parallel()

	config := projectconfig.ComponentConfig{
		Overlays: []projectconfig.ComponentOverlay{
			{Type: projectconfig.ComponentOverlayAddSpecTag, Tag: "Release", Value: "1"},
		},
		Build: projectconfig.ComponentBuildConfig{
			With:                   []string{"feature"},
			Without:                []string{"docs"},
			Defines:                map[string]string{"macro": "value"},
			Undefines:              []string{"othermacro"},
			Check:                  projectconfig.CheckConfig{Skip: true, SkipReason: "flaky"},
			EmitUpstreamProvenance: true,
		},
		Spec: projectconfig.SpecSource{
			SourceType:     projectconfig.SpecSourceTypeUpstream,
			UpstreamName:   "different-name",
			UpstreamCommit: "abc1234",
			UpstreamDistro: projectconfig.DistroReference{Name: "fedora", Version: "43"},
		},
		Release: projectconfig.ReleaseConfig{
			Calculation: projectconfig.ReleaseCalculationAutorelease,
		},
		Render: projectconfig.ComponentRenderConfig{SkipFileFilter: true},
		Packages: map[string]projectconfig.PackageConfig{
			"libfoo": {},
		},
		SourceFiles: []projectconfig.SourceFileReference{
			{Filename: "extra.tar.gz", ReplaceUpstream: true, ReplaceReason: "vendored fix"},
		},
	}

	wantKinds := []string{
		"spec-add-tag",
		"build.with",
		"build.without",
		"build.defines",
		"build.undefines",
		"build.check.skip",
		"build.emit-upstream-provenance",
		"spec.source-type",
		"spec.upstream-commit",
		"spec.upstream-name",
		"spec.upstream-distro",
		"release.calculation",
		"render.skip-file-filter",
		"packages",
		"source-files",
		"source-files.replace-upstream",
	}

	items := collectCustomizations("comp", &config)

	gotKinds := make(map[string]bool, len(items))
	for _, item := range items {
		gotKinds[item.Kind] = true
	}

	for _, kind := range wantKinds {
		assert.Truef(t, gotKinds[kind],
			"collectCustomizations did not emit an item of Kind %q; "+
				"a collector for it may be unwired or its trigger condition wrong", kind)
	}
}

// TestLockChangeDTOMirrorsSource guards the direction the explicit field-by-field
// copy in [toLockChanges] cannot: a NEW field added to [sources.LockChange] /
// [sources.CommitMetadata] would compile fine
// but silently never reach JSON consumers. This asserts the local DTO carries
// a field of the same type for every exported source field (matched by name),
// so a field addition OR a type change (e.g. int64->int32) trips the test.
func TestLockChangeDTOMirrorsSource(t *testing.T) {
	t.Parallel()

	dtoFields := exportedFieldTypes(reflect.TypeFor[LockChange]())

	for name, srcType := range exportedFieldTypes(reflect.TypeFor[sources.LockChange]()) {
		dtoType, ok := dtoFields[name]
		if !assert.Truef(t, ok,
			"sources.LockChange field %q has no counterpart in the local "+
				"LockChange DTO; add it (and to toLockChanges) so it "+
				"reaches JSON consumers, or it is silently dropped.", name) {
			continue
		}

		assert.Equalf(t, srcType, dtoType,
			"LockChange DTO field %q has type %s but sources.LockChange "+
				"has %s; the explicit copy in toLockChanges would silently "+
				"narrow or mistype the value.", name, dtoType, srcType)
	}
}

// TestRenderCardViewLockChangeHint pins the single-component
// card omits the per-commit LockChangeDetails (to stay scannable) but
// must point the user at -O json whenever lock changes exist, so the
// details aren't a silent dead end.
func TestRenderCardViewLockChangeHint(t *testing.T) {
	t.Parallel()

	var withChanges strings.Builder

	renderCardView(&withChanges, HistoryResult{
		Name:           "curl",
		TomlPath:       "azldev.toml",
		TomlCommits:    3,
		Customizations: 2,
		LockChanges:    2,
	})

	out := withChanges.String()
	assert.Contains(t, out, "Component: curl")
	assert.Contains(t, out, "Lock changes:   2")
	assert.Contains(t, out, "-O json",
		"card should point at -O json when lock changes exist")

	var noChanges strings.Builder

	renderCardView(&noChanges, HistoryResult{Name: "bash"})

	assert.NotContains(t, noChanges.String(), "-O json",
		"no lock changes means no -O json hint")
}

// exportedFieldTypes returns the exported fields of a struct type keyed by
// name -> type, flattening anonymously-embedded structs (e.g. CommitMetadata)
// into the parent's namespace.
func exportedFieldTypes(t reflect.Type) map[string]reflect.Type {
	types := make(map[string]reflect.Type)

	for i := range t.NumField() {
		field := t.Field(i)

		if field.Anonymous && field.Type.Kind() == reflect.Struct {
			for name, typ := range exportedFieldTypes(field.Type) {
				types[name] = typ
			}

			continue
		}

		if field.IsExported() {
			types[field.Name] = field.Type
		}
	}

	return types
}
