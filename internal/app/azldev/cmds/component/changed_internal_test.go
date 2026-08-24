// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"fmt"
	"slices"
	"testing"
	"time"

	memfs "github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/testutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testCommitOld     = "old"
	testCommitNew     = "new"
	testSpecsDirSPECS = "SPECS"
)

func classifyForTest(
	t *testing.T,
	name string,
	fromCommits, toCommits map[string]string,
) ChangedResult {
	t.Helper()

	toComponents := func(commits map[string]string) map[string]componentComparisonInputs {
		result := make(map[string]componentComparisonInputs, len(commits))
		for componentName, commit := range commits {
			config := projectconfig.ComponentConfig{
				Name: componentName,
				Spec: projectconfig.SpecSource{
					SourceType:     projectconfig.SpecSourceTypeUpstream,
					UpstreamCommit: commit,
				},
			}
			result[componentName] = componentComparisonInputs{
				Config:         normalizeComponentForComparison(config),
				SourceIdentity: commit,
			}
		}

		return result
	}

	result, err := classifyComponent(name, toComponents(fromCommits), toComponents(toCommits))
	require.NoError(t, err)

	return result
}

// testRepoCommit represents the files added or updated in a single commit.
// Files from previous commits that are not listed here remain unchanged;
// no deletions are modeled.
type testRepoCommit struct {
	files map[string][]byte
}

// testRepoWithCommits creates an in-memory git repo with N sequential commits.
// Returns the repo and a slice of commit hashes (oldest first).
func testRepoWithCommits(
	t *testing.T,
	commits []testRepoCommit,
) (*gogit.Repository, []string) {
	t.Helper()

	require.NotEmpty(t, commits, "need at least one commit")

	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	hashes := make([]string, 0, len(commits))

	for idx, commit := range commits {
		sig := &object.Signature{
			Name:  "Test",
			Email: "test@test.com",
			When:  baseTime.AddDate(0, idx, 0),
		}

		for path, content := range commit.files {
			f, fErr := memFS.Create(path)
			require.NoError(t, fErr)

			_, fErr = f.Write(content)
			require.NoError(t, fErr)
			require.NoError(t, f.Close())

			_, fErr = worktree.Add(path)
			require.NoError(t, fErr)
		}

		hash, commitErr := worktree.Commit(
			fmt.Sprintf("commit %d", idx),
			&gogit.CommitOptions{Author: sig, AllowEmptyCommits: true},
		)
		require.NoError(t, commitErr)

		hashes = append(hashes, hash.String())
	}

	return repo, hashes
}

// testRepoWithTwoCommits is a convenience wrapper around testRepoWithCommits.
func testRepoWithTwoCommits(
	t *testing.T,
	fromFiles, toFiles map[string][]byte,
) (*gogit.Repository, string, string) {
	t.Helper()

	repo, hashes := testRepoWithCommits(t, []testRepoCommit{
		{files: fromFiles},
		{files: toFiles},
	})

	return repo, hashes[0], hashes[1]
}

// --- classifyComponent tests ---

func TestClassifyComponent_Changed(t *testing.T) {
	fromCommits := map[string]string{"curl": testCommitOld}
	toCommits := map[string]string{"curl": testCommitNew}

	result := classifyForTest(t, "curl", fromCommits, toCommits)
	assert.Equal(t, changeTypeChanged, result.ChangeType)
}

func TestClassifyComponent_Unchanged(t *testing.T) {
	commits := map[string]string{"curl": "same"}

	result := classifyForTest(t, "curl", commits, commits)
	assert.Equal(t, changeTypeUnchanged, result.ChangeType)
}

func TestClassifyComponent_Added(t *testing.T) {
	fromCommits := map[string]string{}
	toCommits := map[string]string{"curl": testCommitNew}

	result := classifyForTest(t, "curl", fromCommits, toCommits)
	assert.Equal(t, changeTypeAdded, result.ChangeType)
}

func TestClassifyComponent_Deleted(t *testing.T) {
	fromCommits := map[string]string{"curl": testCommitOld}
	toCommits := map[string]string{}

	result := classifyForTest(t, "curl", fromCommits, toCommits)
	assert.Equal(t, changeTypeDeleted, result.ChangeType)
}

func TestClassifyComponent_NeverExisted(t *testing.T) {
	empty := map[string]string{}

	result := classifyForTest(t, "curl", empty, empty)
	assert.Equal(t, changeTypeUnchanged, result.ChangeType)
}

// --- compareSources tests ---

func TestCompareSources_Changed(t *testing.T) {
	repo, fromRef, toRef := testRepoWithTwoCommits(t,
		map[string][]byte{
			"SPECS/c/curl/sources": []byte("SHA512 (curl-8.0.tar.gz) = oldsha"),
		},
		map[string][]byte{
			"SPECS/c/curl/sources": []byte("SHA512 (curl-8.1.tar.gz) = newsha"),
		},
	)

	fromTree, err := resolveTree(repo, fromRef)
	require.NoError(t, err)

	toTree, err := resolveTree(repo, toRef)
	require.NoError(t, err)

	result, err := compareSources(
		fromTree, toTree, testSpecsDirSPECS, testSpecsDirSPECS, "curl",
	)
	require.NoError(t, err)
	assert.True(t, result)
}

func TestCompareSources_Unchanged(t *testing.T) {
	sourcesContent := []byte("SHA512 (curl-8.0.tar.gz) = samehash")

	repo, fromRef, toRef := testRepoWithTwoCommits(t,
		map[string][]byte{"SPECS/c/curl/sources": sourcesContent},
		map[string][]byte{"SPECS/c/curl/sources": sourcesContent},
	)

	fromTree, err := resolveTree(repo, fromRef)
	require.NoError(t, err)

	toTree, err := resolveTree(repo, toRef)
	require.NoError(t, err)

	result, err := compareSources(
		fromTree, toTree, testSpecsDirSPECS, testSpecsDirSPECS, "curl",
	)
	require.NoError(t, err)
	assert.False(t, result)
}

func TestCompareSources_Appeared(t *testing.T) {
	repo, fromRef, toRef := testRepoWithTwoCommits(t,
		map[string][]byte{"placeholder": []byte("x")},
		map[string][]byte{
			"placeholder":          []byte("x"),
			"SPECS/c/curl/sources": []byte("SHA512 (curl-8.0.tar.gz) = hash"),
		},
	)

	fromTree, err := resolveTree(repo, fromRef)
	require.NoError(t, err)

	toTree, err := resolveTree(repo, toRef)
	require.NoError(t, err)

	result, err := compareSources(
		fromTree, toTree, testSpecsDirSPECS, testSpecsDirSPECS, "curl",
	)
	require.NoError(t, err)
	assert.True(t, result, "sources file appeared")
}

func TestCompareSources_NoSourcesAtEitherRef(t *testing.T) {
	repo, fromRef, toRef := testRepoWithTwoCommits(t,
		map[string][]byte{"placeholder": []byte("x")},
		map[string][]byte{"placeholder": []byte("x")},
	)

	fromTree, err := resolveTree(repo, fromRef)
	require.NoError(t, err)

	toTree, err := resolveTree(repo, toRef)
	require.NoError(t, err)

	result, err := compareSources(
		fromTree, toTree, testSpecsDirSPECS, testSpecsDirSPECS, "curl",
	)
	require.NoError(t, err)
	assert.False(t, result, "no sources at either ref")
}

// --- Multi-component batch test ---

func TestMultiComponentBatch(t *testing.T) {
	curlFrom := "curl-v1"
	curlTo := "curl-v2"
	bashCommit := "bash-v1"
	sedFrom := "sed-v1"
	sedTo := "sed-v2"

	fromCommits := map[string]string{
		"curl": curlFrom,
		"bash": bashCommit,
		"sed":  sedFrom,
	}

	toCommits := map[string]string{
		"curl": curlTo,
		"bash": bashCommit,
		"sed":  sedTo,
	}

	curlResult := classifyForTest(t, "curl", fromCommits, toCommits)
	assert.Equal(t, changeTypeChanged, curlResult.ChangeType)

	bashResult := classifyForTest(t, "bash", fromCommits, toCommits)
	assert.Equal(t, changeTypeUnchanged, bashResult.ChangeType)

	sedResult := classifyForTest(t, "sed", fromCommits, toCommits)
	assert.Equal(t, changeTypeChanged, sedResult.ChangeType)
}

// --- Incremental updates test ---

func TestIncrementalUpdates(t *testing.T) {
	commits := []string{"aaa", "bbb", "ccc"}

	repo, hashes := testRepoWithCommits(t, []testRepoCommit{
		{files: map[string][]byte{
			"SPECS/c/curl/sources": []byte("SHA512 (curl-7.0.tar.gz) = old"),
		}},
		{files: map[string][]byte{
			"SPECS/c/curl/sources": []byte("SHA512 (curl-8.0.tar.gz) = mid"),
		}},
		{files: map[string][]byte{
			"SPECS/c/curl/sources": []byte("SHA512 (curl-8.1.tar.gz) = new"),
		}},
	})

	tests := []struct {
		name          string
		fromIdx       int
		toIdx         int
		changeType    string
		sourcesChange bool
	}{
		{"v1-v2", 0, 1, changeTypeChanged, true},
		{"v2-v3", 1, 2, changeTypeChanged, true},
		{"v1-v3 skip middle", 0, 2, changeTypeChanged, true},
		{"v2-v2 same ref", 1, 1, changeTypeUnchanged, false},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fromCommits := map[string]string{"curl": commits[testCase.fromIdx]}
			toCommits := map[string]string{"curl": commits[testCase.toIdx]}

			result := classifyForTest(t, "curl", fromCommits, toCommits)
			assert.Equal(t, testCase.changeType, result.ChangeType, "changeType")

			fromTree, treeErr := resolveTree(repo, hashes[testCase.fromIdx])
			require.NoError(t, treeErr)

			toTree, treeErr := resolveTree(repo, hashes[testCase.toIdx])
			require.NoError(t, treeErr)

			srcChange, srcErr := compareSources(
				fromTree, toTree, testSpecsDirSPECS, testSpecsDirSPECS, "curl",
			)
			require.NoError(t, srcErr)
			assert.Equal(t, testCase.sourcesChange, srcChange, "sourcesChange")
		})
	}
}

// --- Config-only change (rebuild without re-upload) ---

func TestConfigOnlyChange(t *testing.T) {
	fromCommits := map[string]string{"curl": "config-v1"}
	toCommits := map[string]string{"curl": "config-v2"}

	result := classifyForTest(t, "curl", fromCommits, toCommits)
	assert.Equal(t, changeTypeChanged, result.ChangeType, "upstream commit changed")
}

func TestClassifyComponent_BuildFieldChange(t *testing.T) {
	fromComponents := map[string]componentComparisonInputs{
		"curl": {
			Config: projectconfig.ComponentConfig{
				Build: projectconfig.ComponentBuildConfig{
					Defines: map[string]string{"feature": "disabled"},
				},
			},
		},
	}
	toComponents := map[string]componentComparisonInputs{
		"curl": {
			Config: projectconfig.ComponentConfig{
				Build: projectconfig.ComponentBuildConfig{
					Defines: map[string]string{"feature": "enabled"},
				},
			},
		},
	}

	result, err := classifyComponent("curl", fromComponents, toComponents)
	require.NoError(t, err)
	assert.Equal(t, changeTypeChanged, result.ChangeType)
}

func TestClassifyComponent_ExcludedFieldsUnchanged(t *testing.T) {
	base := projectconfig.ComponentConfig{
		Name:             "curl",
		SourceConfigFile: &projectconfig.ConfigFile{},
		RenderedSpecDir:  "/first/SPECS/c/curl",
		Spec: projectconfig.SpecSource{
			Path: "/first/specs/curl.spec",
			UpstreamDistro: projectconfig.DistroReference{
				Snapshot: "2025-01-01T00:00:00Z",
			},
		},
		Build: projectconfig.ComponentBuildConfig{
			Check: projectconfig.CheckConfig{Skip: true, SkipReason: "first reason"},
			Failure: projectconfig.ComponentBuildFailureConfig{
				Expected:       true,
				ExpectedReason: "first failure reason",
			},
			Hints: projectconfig.ComponentBuildHints{Expensive: true},
		},
		OverlayFiles: []string{"first/*.toml"},
		Publish:      projectconfig.ComponentPublishConfig{RPMChannel: "first"},
		Tests: &projectconfig.ComponentTestsConfig{
			Tests: []projectconfig.TestRef{{Name: "first-test"}},
		},
		Overlays: []projectconfig.ComponentOverlay{{
			Type:        projectconfig.ComponentOverlayAppendSpecLines,
			Description: "first description",
			Lines:       []string{"# functional content"},
			Source:      "/first/overlay.patch",
			Metadata:    &projectconfig.OverlayMetadata{},
		}},
		SourceFiles: []projectconfig.SourceFileReference{{
			Filename:      "source.tar.gz",
			Hash:          "abc123",
			Origin:        projectconfig.Origin{Type: projectconfig.OriginTypeURI, Uri: "https://first.example/source"},
			ReplaceReason: "first replacement reason",
		}},
		Packages: map[string]projectconfig.PackageConfig{
			"curl": {Publish: projectconfig.PackagePublishConfig{RPMChannel: "first"}},
		},
	}
	updated := base
	updated.Name = "renamed metadata"
	updated.SourceConfigFile = nil
	updated.RenderedSpecDir = "/second/SPECS/c/curl"
	updated.Spec.Path = "/second/specs/curl.spec"
	updated.Spec.UpstreamDistro.Snapshot = "2026-01-01T00:00:00Z"
	updated.Build.Check.SkipReason = "second reason"
	updated.Build.Failure.Expected = false
	updated.Build.Failure.ExpectedReason = "second failure reason"
	updated.Build.Hints.Expensive = false
	updated.OverlayFiles = []string{"second/*.toml"}
	updated.Publish.RPMChannel = "second"
	updated.Tests = &projectconfig.ComponentTestsConfig{
		Tests: []projectconfig.TestRef{{Name: "second-test"}},
	}
	updated.Overlays = slices.Clone(base.Overlays)
	updated.Overlays[0].Description = "second description"
	updated.Overlays[0].Source = "/second/overlay.patch"
	updated.Overlays[0].Metadata = nil
	updated.SourceFiles = slices.Clone(base.SourceFiles)
	updated.SourceFiles[0].Origin.Type = projectconfig.OriginTypeCustom
	updated.SourceFiles[0].Origin.Uri = "https://second.example/source"
	updated.SourceFiles[0].ReplaceReason = "second replacement reason"
	updated.Packages = map[string]projectconfig.PackageConfig{
		"curl": {Publish: projectconfig.PackagePublishConfig{RPMChannel: "second"}},
	}

	fromComponents := map[string]componentComparisonInputs{
		"curl": {Config: normalizeComponentForComparison(base)},
	}
	toComponents := map[string]componentComparisonInputs{
		"curl": {Config: normalizeComponentForComparison(updated)},
	}

	result, err := classifyComponent("curl", fromComponents, toComponents)
	require.NoError(t, err)
	assert.Equal(t, changeTypeUnchanged, result.ChangeType)
}

func TestBuildComponentComparisonInputs_ContentIdentities(t *testing.T) {
	testEnv := testutils.NewTestEnv(t)
	specPath := "/specs/curl/curl.spec"
	overlayPath := "/overlays/fix.patch"

	require.NoError(t, fileutils.WriteFile(
		testEnv.FS(), specPath, []byte("Version: 1\n"), fileperms.PublicFile,
	))
	require.NoError(t, fileutils.WriteFile(
		testEnv.FS(), overlayPath, []byte("first patch\n"), fileperms.PublicFile,
	))

	component := projectconfig.ComponentConfig{
		Name: "curl",
		Spec: projectconfig.SpecSource{
			SourceType: projectconfig.SpecSourceTypeLocal,
			Path:       specPath,
		},
		Overlays: []projectconfig.ComponentOverlay{{
			Type:     projectconfig.ComponentOverlayAddPatch,
			Filename: "fix.patch",
			Source:   overlayPath,
		}},
	}

	first, err := buildComponentComparisonInputs(testEnv.FS(), testEnv.Env, &component)
	require.NoError(t, err)
	assert.Empty(t, first.Config.Spec.Path)
	assert.Contains(t, first.SourceIdentity, "sha256:")
	assert.Regexp(t, `^fix\.patch:[0-9a-f]{64}$`, first.OverlaySourceHashes["0"])

	require.NoError(t, fileutils.WriteFile(
		testEnv.FS(), specPath, []byte("Version: 2\n"), fileperms.PublicFile,
	))
	second, err := buildComponentComparisonInputs(testEnv.FS(), testEnv.Env, &component)
	require.NoError(t, err)
	assert.NotEqual(t, first.SourceIdentity, second.SourceIdentity)
	assert.Equal(t, first.OverlaySourceHashes, second.OverlaySourceHashes)

	require.NoError(t, fileutils.WriteFile(
		testEnv.FS(), overlayPath, []byte("second patch\n"), fileperms.PublicFile,
	))
	third, err := buildComponentComparisonInputs(testEnv.FS(), testEnv.Env, &component)
	require.NoError(t, err)
	assert.Equal(t, second.SourceIdentity, third.SourceIdentity)
	assert.NotEqual(t, second.OverlaySourceHashes, third.OverlaySourceHashes)
}

func TestLoadHistoricalProject_UsesNormalIncludesAndMerging(t *testing.T) {
	rootConfig := []byte(`includes = [
    "config/components.toml",
    "config/upstream-commits/*.toml",
]

[project]
default-distro = { name = "testdistro", version = "1.0" }
rendered-specs-dir = "SPECS"

[distros.testdistro]
description = "Test distro"

[distros.testdistro.versions."1.0"]
release-ver = "1.0"

[component-groups.core]
components = ["curl"]
`)
	componentConfig := []byte(`[components.curl]
spec = {
    type = "upstream",
    upstream-distro = { name = "testdistro", version = "1.0" },
    upstream-name = "curl",
}
build = { defines = { feature = "enabled" } }
`)
	commitConfig := []byte(`# This file was generated by 'azldev component refresh-upstream-commit'
# Do not edit this file, changes will be lost
# For more details see 'azldev component refresh-upstream-commit --help'
[components.curl.spec]
upstream-commit = "abcdef1234567"
`)

	repo, hashes := testRepoWithCommits(t, []testRepoCommit{
		{files: map[string][]byte{
			"azldev.toml":                       rootConfig,
			"config/components.toml":            componentConfig,
			"config/upstream-commits/curl.toml": commitConfig,
		}},
	})

	tree, err := resolveTree(repo, hashes[0])
	require.NoError(t, err)

	testEnv := testutils.NewTestEnv(t)
	project, err := loadHistoricalProject(testEnv.Env, tree, ".")
	require.NoError(t, err)

	curlConfig, ok := project.components["curl"]
	require.True(t, ok)
	assert.Equal(t, "abcdef1234567", curlConfig.Spec.UpstreamCommit)
	assert.Equal(t, "enabled", curlConfig.Build.Defines["feature"])
	assert.Equal(t, "abcdef1234567", project.comparisonInputs["curl"].SourceIdentity)
	assert.Equal(t, "1.0", project.comparisonInputs["curl"].ReleaseVer)
	assert.Equal(t, "SPECS", project.renderedSpecsRelDir)
	assert.Equal(t, []string{"curl"}, project.componentGroups["core"])
}

func TestSelectHistoricalComponentNames_UsesBothRefs(t *testing.T) {
	fromProject := &historicalProject{
		components: map[string]projectconfig.ComponentConfig{
			"deleted": {Name: "deleted"},
			"shared":  {Name: "shared"},
		},
		componentGroups: map[string][]string{
			"historical": {"deleted"},
		},
	}
	toProject := &historicalProject{
		components: map[string]projectconfig.ComponentConfig{
			"added":  {Name: "added"},
			"shared": {Name: "shared"},
		},
		componentGroups: map[string][]string{
			"historical": {"added"},
		},
	}
	testEnv := testutils.NewTestEnv(t)

	allNames, err := selectHistoricalComponentNames(
		testEnv.Env,
		&components.ComponentFilter{IncludeAllComponents: true},
		fromProject,
		toProject,
		"/project",
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"added", "deleted", "shared"}, allNames)

	groupNames, err := selectHistoricalComponentNames(
		testEnv.Env,
		&components.ComponentFilter{ComponentGroupNames: []string{"historical"}},
		fromProject,
		toProject,
		"/project",
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"added", "deleted"}, groupNames)
}

func TestSelectHistoricalComponentNames_UsesHistoricalSpecPaths(t *testing.T) {
	fromProject := &historicalProject{
		components: map[string]projectconfig.ComponentConfig{
			"renamed": {
				Name: "renamed",
				Spec: projectconfig.SpecSource{Path: "/repo/project/specs/old.spec"},
			},
		},
	}
	toProject := &historicalProject{
		components: map[string]projectconfig.ComponentConfig{
			"renamed": {
				Name: "renamed",
				Spec: projectconfig.SpecSource{Path: "/repo/project/specs/new.spec"},
			},
		},
	}
	testEnv := testutils.NewTestEnv(t)

	names, err := selectHistoricalComponentNames(
		testEnv.Env,
		&components.ComponentFilter{SpecPaths: []string{"/project/specs/old.spec"}},
		fromProject,
		toProject,
		"/",
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"renamed"}, names)
}

// --- resolveTree / readFileFromTree / helpers ---

func TestResolveTree(t *testing.T) {
	repo, fromRef, _ := testRepoWithTwoCommits(t,
		map[string][]byte{"file.txt": []byte("hello")},
		map[string][]byte{"file.txt": []byte("world")},
	)

	tree, err := resolveTree(repo, fromRef)
	require.NoError(t, err)
	require.NotNil(t, tree)

	content, err := readFileFromTree(tree, "file.txt")
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), content)
}

func TestResolveTree_InvalidRef(t *testing.T) {
	repo, _, _ := testRepoWithTwoCommits(t,
		map[string][]byte{"file.txt": []byte("hello")},
		map[string][]byte{"file.txt": []byte("world")},
	)

	_, err := resolveCommitHash(repo, "nonexistent-ref")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolving ref")
}

func TestReadFileFromTree(t *testing.T) {
	repo, ref, _ := testRepoWithTwoCommits(t,
		map[string][]byte{"SPECS/c/curl/sources": []byte("SHA512 (file) = hash")},
		map[string][]byte{"SPECS/c/curl/sources": []byte("SHA512 (file) = hash")},
	)

	tree, err := resolveTree(repo, ref)
	require.NoError(t, err)

	content, err := readFileFromTree(tree, "SPECS/c/curl/sources")
	require.NoError(t, err)
	assert.Equal(t, []byte("SHA512 (file) = hash"), content)
}

func TestReadFileFromTree_Missing(t *testing.T) {
	repo, ref, _ := testRepoWithTwoCommits(t,
		map[string][]byte{"placeholder": []byte("x")},
		map[string][]byte{"placeholder": []byte("x")},
	)

	tree, err := resolveTree(repo, ref)
	require.NoError(t, err)

	_, err = readFileFromTree(tree, "nonexistent/path")
	require.Error(t, err)
}

func TestReadFileFromTreeSafe_NotFound(t *testing.T) {
	repo, ref, _ := testRepoWithTwoCommits(t,
		map[string][]byte{"placeholder": []byte("x")},
		map[string][]byte{"placeholder": []byte("x")},
	)

	tree, err := resolveTree(repo, ref)
	require.NoError(t, err)

	_, notFound, readErr := readFileFromTreeSafe(tree, "nonexistent/path")
	require.NoError(t, readErr)
	assert.True(t, notFound)
}

// --- classifyComponent table-driven ---

func TestClassifyComponent_TableDriven(t *testing.T) {
	commitA := "aaa"
	commitB := "bbb"

	tests := []struct {
		name           string
		fromCommits    map[string]string
		toCommits      map[string]string
		wantChangeType string
	}{
		{
			name:           "both present, upstream commit changed",
			fromCommits:    map[string]string{"curl": commitA},
			toCommits:      map[string]string{"curl": commitB},
			wantChangeType: changeTypeChanged,
		},
		{
			name:           "both present, upstream commit same",
			fromCommits:    map[string]string{"curl": commitA},
			toCommits:      map[string]string{"curl": commitA},
			wantChangeType: changeTypeUnchanged,
		},
		{
			name:           "added",
			fromCommits:    map[string]string{},
			toCommits:      map[string]string{"curl": commitA},
			wantChangeType: changeTypeAdded,
		},
		{
			name:           "deleted",
			fromCommits:    map[string]string{"curl": commitA},
			toCommits:      map[string]string{},
			wantChangeType: changeTypeDeleted,
		},
		{
			name:           "never existed",
			fromCommits:    map[string]string{},
			toCommits:      map[string]string{},
			wantChangeType: changeTypeUnchanged,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			result := classifyForTest(t, "curl", testCase.fromCommits, testCase.toCommits)
			assert.Equal(t, testCase.wantChangeType, result.ChangeType)
		})
	}
}
