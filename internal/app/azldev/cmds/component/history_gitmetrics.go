// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/sources"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/git"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/parmap"
)

// historyContext holds resolved repo state shared across components.
type historyContext struct {
	repoRoot string
}

// newHistoryContext opens the project repository once just to resolve the
// worktree root, then discards it: go-git's *Repository is not safe to
// share across goroutines (see synthistory.go which always opens a fresh
// repo per component). Per-worker repos are reopened inline.
func newHistoryContext(env *azldev.Env) (*historyContext, error) {
	cfg := env.Config()
	if cfg == nil {
		return nil, errors.New("no project configuration loaded")
	}

	repo, err := git.OpenProjectRepo(env.ProjectDir())
	if err != nil {
		return nil, fmt.Errorf("opening project repository:\n%w", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("getting project worktree:\n%w", err)
	}

	return &historyContext{
		repoRoot: worktree.Filesystem.Root(),
	}, nil
}

// countTomlSharing returns the number of components that point at each
// source TOML path. Used to detect shared files (where toml-commit counts
// are coarse).
func countTomlSharing(allComponents map[string]projectconfig.ComponentConfig) map[string]int {
	sharing := make(map[string]int)

	for _, cfg := range allComponents {
		if cfg.SourceConfigFile == nil {
			continue
		}

		path := cfg.SourceConfigFile.SourcePath()
		if path == "" {
			continue
		}

		sharing[path]++
	}

	return sharing
}

// tomlMetrics is one entry in the precomputed cache populated by
// [precomputeTomlMetrics]. Keyed by repo-relative TOML path. A non-nil err
// records a real `git log` failure so [populateTomlMetrics] can surface a
// warning, keeping it distinguishable from a genuine zero-commit history.
type tomlMetrics struct {
	count  int
	latest time.Time
	err    error
}

// precomputeTomlMetricsForStubs runs `git log` once per *unique* source-
// TOML path across the selected stubs and returns the results keyed by
// repo-relative path. This is the central performance optimization: in
// real projects (e.g., azurelinux) thousands of components share a single
// components.toml file, and without de-duplicating we'd re-run the same
// `git log` thousands of times.
//
// Paths that resolve outside the repo are skipped. A `git log` failure is
// cached as a per-path error (not a fatal one) so [populateTomlMetrics] can
// surface a warning, keeping it distinguishable from a genuine zero-commit
// history.
func precomputeTomlMetricsForStubs(
	workerEnv *azldev.Env,
	env *azldev.Env,
	ctx *historyContext,
	stubs []historyStub,
) (map[string]tomlMetrics, error) {
	uniqueRelPaths := collectUniqueTomlRelPathsFromStubs(ctx.repoRoot, stubs)
	if len(uniqueRelPaths) == 0 {
		return map[string]tomlMetrics{}, nil
	}

	progressEvent := env.StartEvent("Counting TOML commit history", "uniqueFiles", len(uniqueRelPaths))
	defer progressEvent.End()

	total := int64(len(uniqueRelPaths))

	parmapResults := parmap.Map(
		workerEnv,
		env.IOBoundConcurrency(),
		uniqueRelPaths,
		func(done, _ int) { progressEvent.SetProgress(int64(done), total) },
		func(_ context.Context, relPath string) tomlMetrics {
			count, latest, err := git.CountCommitsTouchingFile( //nolint:contextcheck // env carries the ctx
				workerEnv, workerEnv, ctx.repoRoot, relPath,
			)
			if err != nil {
				// Cache the failure rather than failing the whole command --
				// populateTomlMetrics surfaces it as a warning so a real error
				// (corrupt repo, permission denied) stays distinguishable from a
				// genuine zero-commit history.
				return tomlMetrics{err: err}
			}

			return tomlMetrics{count: count, latest: latest}
		},
	)

	cache := make(map[string]tomlMetrics, len(uniqueRelPaths))

	for idx, parmapRes := range parmapResults {
		if parmapRes.Cancelled {
			continue
		}

		cache[uniqueRelPaths[idx]] = parmapRes.Value
	}

	return cache, nil
}

// collectUniqueTomlRelPathsFromStubs returns the deduplicated set of in-
// repo, repo-relative source-TOML paths across the given stubs.
func collectUniqueTomlRelPathsFromStubs(repoRoot string, stubs []historyStub) []string {
	seen := make(map[string]struct{})

	relPaths := make([]string, 0)

	for _, stub := range stubs {
		config := stub.component.GetConfig()
		if config.SourceConfigFile == nil {
			continue
		}

		absPath := config.SourceConfigFile.SourcePath()
		if absPath == "" {
			continue
		}

		relPath, err := repoRelPath(repoRoot, absPath)
		if err != nil {
			continue
		}

		if _, dup := seen[relPath]; dup {
			continue
		}

		seen[relPath] = struct{}{}

		relPaths = append(relPaths, relPath)
	}

	return relPaths
}

// buildHistoryResult assembles a single [HistoryResult] for a stub. The
// stub already carries the precomputed customization items; this function
// fills in the git-driven metrics.
func buildHistoryResult(
	env *azldev.Env,
	stub historyStub,
	ctx *historyContext,
	tomlSharing map[string]int,
	tomlCache map[string]tomlMetrics,
	sharedMode string,
	explicit bool,
) HistoryResult {
	result := HistoryResult{
		Name:               stub.component.GetName(),
		CustomizationItems: stub.customizationItems,
		Customizations:     len(stub.customizationItems),
	}

	populateTomlMetrics(stub.component, ctx, tomlSharing, tomlCache, sharedMode, explicit, &result)
	populateUpstreamCommitMetrics(env, stub.component, ctx, &result)

	return result
}

// populateTomlMetrics fills in TomlCommits, SharedToml, TomlPath,
// LatestCommit from the precomputed [tomlMetrics] cache.
func populateTomlMetrics(
	comp components.Component,
	ctx *historyContext,
	tomlSharing map[string]int,
	tomlCache map[string]tomlMetrics,
	sharedMode string,
	explicit bool,
	result *HistoryResult,
) {
	config := comp.GetConfig()

	if config.SourceConfigFile == nil || config.SourceConfigFile.SourcePath() == "" {
		return
	}

	tomlAbsPath := config.SourceConfigFile.SourcePath()
	result.SharedToml = tomlSharing[tomlAbsPath] > 1

	tomlRelPath, err := repoRelPath(ctx.repoRoot, tomlAbsPath)
	if err != nil {
		// A TOML file outside the repo isn't a hard error -- record a
		// warning and leave path/commit counts empty.
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("source TOML %q is outside the git repository; toml-commits skipped: %v",
				tomlAbsPath, err))

		return
	}

	result.TomlPath = tomlRelPath

	// --shared=omit suppresses the (coarse) count for shared TOMLs, but an
	// explicitly-named component is the user asking for that component
	// specifically -- give them the real count, mirroring the row-keep
	// override in [ComponentHistory].
	if result.SharedToml && sharedMode == sharedTomlModeOmit && !explicit {
		return
	}

	metrics, ok := tomlCache[tomlRelPath]
	if !ok {
		// Precompute didn't run for this path (e.g., out-of-repo TOML
		// or a precompute failure that was tolerated). Surface so the
		// user can tell zero-counts apart from missing-data.
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("no TOML commit metrics cached for %q; toml-commits left at zero", tomlRelPath))

		return
	}

	if metrics.err != nil {
		// A real `git log` failure was cached during precompute. Surface it
		// rather than silently reporting zero commits.
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("counting TOML commits for %q failed; toml-commits left at zero: %v",
				tomlRelPath, metrics.err))

		return
	}

	result.TomlCommits = metrics.count
	result.LatestCommit = metrics.latest
}

// populateUpstreamCommitMetrics fills in the generated commit TOML history.
// Details are always populated here; the caller strips them
// when more than one component is reported. See [ComponentHistory] for the
// rationale.
func populateUpstreamCommitMetrics(
	env *azldev.Env,
	comp components.Component,
	ctx *historyContext,
	result *HistoryResult,
) {
	name := comp.GetName()

	config := comp.GetConfig()

	upstreamCommitConfigFile := config.UpstreamCommitConfigFile()
	if upstreamCommitConfigFile == nil || upstreamCommitConfigFile.SourcePath() == "" ||
		config.Spec.UpstreamCommit == "" {
		return
	}

	configPath := upstreamCommitConfigFile.SourcePath()

	configRelPath, err := repoRelPath(ctx.repoRoot, configPath)
	if err != nil {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("upstream commit TOML %q is outside the git repository; changes skipped: %v",
				configPath, err))

		return
	}

	changes, err := func() ([]sources.UpstreamCommitChange, error) {
		// Open a fresh repo for this call -- go-git's *Repository is not
		// safe for concurrent use. Opening is cheap (just reads .git/config).
		repo, openErr := git.OpenProjectRepo(env.ProjectDir())
		if openErr != nil {
			return nil, fmt.Errorf("opening project repository:\n%w", openErr)
		}

		return sources.FindUpstreamCommitChanges(
			env.Context(), env, repo, ctx.repoRoot, configRelPath, name,
		)
	}()
	if err != nil {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("computing upstream commit changes for %q: %v", configRelPath, err))

		return
	}

	result.UpstreamCommitChanges = len(changes)
	result.UpstreamCommitChangeDetails = toUpstreamCommitChanges(changes)
}

// toUpstreamCommitChanges copies each source change into the wire type by
// naming every field explicitly. Removing a field from
// [sources.UpstreamCommitChange] or
// [sources.CommitMetadata] trips a compile error here, alerting us to a
// quietly-shrunk changelog payload.
func toUpstreamCommitChanges(
	changes []sources.UpstreamCommitChange,
) []UpstreamCommitChange {
	if len(changes) == 0 {
		return nil
	}

	out := make([]UpstreamCommitChange, len(changes))
	for i, change := range changes {
		out[i] = UpstreamCommitChange{
			Hash:           change.Hash,
			Author:         change.Author,
			AuthorEmail:    change.AuthorEmail,
			Timestamp:      change.Timestamp,
			Message:        change.Message,
			UpstreamCommit: change.UpstreamCommit,
		}
	}

	return out
}
