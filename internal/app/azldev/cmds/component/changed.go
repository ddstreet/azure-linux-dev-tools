// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/providers/sourceproviders"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/git"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
)

// ChangedComponentOptions holds options for the component changed command.
type ChangedComponentOptions struct {
	ComponentFilter components.ComponentFilter
	// From is the git ref to compare from (e.g., branch, tag, commit hash).
	From string
	// To is the git ref to compare to. Defaults to HEAD.
	To string
	// IncludeUnchanged includes unchanged components in the output.
	IncludeUnchanged bool
}

func changedOnAppInit(_ *azldev.App, parentCmd *cobra.Command) {
	parentCmd.AddCommand(NewChangedCmd())
}

// NewChangedCmd constructs a [cobra.Command] for the "component changed" CLI subcommand.
func NewChangedCmd() *cobra.Command {
	options := &ChangedComponentOptions{}

	cmd := &cobra.Command{
		Use:   "changed",
		Short: "Detect which components changed between two git refs",
		Long: `Load the project configuration independently at two git refs and compare the
resolved component build inputs. Normal project TOML parsing is used at each
ref, including recursive includes, config merging, component defaults, and
generated upstream-commit TOMLs included by the project.

For each selected component, the command reports whether the remaining build
inputs represented by the former lock-file fingerprint changed: normalized
component configuration, upstream commit or local spec-directory contents,
overlay source filenames and contents, and effective distro release version.
The removed lock-file manual-bump input has no current equivalent.

Documentation, publishing, test-selection, scheduling-hint, snapshot-time, and
checkout-path-only fields do not trigger a component change. The sourcesChange
field separately compares the raw committed sources manifests at the rendered
spec directories configured by each ref. All-component scans use the union of
components in both refs, so added and deleted components are reported without
consulting the current checkout.

This is useful for CI/CD pipelines to determine which components need to be
rebuilt or have their lookaside tarballs re-uploaded after a PR merge.`,
		Example: `  # Show changed components between a branch and HEAD
  azldev component changed --from main -a

  # Show changes between two specific refs
  azldev component changed --from v1.0 --to v2.0 -a

  # Include unchanged components in output
  azldev component changed --from main -a --include-unchanged

  # JSON output for scripting
  azldev component changed --from main -a -q -O json`,
		RunE: azldev.RunFuncWithExtraArgs(func(env *azldev.Env, args []string) (interface{}, error) {
			options.ComponentFilter.ComponentNamePatterns = append(args, options.ComponentFilter.ComponentNamePatterns...)

			return ChangedComponents(env, options)
		}),
		ValidArgsFunction: components.GenerateComponentNameCompletions,
	}

	components.AddComponentFilterOptionsToCommand(cmd, &options.ComponentFilter)

	cmd.Flags().StringVar(&options.From, "from", "", "Git ref to compare from (required)")
	cmd.Flags().StringVar(&options.To, "to", "HEAD", "Git ref to compare to")
	cmd.Flags().BoolVar(&options.IncludeUnchanged, "include-unchanged", false,
		"Include unchanged components in output (only applies to broad -a scans; explicit selections always show status)")

	_ = cmd.MarkFlagRequired("from")

	azldev.ExportAsReadOnlyMCPTool(cmd)

	return cmd
}

// ChangedResult holds the change status for a single component.
type ChangedResult struct {
	Component     string `json:"component"`
	ChangeType    string `json:"changeType"`
	SourcesChange bool   `json:"sourcesChange"`
}

// Change type constants for [ChangedResult.ChangeType].
const (
	changeTypeAdded     = "added"
	changeTypeChanged   = "changed"
	changeTypeUnchanged = "unchanged"
	changeTypeDeleted   = "deleted"
)

// ChangedComponents loads and compares resolved project components and rendered
// sources between two git refs.
func ChangedComponents(
	env *azldev.Env, options *ChangedComponentOptions,
) ([]ChangedResult, error) {
	ctx, err := newChangedContext(env)
	if err != nil {
		return nil, err
	}

	fromHash, err := resolveCommitHash(ctx.repo, options.From)
	if err != nil {
		return nil, fmt.Errorf("resolving --from ref %#q:\n%w", options.From, err)
	}

	toHash, err := resolveCommitHash(ctx.repo, options.To)
	if err != nil {
		return nil, fmt.Errorf("resolving --to ref %#q:\n%w", options.To, err)
	}

	projectRelDir, err := repoRelPath(ctx.repoRoot, env.ProjectDir())
	if err != nil {
		return nil, fmt.Errorf("resolving project directory within repository:\n%w", err)
	}

	fromTree, err := resolveTree(ctx.repo, fromHash)
	if err != nil {
		return nil, fmt.Errorf("resolving tree for --from:\n%w", err)
	}

	toTree, err := resolveTree(ctx.repo, toHash)
	if err != nil {
		return nil, fmt.Errorf("resolving tree for --to:\n%w", err)
	}

	fromProject, err := loadHistoricalProject(env, fromTree, projectRelDir)
	if err != nil {
		return nil, fmt.Errorf("loading project at --from:\n%w", err)
	}

	toProject, err := loadHistoricalProject(env, toTree, projectRelDir)
	if err != nil {
		return nil, fmt.Errorf("loading project at --to:\n%w", err)
	}

	names, err := selectHistoricalComponentNames(
		env, &options.ComponentFilter, fromProject, toProject, ctx.repoRoot,
	)
	if err != nil {
		return nil, fmt.Errorf("selecting components:\n%w", err)
	}

	return buildResults(
		names, fromProject, toProject, fromTree, toTree,
		options.IncludeUnchanged, options.ComponentFilter.IncludeAllComponents,
	)
}

// changedContext holds resolved repo state for change detection.
type changedContext struct {
	repo     *gogit.Repository
	repoRoot string
}

// newChangedContext opens the project repository and resolves paths.
func newChangedContext(env *azldev.Env) (*changedContext, error) {
	repo, err := git.OpenProjectRepo(env.ProjectDir())
	if err != nil {
		return nil, fmt.Errorf("opening project repository:\n%w", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("getting project worktree:\n%w", err)
	}

	repoRoot := worktree.Filesystem.Root()

	return &changedContext{
		repo:     repo,
		repoRoot: repoRoot,
	}, nil
}

const (
	snapshotRepoRoot = "/repo"
	snapshotTempDir  = "/config-tmp"
)

type fixedFileSystemFactory struct {
	fs opctx.FS
}

func (factory *fixedFileSystemFactory) FS() opctx.FS {
	return factory.fs
}

// historicalProject contains only data that remains valid after the temporary
// in-memory checkout used to resolve it is discarded.
type historicalProject struct {
	components          map[string]projectconfig.ComponentConfig
	comparisonInputs    map[string]componentComparisonInputs
	componentGroups     map[string][]string
	renderedSpecsRelDir string
}

// componentComparisonInputs mirrors the build-relevant inputs covered by the
// input fingerprint on the main branch.
type componentComparisonInputs struct {
	Config              projectconfig.ComponentConfig `json:"config"`
	SourceIdentity      string                        `json:"sourceIdentity,omitempty"`
	OverlaySourceHashes map[string]string             `json:"overlaySourceHashes,omitempty"`
	ReleaseVer          string                        `json:"releaseVer,omitempty"`
}

func loadHistoricalProject(
	env *azldev.Env,
	tree *object.Tree,
	projectRelDir string,
) (*historicalProject, error) {
	snapshotFS := afero.NewMemMapFs()

	if err := copyTreeToFS(tree, snapshotFS, snapshotRepoRoot); err != nil {
		return nil, fmt.Errorf("materializing git tree:\n%w", err)
	}

	if err := fileutils.MkdirAll(snapshotFS, snapshotTempDir); err != nil {
		return nil, fmt.Errorf("creating temporary config directory:\n%w", err)
	}

	projectDir := filepath.Join(snapshotRepoRoot, projectRelDir)

	loadedProjectDir, config, err := projectconfig.LoadProjectConfig(
		snapshotFS,
		env.OSEnv(),
		projectDir,
		false,
		snapshotTempDir,
		nil,
		env.PermissiveConfigParsing(),
	)
	if err != nil {
		return nil, fmt.Errorf("parsing project configuration:\n%w", err)
	}

	historicalEnv := azldev.NewEnv(env.Context(), azldev.EnvOptions{
		ProjectDir: loadedProjectDir,
		Config:     config,
		Interfaces: azldev.SystemInterfaces{
			FileSystemFactory: &fixedFileSystemFactory{fs: snapshotFS},
		},
	})
	historicalEnv.SetPermissiveConfigParsing(env.PermissiveConfigParsing())

	resolver := components.NewResolver(historicalEnv)

	resolvedComponents, err := resolver.FindAllComponents()
	if err != nil {
		return nil, fmt.Errorf("resolving components:\n%w", err)
	}

	result := &historicalProject{
		components:       make(map[string]projectconfig.ComponentConfig, resolvedComponents.Len()),
		comparisonInputs: make(map[string]componentComparisonInputs, resolvedComponents.Len()),
		componentGroups:  make(map[string][]string, len(config.ComponentGroups)),
	}

	if err := populateHistoricalComponents(
		result, snapshotFS, historicalEnv, resolvedComponents,
	); err != nil {
		return nil, err
	}

	if err := populateHistoricalGroups(result, resolver, config); err != nil {
		return nil, err
	}

	result.renderedSpecsRelDir, err = repoRelPath(snapshotRepoRoot, config.Project.RenderedSpecsDir)
	if err != nil {
		return nil, fmt.Errorf("resolving rendered specs directory:\n%w", err)
	}

	return result, nil
}

func populateHistoricalGroups(
	project *historicalProject,
	resolver *components.Resolver,
	config *projectconfig.ProjectConfig,
) error {
	for groupName := range config.ComponentGroups {
		memberNames := make(map[string]bool)
		for _, memberName := range config.ComponentGroups[groupName].Components {
			memberNames[memberName] = true
		}

		group, groupErr := resolver.GetComponentGroupByName(groupName)
		if groupErr != nil {
			return fmt.Errorf("resolving component group %#q:\n%w", groupName, groupErr)
		}

		for _, member := range group.Components {
			memberNames[member.ComponentName] = true
		}

		project.componentGroups[groupName] = sortedComponentNames(memberNames)
	}

	return nil
}

func populateHistoricalComponents(
	project *historicalProject,
	fs opctx.FS,
	env *azldev.Env,
	resolvedComponents *components.ComponentSet,
) error {
	for _, component := range resolvedComponents.Components() {
		project.components[component.GetName()] = *component.GetConfig()

		inputs, err := buildComponentComparisonInputs(fs, env, component.GetConfig())
		if err != nil {
			return fmt.Errorf(
				"building change inputs for component %#q:\n%w",
				component.GetName(), err,
			)
		}

		project.comparisonInputs[component.GetName()] = inputs
	}

	return nil
}

func buildComponentComparisonInputs(
	fs opctx.FS,
	env *azldev.Env,
	component *projectconfig.ComponentConfig,
) (componentComparisonInputs, error) {
	inputs := componentComparisonInputs{
		Config:         normalizeComponentForComparison(*component),
		SourceIdentity: component.EffectiveUpstreamCommit(),
	}

	if component.Spec.SourceType == projectconfig.SpecSourceTypeLocal {
		identity, err := sourceproviders.ResolveLocalSourceIdentity(
			fs, filepath.Dir(component.Spec.Path),
		)
		if err != nil {
			return inputs, fmt.Errorf("resolving local source identity:\n%w", err)
		}

		inputs.SourceIdentity = identity
	}

	ref := component.Spec.UpstreamDistro
	if ref.Name == "" {
		ref = env.Config().Project.DefaultDistro
	}

	if ref.Name != "" {
		_, distroVersion, err := env.ResolveDistroRef(ref)
		if err != nil {
			return inputs, fmt.Errorf("resolving distro reference %#q:\n%w", ref.Name, err)
		}

		inputs.ReleaseVer = distroVersion.ReleaseVer
	}

	for idx, overlay := range component.Overlays {
		sourceName := overlay.EffectiveSourceName()
		if sourceName == "" {
			continue
		}

		contentHash, err := fileutils.ComputeFileHash(
			fs, fileutils.HashTypeSHA256, overlay.Source,
		)
		if err != nil {
			return inputs, fmt.Errorf("hashing overlay source %#q:\n%w", overlay.Source, err)
		}

		if inputs.OverlaySourceHashes == nil {
			inputs.OverlaySourceHashes = make(map[string]string)
		}

		inputs.OverlaySourceHashes[strconv.Itoa(idx)] = sourceName + ":" + contentHash
	}

	return inputs, nil
}

// normalizeComponentForComparison removes the same non-build fields that the
// main branch excluded from component input fingerprints.
func normalizeComponentForComparison(component projectconfig.ComponentConfig) projectconfig.ComponentConfig {
	normalized := component
	normalized.Name = ""
	normalized.SourceConfigFile = nil
	normalized.RenderedSpecDir = ""
	normalized.Spec.Path = ""
	normalized.Spec.UpstreamDistro.Snapshot = ""
	normalized.Build.Check.SkipReason = ""
	normalized.Build.Failure = projectconfig.ComponentBuildFailureConfig{}
	normalized.Build.Hints = projectconfig.ComponentBuildHints{}
	normalized.OverlayFiles = nil
	normalized.Publish = projectconfig.ComponentPublishConfig{}
	normalized.Tests = nil

	normalized.Overlays = slices.Clone(component.Overlays)
	for idx := range normalized.Overlays {
		normalized.Overlays[idx].Description = ""
		normalized.Overlays[idx].Source = ""
		normalized.Overlays[idx].Metadata = nil
	}

	normalized.SourceFiles = slices.Clone(component.SourceFiles)
	for idx := range normalized.SourceFiles {
		normalized.SourceFiles[idx].Origin.Type = ""
		normalized.SourceFiles[idx].Origin.Uri = ""
		normalized.SourceFiles[idx].ReplaceReason = ""
	}

	if component.Packages != nil {
		normalized.Packages = make(map[string]projectconfig.PackageConfig, len(component.Packages))
		for name, pkg := range component.Packages {
			pkg.Publish = projectconfig.PackagePublishConfig{}
			normalized.Packages[name] = pkg
		}
	}

	return normalized
}

func copyTreeToFS(tree *object.Tree, fs opctx.FS, destinationRoot string) error {
	files := tree.Files()

	err := files.ForEach(func(file *object.File) error {
		content, err := file.Contents()
		if err != nil {
			return fmt.Errorf("reading %#q:\n%w", file.Name, err)
		}

		destinationPath := filepath.Join(destinationRoot, filepath.FromSlash(file.Name))
		if err := fileutils.MkdirAll(fs, filepath.Dir(destinationPath)); err != nil {
			return fmt.Errorf("creating parent directory for %#q:\n%w", file.Name, err)
		}

		if err := fileutils.WriteFile(
			fs, destinationPath, []byte(content), fileperms.PublicFile,
		); err != nil {
			return fmt.Errorf("writing %#q:\n%w", file.Name, err)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("iterating git tree files:\n%w", err)
	}

	return nil
}

func selectHistoricalComponentNames(
	env *azldev.Env,
	filter *components.ComponentFilter,
	fromProject, toProject *historicalProject,
	repoRoot string,
) ([]string, error) {
	allNames := make(map[string]bool, len(fromProject.components)+len(toProject.components))
	for name := range fromProject.components {
		allNames[name] = true
	}

	for name := range toProject.components {
		allNames[name] = true
	}

	if filter.HasNoCriteria() {
		slog.Warn("No component selection options were given, no components will be selected.")

		return []string{}, nil
	}

	selected := make(map[string]bool)

	if filter.IncludeAllComponents {
		for name := range allNames {
			selected[name] = true
		}

		return sortedComponentNames(selected), nil
	}

	if err := addPatternSelections(
		selected, allNames, filter.ComponentNamePatterns,
	); err != nil {
		return nil, err
	}

	if err := addGroupSelections(
		selected, fromProject, toProject, filter.ComponentGroupNames,
	); err != nil {
		return nil, err
	}

	if err := addSpecPathSelections(
		env, selected, fromProject, toProject, repoRoot, filter.SpecPaths,
	); err != nil {
		return nil, err
	}

	return sortedComponentNames(selected), nil
}

func addPatternSelections(
	selected, allNames map[string]bool,
	patterns []string,
) error {
	for _, pattern := range patterns {
		matched := false

		for name := range allNames {
			isMatch, err := filepath.Match(pattern, name)
			if err != nil {
				return fmt.Errorf("comparing component pattern %#q:\n%w", pattern, err)
			}

			if isMatch {
				selected[name] = true
				matched = true
			}
		}

		if !matched && !strings.ContainsAny(pattern, "*?[") {
			return fmt.Errorf("component not found: %#q", pattern)
		}
	}

	return nil
}

func addGroupSelections(
	selected map[string]bool,
	fromProject, toProject *historicalProject,
	groupNames []string,
) error {
	for _, groupName := range groupNames {
		fromMembers, inFrom := fromProject.componentGroups[groupName]

		toMembers, inTo := toProject.componentGroups[groupName]
		if !inFrom && !inTo {
			return fmt.Errorf("%w: %#q", components.ErrComponentGroupNotFound, groupName)
		}

		for _, name := range append(fromMembers, toMembers...) {
			selected[name] = true
		}
	}

	return nil
}

func addSpecPathSelections(
	env *azldev.Env,
	selected map[string]bool,
	fromProject, toProject *historicalProject,
	repoRoot string,
	specPaths []string,
) error {
	for _, specPath := range specPaths {
		specRelPath, err := projectSpecRepoRelPath(env, repoRoot, specPath)
		if err != nil {
			return err
		}

		snapshotSpecPath := filepath.Join(snapshotRepoRoot, specRelPath)
		matched := false

		for _, project := range []*historicalProject{fromProject, toProject} {
			for name, component := range project.components {
				if filepath.Clean(component.Spec.Path) == snapshotSpecPath {
					selected[name] = true
					matched = true
				}
			}
		}

		if !matched {
			return fmt.Errorf("component not found for spec path %#q", specPath)
		}
	}

	return nil
}

func projectSpecRepoRelPath(env *azldev.Env, repoRoot, specPath string) (string, error) {
	absolutePath := specPath
	if !filepath.IsAbs(absolutePath) {
		absolutePath = filepath.Join(env.ProjectDir(), absolutePath)
	}

	relativePath, err := repoRelPath(repoRoot, absolutePath)
	if err != nil {
		return "", fmt.Errorf("resolving spec path %#q:\n%w", specPath, err)
	}

	return relativePath, nil
}

func sortedComponentNames(names map[string]bool) []string {
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}

	sort.Strings(result)

	return result
}

func buildResults(
	names []string,
	fromProject, toProject *historicalProject,
	fromTree, toTree *object.Tree,
	includeUnchanged, includeAllComponents bool,
) ([]ChangedResult, error) {
	results := make([]ChangedResult, 0, len(names))

	for _, name := range names {
		result, err := classifyComponent(
			name, fromProject.comparisonInputs, toProject.comparisonInputs,
		)
		if err != nil {
			return nil, fmt.Errorf("comparing component %#q:\n%w", name, err)
		}

		result.SourcesChange, err = compareSources(
			fromTree,
			toTree,
			fromProject.renderedSpecsRelDir,
			toProject.renderedSpecsRelDir,
			name,
		)
		if err != nil {
			return nil, fmt.Errorf("comparing sources for %#q:\n%w", name, err)
		}

		if includeAllComponents &&
			!includeUnchanged &&
			result.ChangeType == changeTypeUnchanged &&
			!result.SourcesChange {
			continue
		}

		results = append(results, result)
	}

	return results, nil
}

// classifyComponent compares the serialized build-relevant inputs for a
// component at two refs.
func classifyComponent(
	name string,
	fromComponents, toComponents map[string]componentComparisonInputs,
) (ChangedResult, error) {
	result := ChangedResult{
		Component:  name,
		ChangeType: changeTypeUnchanged,
	}

	fromComponent, inFrom := fromComponents[name]
	toComponent, inTo := toComponents[name]

	switch {
	case !inFrom && !inTo:
		result.ChangeType = changeTypeUnchanged
	case !inFrom:
		result.ChangeType = changeTypeAdded
	case !inTo:
		result.ChangeType = changeTypeDeleted
	default:
		fromJSON, err := json.Marshal(fromComponent)
		if err != nil {
			return result, fmt.Errorf("serializing component at --from:\n%w", err)
		}

		toJSON, err := json.Marshal(toComponent)
		if err != nil {
			return result, fmt.Errorf("serializing component at --to:\n%w", err)
		}

		if !bytes.Equal(fromJSON, toJSON) {
			result.ChangeType = changeTypeChanged
		}
	}

	return result, nil
}

// compareSources compares the rendered sources file between two git trees.
func compareSources(
	fromTree, toTree *object.Tree,
	fromRenderedSpecsRelDir, toRenderedSpecsRelDir, name string,
) (bool, error) {
	fromRenderedDir, err := components.RenderedSpecDir(fromRenderedSpecsRelDir, name)
	if err != nil {
		return false, fmt.Errorf("resolving rendered spec dir at --from:\n%w", err)
	}

	toRenderedDir, err := components.RenderedSpecDir(toRenderedSpecsRelDir, name)
	if err != nil {
		return false, fmt.Errorf("resolving rendered spec dir at --to:\n%w", err)
	}

	fromSourcesPath := filepath.Join(fromRenderedDir, "sources")
	toSourcesPath := filepath.Join(toRenderedDir, "sources")

	fromSources, fromNotFound, fromErr := readFileFromTreeSafe(fromTree, fromSourcesPath)
	toSources, toNotFound, toErr := readFileFromTreeSafe(toTree, toSourcesPath)

	if fromErr != nil {
		return false, fmt.Errorf("reading sources at --from:\n%w", fromErr)
	}

	if toErr != nil {
		return false, fmt.Errorf("reading sources at --to:\n%w", toErr)
	}

	switch {
	case fromNotFound && toNotFound:
		return false, nil
	case fromNotFound || toNotFound:
		return true, nil
	default:
		return !bytes.Equal(fromSources, toSources), nil
	}
}

// repoRelPath computes a repo-relative path and rejects `..`-prefixed results
// that would escape the repository root.
func repoRelPath(repoRoot, absPath string) (string, error) {
	relPath, err := filepath.Rel(repoRoot, absPath)
	if err != nil {
		return "", fmt.Errorf("computing relative path from %#q to %#q:\n%w", repoRoot, absPath, err)
	}

	if relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %#q escapes repository root %#q", absPath, repoRoot)
	}

	return relPath, nil
}

// isFileNotFound returns true if the error indicates a missing file or
// directory in a git tree.
func isFileNotFound(err error) bool {
	return errors.Is(err, object.ErrFileNotFound) ||
		errors.Is(err, object.ErrDirectoryNotFound) ||
		errors.Is(err, object.ErrEntryNotFound)
}

// readFileFromTreeSafe reads a file from a git tree, distinguishing
// file-not-found from other errors.
func readFileFromTreeSafe(
	tree *object.Tree, relPath string,
) ([]byte, bool, error) {
	content, err := readFileFromTree(tree, relPath)
	if err != nil {
		if isFileNotFound(err) {
			return nil, true, nil
		}

		return nil, false, err
	}

	return content, false, nil
}

// resolveCommitHash resolves a git ref string to a commit hash string.
func resolveCommitHash(repo *gogit.Repository, ref string) (string, error) {
	hash, err := repo.ResolveRevision(plumbing.Revision(ref))
	if err != nil {
		return "", fmt.Errorf("resolving ref %#q:\n%w", ref, err)
	}

	return hash.String(), nil
}

// resolveTree resolves a commit hash to a tree object.
func resolveTree(repo *gogit.Repository, commitHash string) (*object.Tree, error) {
	hash := plumbing.NewHash(commitHash)

	commit, err := repo.CommitObject(hash)
	if err != nil {
		return nil, fmt.Errorf("reading commit %#q:\n%w", commitHash, err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("reading tree for commit %#q:\n%w", commitHash, err)
	}

	return tree, nil
}

// readFileFromTree reads a file's contents from a git tree object.
func readFileFromTree(tree *object.Tree, relPath string) ([]byte, error) {
	file, err := tree.File(relPath)
	if err != nil {
		return nil, fmt.Errorf("reading %#q from tree:\n%w", relPath, err)
	}

	content, err := file.Contents()
	if err != nil {
		return nil, fmt.Errorf("reading contents of %#q:\n%w", relPath, err)
	}

	return []byte(content), nil
}
