// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/providers/sourceproviders"
	"github.com/microsoft/azure-linux-dev-tools/internal/upstreamcommit"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/parmap"
	"github.com/spf13/cobra"
)

// UpdateComponentOptions holds options for the component update command.
type UpdateComponentOptions struct {
	ComponentFilter components.ComponentFilter
	// UpstreamCommitsDir is the project-relative or absolute directory containing
	// generated per-component TOML files.
	UpstreamCommitsDir string
	// CheckOnly resolves upstream commits but does not write TOML files or
	// prune orphans.
	CheckOnly bool
}

const defaultUpstreamCommitsDir = "base/upstream-commits"

func updateOnAppInit(_ *azldev.App, parentCmd *cobra.Command) {
	parentCmd.AddCommand(NewUpdateCmd())
}

// NewUpdateCmd constructs a [cobra.Command] for the "component update" CLI subcommand.
func NewUpdateCmd() *cobra.Command {
	options := &UpdateComponentOptions{}
	options.UpstreamCommitsDir = defaultUpstreamCommitsDir

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Resolve and record upstream commits for components",
		Long: `Resolve upstream commits for components and write normal per-component TOML configuration.

For upstream components, this resolves the effective commit hash using the
distro snapshot time, then records it as spec.upstream-commit in
base/upstream-commits/<name>.toml by default. Include that directory's TOML
files before component-specific TOML configuration so subsequent commands use
the generated commit unless the component configuration explicitly overrides
it.

All TOML configuration is resolved before the source type is checked. Selected
components whose effective spec.type is not "upstream" do not contact an
upstream provider, and any generated upstream-commit TOML for them is removed.

When updating all components (-a), orphan generated TOML files are
automatically pruned.
Orphan pruning is skipped when updating individual components to avoid
accidentally removing files for components not included in the filter.

The --check-only flag runs the full pipeline but does NOT write TOML files or
prune orphans. The command exits 0 when nothing would change and exits 1 when
any component is stale or any generated TOML would be pruned. Intended for CI gates.`,
		Example: `  # Update all components
  azldev component update -a

  # Update a single component
  azldev component update -p curl

  # Update components in a group
  azldev component update -g core

  # Write generated files to a custom directory
  azldev component update -a --upstream-commits-dir config/commits

  # CI gate: exit 0 if commit TOMLs are current, 1 if anything would change
  azldev component update -a --check-only -q`,
		RunE: azldev.RunFuncWithExtraArgs(func(env *azldev.Env, args []string) (interface{}, error) {
			options.ComponentFilter.ComponentNamePatterns = append(
				args, options.ComponentFilter.ComponentNamePatterns...,
			)

			return UpdateComponents(env, options)
		}),
		ValidArgsFunction: components.GenerateComponentNameCompletions,
	}

	components.AddComponentFilterOptionsToCommand(cmd, &options.ComponentFilter)

	cmd.Flags().StringVar(&options.UpstreamCommitsDir, "upstream-commits-dir",
		defaultUpstreamCommitsDir,
		"directory for generated per-component upstream-commit TOML files")
	_ = cmd.MarkFlagDirname("upstream-commits-dir")
	cmd.Flags().BoolVar(&options.CheckOnly, "check-only", false,
		"resolve upstream commits but do not write TOML files or prune orphans. "+
			"Exits 0 when nothing would change and 1 when any component is stale "+
			"(or, with --all-components, when any orphan generated TOML "+
			"would be pruned). Intended for CI gates")

	return cmd
}

// UpdateResult is the per-component output for the update command.
type UpdateResult struct {
	Component      string `json:"component"                table:",sortkey"`
	UpstreamCommit string `json:"upstreamCommit,omitempty"`
	PreviousCommit string `json:"previousCommit,omitempty" table:"-"`
	Changed        bool   `json:"changed"`
	Removed        bool   `json:"removed,omitempty"        table:",omitempty"`
	Skipped        bool   `json:"skipped,omitempty"`
	SkipReason     string `json:"skipReason,omitempty"     table:",omitempty"`
	Error          string `json:"error,omitempty"          table:",omitempty"`
}

// UpdateComponents resolves upstream commits for all selected components and
// writes the results to per-component TOML files.
func UpdateComponents(env *azldev.Env, options *UpdateComponentOptions) ([]UpdateResult, error) {
	resolver := components.NewResolver(env)

	resolved, err := resolver.FindComponents(&options.ComponentFilter)
	if err != nil {
		return nil, fmt.Errorf("resolving components:\n%w", err)
	}

	allComps := resolved.Components()
	if len(allComps) == 0 && !options.ComponentFilter.IncludeAllComponents {
		return nil, errors.New("no components matched the filter")
	}

	if env.ProjectDir() == "" {
		return nil, errors.New("no project directory configured; cannot update upstream commit TOML files")
	}

	commitDir := options.UpstreamCommitsDir
	if commitDir == "" {
		commitDir = defaultUpstreamCommitsDir
	}

	if !filepath.IsAbs(commitDir) {
		commitDir = filepath.Join(env.ProjectDir(), commitDir)
	}

	store := upstreamcommit.NewStore(env.FS(), commitDir)

	// Configuration is fully parsed and merged before deciding whether each
	// selected component has an upstream identity. Non-upstream components
	// never contact a provider; an existing generated pin is marked for
	// removal instead.
	upstreamComps, results, inspectErr := inspectSelectedComponents(allComps, store)
	if inspectErr != nil {
		return results, inspectErr
	}

	results = append(results, resolveUpstreamCommitsParallel(env, upstreamComps, store)...)

	// Don't save if the context was cancelled (Ctrl+C).
	if env.Context().Err() != nil {
		return results, errors.New("update cancelled; upstream commit TOML files not updated")
	}

	// Check results and bail on errors before saving.
	if err := checkUpdateErrors(results); err != nil {
		return filterDisplayResults(results), err
	}

	// Write per-component TOML files only on full success.
	if err := saveUpstreamCommitConfigs(store, results, options.CheckOnly); err != nil {
		return results, err
	}

	// Skipped in --check-only mode -- the "changed" counter would lie about a
	// run that wrote nothing, and the structured error returned below already
	// names every affected component.
	if !options.CheckOnly {
		logUpdateSummary(results)
	}

	// Prune orphan generated TOML files when updating all components.
	// Use the resolved component set (not raw config) to include
	// spec-glob-discovered components that aren't in config directly.
	// Generated TOMLs are version controlled, so pruning is safe even if the
	// resolved set is empty (e.g., all components removed from config).
	wouldPrune, orphanErr := handleOrphanConfigs(store, allComps, options)
	if orphanErr != nil {
		return filterDisplayResults(results), orphanErr
	}

	if options.CheckOnly {
		wouldPrune = excludePendingRemovals(wouldPrune, results)

		return checkOnlyResult(results, wouldPrune)
	}

	// Filter results for table output: show changed and skipped components.
	return filterDisplayResults(results), nil
}

func inspectSelectedComponents(
	comps []components.Component,
	store *upstreamcommit.Store,
) ([]components.Component, []UpdateResult, error) {
	upstream := make([]components.Component, 0, len(comps))

	var results []UpdateResult

	for _, comp := range comps {
		if comp.GetConfig().Spec.SourceType == projectconfig.SpecSourceTypeUpstream {
			upstream = append(upstream, comp)

			continue
		}

		exists, err := store.Exists(comp.GetName())
		if err != nil {
			return nil, results, fmt.Errorf(
				"checking generated upstream commit TOML for non-upstream component %#q:\n%w",
				comp.GetName(), err,
			)
		}

		if exists {
			results = append(results, UpdateResult{
				Component: comp.GetName(),
				Changed:   true,
				Removed:   true,
			})
		}
	}

	return upstream, results, nil
}

func excludePendingRemovals(orphans []string, results []UpdateResult) []string {
	removed := make(map[string]struct{})

	for idx := range results {
		if results[idx].Removed {
			removed[results[idx].Component] = struct{}{}
		}
	}

	filtered := make([]string, 0, len(orphans))
	for _, orphan := range orphans {
		if _, found := removed[orphan]; !found {
			filtered = append(filtered, orphan)
		}
	}

	return filtered
}

// handleOrphanConfigs reconciles the generated TOML directory with the resolved
// component set. In normal mode it deletes orphan files; in --check-only
// mode it returns the list of orphans that would be deleted without
// touching disk. Returns (nil, nil) when not running with --all-components,
// since orphan handling is scoped to whole-set updates.
func handleOrphanConfigs(
	store *upstreamcommit.Store,
	comps []components.Component,
	options *UpdateComponentOptions,
) ([]string, error) {
	if !options.ComponentFilter.IncludeAllComponents {
		return nil, nil
	}

	if len(comps) == 0 {
		if options.CheckOnly {
			slog.Warn("No components resolved; all generated upstream commit TOMLs would be treated as orphans")
		} else {
			slog.Warn("No components resolved; all generated upstream commit TOMLs will be treated as orphans")
		}
	}

	resolvedNames := make(map[string]projectconfig.ComponentConfig, len(comps))
	for _, comp := range comps {
		resolvedNames[comp.GetName()] = *comp.GetConfig()
	}

	if options.CheckOnly {
		orphans, findErr := store.FindOrphans(resolvedNames)
		if findErr != nil {
			return nil, fmt.Errorf("finding orphan upstream commit TOMLs:\n%w", findErr)
		}

		return orphans, nil
	}

	pruned, pruneErr := store.PruneOrphans(resolvedNames)
	if pruneErr != nil {
		return nil, fmt.Errorf("pruning orphan upstream commit TOMLs:\n%w", pruneErr)
	}

	if pruned > 0 {
		slog.Info("Pruned orphan upstream commit TOMLs", "count", pruned)
	}

	return nil, nil
}

// checkOnlyResult inspects the results of a --check-only update run and
// returns (results, error) when any component would change or any generated TOML
// would be pruned. The error names the affected components so CI logs are
// useful at a glance. Returns (results, nil) when nothing would change --
// the caller exits 0. Results are returned in both cases so structured
// consumers (e.g. -O json) retain the per-component data the pipeline just
// computed.
func checkOnlyResult(
	results []UpdateResult, wouldPrune []string,
) ([]UpdateResult, error) {
	var changed []string

	for idx := range results {
		if results[idx].Changed {
			changed = append(changed, results[idx].Component)
		}
	}

	display := filterDisplayResults(results)

	if len(changed) == 0 && len(wouldPrune) == 0 {
		return display, nil
	}

	var parts []string
	if len(changed) > 0 {
		parts = append(parts, fmt.Sprintf("%d component(s) would change: %s",
			len(changed), strings.Join(changed, ", ")))
	}

	if len(wouldPrune) > 0 {
		parts = append(parts, fmt.Sprintf("%d orphan upstream commit TOML file(s) would be pruned: %s",
			len(wouldPrune), strings.Join(wouldPrune, ", ")))
	}

	return display, fmt.Errorf("upstream commit TOML files are stale; %s. Run 'azldev component update -a' to refresh",
		strings.Join(parts, "; "))
}

// saveUpstreamCommitConfigs writes TOML files for changed upstream commits.
func saveUpstreamCommitConfigs(
	store *upstreamcommit.Store, results []UpdateResult, checkOnly bool,
) error {
	saved := make([]string, 0, len(results))

	// Log partially-saved components on any error so the user knows which
	// TOML files were written before the failure.
	var retErr error

	defer func() {
		if retErr != nil && len(saved) > 0 {
			slog.Info("Upstream commit TOMLs saved before failure", "components", saved)
		}
	}()

	for idx := range results {
		if results[idx].Error != "" || results[idx].Skipped {
			continue
		}

		written, err := updateComponentConfig(store, &results[idx], checkOnly)
		if err != nil {
			retErr = err

			return retErr
		}

		if written {
			saved = append(saved, results[idx].Component)
		}
	}

	return nil
}

// updateComponentConfig writes one changed TOML file. The returned 'written'
// flag is always false in check-only mode.
func updateComponentConfig(
	store *upstreamcommit.Store, result *UpdateResult, checkOnly bool,
) (bool, error) {
	if !result.Changed {
		return false, nil
	}

	// In check-only mode the caller wants to know what *would* change without
	// touching disk. Skip the write but keep result.Changed flipped so the
	// caller can build the user-visible diff list.
	if checkOnly {
		return false, nil
	}

	if result.Removed {
		removed, removeErr := store.Remove(result.Component)
		if removeErr != nil {
			return false, fmt.Errorf(
				"removing upstream commit TOML for non-upstream component %#q:\n%w",
				result.Component, removeErr,
			)
		}

		return removed, nil
	}

	if saveErr := store.Save(result.Component, result.UpstreamCommit); saveErr != nil {
		return false, fmt.Errorf("saving upstream commit TOML for %#q:\n%w", result.Component, saveErr)
	}

	return true, nil
}

// checkUpdateErrors returns an error if any component failed to resolve.
// Does NOT log a summary — call [logUpdateSummary] after saves are complete.
func checkUpdateErrors(results []UpdateResult) error {
	var failedNames []string

	for idx := range results {
		if results[idx].Error != "" {
			failedNames = append(failedNames, results[idx].Component)
		}
	}

	if len(failedNames) > 0 {
		slog.Error("Update failed",
			"total", len(results),
			"errors", len(failedNames))

		return fmt.Errorf(
			"%d component(s) failed to resolve; upstream commit TOML files not updated:\n  %s",
			len(failedNames), strings.Join(failedNames, "\n  "))
	}

	return nil
}

// logUpdateSummary logs the final update summary.
func logUpdateSummary(results []UpdateResult) {
	var changed, skipped, upToDate int

	for idx := range results {
		switch {
		case results[idx].Skipped:
			skipped++
		case results[idx].Changed:
			changed++
		default:
			upToDate++
		}
	}

	slog.Info("Update complete",
		"total", len(results),
		"changed", changed,
		"upToDate", upToDate,
		"skipped", skipped)
}

// filterDisplayResults returns changed, skipped, and errored results for table
// display. Up-to-date components (not Changed, not Skipped, no Error) are
// excluded — they represent the common "nothing to do" case and would dominate
// the output. Errored entries are kept so the user can see what failed when
// the command exits non-zero via the partial-results-on-error path.
func filterDisplayResults(results []UpdateResult) []UpdateResult {
	var tableResults []UpdateResult

	for idx := range results {
		if results[idx].Changed || results[idx].Skipped || results[idx].Error != "" {
			tableResults = append(tableResults, results[idx])
		}
	}

	return tableResults
}

func resolveUpstreamCommitsParallel(
	env *azldev.Env,
	comps []components.Component,
	store *upstreamcommit.Store,
) []UpdateResult {
	results := make([]UpdateResult, len(comps))

	progressEvent := env.StartEvent("Resolving upstream commits", "count", len(comps))
	defer progressEvent.End()

	workerEnv, cancel := env.WithCancel()
	defer cancel()

	// Resolve every selected upstream component instead of inferring freshness
	// from duplicated metadata. The provider is the authority for the commit
	// selected by the snapshot, and that result is compared directly with the
	// generated TOML before deciding whether a write is needed.
	parallel := make([]parallelItem, len(comps))
	for idx, comp := range comps {
		results[idx].Component = comp.GetName()
		parallel[idx] = parallelItem{idx: idx, comp: comp}
	}

	// Each resolution may involve network I/O, so we parallelize.
	parmapResults := parmap.Map(
		workerEnv,
		env.FastConcurrency(),
		parallel,
		func(done, _ int) {
			progressEvent.SetProgress(int64(done), int64(len(comps)))
		},
		func(ctx context.Context, item parallelItem) struct{} {
			resolveAndRecordCommit(ctx, workerEnv, cancel, item.comp, store, &results[item.idx])

			return struct{}{}
		},
	)

	// Items that never acquired a worker slot (ctx cancelled mid-flight) get
	// marked Skipped — matches the legacy semaphore-select behaviour.
	for i, pr := range parmapResults {
		if pr.Cancelled {
			idx := parallel[i].idx
			results[idx].Skipped = true
			results[idx].SkipReason = "cancelled"
		}
	}

	return results
}

// parallelItem pairs a component with its result index for parmap workers.
type parallelItem struct {
	idx  int
	comp components.Component
}

// resolveAndRecordCommit resolves one component's upstream commit.
func resolveAndRecordCommit(
	ctx context.Context,
	env *azldev.Env,
	cancel context.CancelFunc,
	comp components.Component,
	store *upstreamcommit.Store,
	result *UpdateResult,
) {
	// Clear the loaded pin before asking the provider to resolve. Render and
	// build honor Spec.UpstreamCommit for reproducibility, but update is the
	// operation that advances that pin: leaving the old value in place would
	// make the provider return it immediately and a newer snapshot could never
	// move the component forward.
	comp.GetConfig().Spec.UpstreamCommit = ""

	commit, resolveErr := resolveUpstreamCommit(ctx, env, comp)
	if resolveErr != nil {
		result.Error = resolveErr.Error()

		// Cancel remaining goroutines on first real failure.
		cancel()

		return
	}

	result.UpstreamCommit = commit

	checkConfigChanged(store, comp.GetName(), result)
}

// checkConfigChanged compares the resolved commit with the generated TOML.
func checkConfigChanged(store *upstreamcommit.Store, componentName string, result *UpdateResult) {
	existingCommit, exists, loadErr := store.Get(componentName)
	if loadErr != nil {
		result.Error = fmt.Sprintf("loading upstream commit TOML: %v", loadErr)

		return
	}

	if !exists {
		result.Changed = true

		return
	}

	result.PreviousCommit = existingCommit
	result.Changed = existingCommit != result.UpstreamCommit
}

func resolveUpstreamCommit(
	ctx context.Context,
	env *azldev.Env,
	comp components.Component,
) (string, error) {
	componentName := comp.GetName()

	distro, err := sourceproviders.ResolveDistro(env, comp)
	if err != nil {
		return "", fmt.Errorf("resolving distro for %#q:\n%w", componentName, err)
	}

	sourceManager, err := sourceproviders.NewSourceManager(env, distro)
	if err != nil {
		return "", fmt.Errorf("creating source manager for %#q:\n%w", componentName, err)
	}

	commit, err := sourceManager.ResolveSourceIdentity(ctx, comp)
	if err != nil {
		return "", fmt.Errorf("resolving upstream commit for %#q:\n%w", componentName, err)
	}

	slog.Debug("Resolved upstream commit", "component", componentName, "commit", commit)

	return commit, nil
}
