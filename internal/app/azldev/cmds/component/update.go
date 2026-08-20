// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/lockfile"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/providers/sourceproviders"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/parmap"
	"github.com/spf13/cobra"
)

// UpdateComponentOptions holds options for the component update command.
type UpdateComponentOptions struct {
	ComponentFilter components.ComponentFilter
	// CheckOnly resolves upstream commits but does not write lock files or
	// prune orphans. Returns a non-nil error when any component would change
	// or any lock file would be pruned. Intended for CI gates:
	// `azldev component update -a --check-only` exits 0 when locks are
	// fresh and 1 when something is stale.
	CheckOnly bool
}

func updateOnAppInit(_ *azldev.App, parentCmd *cobra.Command) {
	parentCmd.AddCommand(NewUpdateCmd())
}

// NewUpdateCmd constructs a [cobra.Command] for the "component update" CLI subcommand.
func NewUpdateCmd() *cobra.Command {
	options := &UpdateComponentOptions{}

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Resolve and lock source identities for components",
		Long: `Resolve upstream commits for components and write them to per-component lock files.

For upstream components, this resolves the effective commit hash using the
distro snapshot time or explicit pin, then records it in locks/<name>.lock.
Subsequent commands (render, build) use the locked state for deterministic,
reproducible results.

When updating all components (-a), orphan lock files (locks for components
that no longer exist in the project config) are automatically pruned.
Orphan pruning is skipped when updating individual components to avoid
accidentally removing lock files for components not included in the filter.

The --check-only flag runs the full pipeline but does NOT write lock files or
prune orphans. The command exits 0 when nothing would change and exits 1 when
any component is stale or any lock would be pruned. Intended for CI gates.`,
		Example: `  # Update all components
  azldev component update -a

  # Update a single component
  azldev component update -p curl

  # Update components in a group
  azldev component update -g core

  # CI gate: exit 0 if locks are fresh, 1 if anything would change
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

	cmd.Flags().BoolVar(&options.CheckOnly, "check-only", false,
		"resolve upstream commits but do not write lock files or prune orphans. "+
			"Exits 0 when nothing would change and 1 when any component is stale "+
			"(or, with --all-components, when any orphan lock "+
			"would be pruned). Intended for CI gates")
	// Update always skips lock validation (it's the lock writer), so the
	// flag is meaningless here. Hide it to avoid confusion.
	_ = cmd.Flags().MarkHidden("skip-lock-validation")

	return cmd
}

// UpdateResult is the per-component output for the update command.
type UpdateResult struct {
	Component      string `json:"component"                table:",sortkey"`
	UpstreamCommit string `json:"upstreamCommit,omitempty"`
	PreviousCommit string `json:"previousCommit,omitempty" table:"-"`
	Changed        bool   `json:"changed"`
	Skipped        bool   `json:"skipped,omitempty"`
	SkipReason     string `json:"skipReason,omitempty"     table:",omitempty"`
	Error          string `json:"error,omitempty"          table:",omitempty"`
}

// UpdateComponents resolves upstream commits for all selected components and
// writes the results to per-component lock files under locks/.
// Lock validation is always skipped regardless of the caller's SkipLockValidation
// value — update is the lock writer.
func UpdateComponents(env *azldev.Env, options *UpdateComponentOptions) ([]UpdateResult, error) {
	resolver := components.NewResolver(env)
	// Suppress staleness warnings — we're about to refresh the locks ourselves,
	// so warning the user to "run component update" would be self-referential noise.
	resolver.SuppressLockWarnings = true
	// Skip lock validation — update is the lock file writer, so missing or
	// stale locks are expected and will be fixed by this command.
	options.ComponentFilter.SkipLockValidation = true

	resolved, err := resolver.FindComponents(&options.ComponentFilter)
	if err != nil {
		return nil, fmt.Errorf("resolving components:\n%w", err)
	}

	allComps := resolved.Components()
	if len(allComps) == 0 && !options.ComponentFilter.IncludeAllComponents {
		return nil, errors.New("no components matched the filter")
	}

	upstreamComps := make([]components.Component, 0, len(allComps))
	for _, comp := range allComps {
		// Locks now represent only an upstream commit, so local components
		// have no state for update to persist. Their filesystem content is
		// consumed directly by render and build instead of being summarized
		// into a lock fingerprint.
		if comp.GetConfig().Spec.SourceType == projectconfig.SpecSourceTypeUpstream {
			upstreamComps = append(upstreamComps, comp)
		}
	}

	store := env.LockStore()
	if store == nil {
		return nil, errors.New("no project directory configured; cannot update lock files")
	}

	results := resolveUpstreamCommitsParallel(env, upstreamComps, store)

	// Don't save if the context was cancelled (Ctrl+C).
	if env.Context().Err() != nil {
		return results, errors.New("update cancelled; lock files not updated")
	}

	// Check results and bail on errors before saving.
	if err := checkUpdateErrors(results); err != nil {
		return results, err
	}

	// Write per-component lock files only on full success.
	if err := saveComponentLocks(store, results, options.CheckOnly); err != nil {
		return results, err
	}

	// Skipped in --check-only mode -- the "changed" counter would lie about a
	// run that wrote nothing, and the structured error returned below already
	// names every affected component.
	if !options.CheckOnly {
		logUpdateSummary(results)
	}

	// Prune orphan lock files when updating all components.
	// Use the resolved component set (not raw config) to include
	// spec-glob-discovered components that aren't in config directly.
	// Lock files are version controlled, so pruning is safe even if the
	// resolved set is empty (e.g., all components removed from config).
	wouldPrune, orphanErr := handleOrphanLocks(store, allComps, options)
	if orphanErr != nil {
		return results, orphanErr
	}

	if options.CheckOnly {
		return checkOnlyResult(results, wouldPrune)
	}

	// Filter results for table output: show changed and skipped components.
	return filterDisplayResults(results), nil
}

// handleOrphanLocks reconciles the lockfile directory with the resolved
// component set. In normal mode it deletes orphan locks; in --check-only
// mode it returns the list of orphans that would be deleted without
// touching disk. Returns (nil, nil) when not running with --all-components,
// since orphan handling is scoped to whole-set updates.
func handleOrphanLocks(
	store *lockfile.Store,
	comps []components.Component,
	options *UpdateComponentOptions,
) ([]string, error) {
	if !options.ComponentFilter.IncludeAllComponents {
		return nil, nil
	}

	if len(comps) == 0 {
		if options.CheckOnly {
			slog.Warn("No components resolved; all existing lock files would be treated as orphans")
		} else {
			slog.Warn("No components resolved; all existing lock files will be treated as orphans")
		}
	}

	resolvedNames := make(map[string]projectconfig.ComponentConfig, len(comps))
	for _, comp := range comps {
		resolvedNames[comp.GetName()] = *comp.GetConfig()
	}

	if options.CheckOnly {
		orphans, findErr := store.FindOrphanLockFiles(resolvedNames)
		if findErr != nil {
			return nil, fmt.Errorf("finding orphan lock files:\n%w", findErr)
		}

		return orphans, nil
	}

	pruned, pruneErr := store.PruneOrphans(resolvedNames)
	if pruneErr != nil {
		return nil, fmt.Errorf("pruning orphan lock files:\n%w", pruneErr)
	}

	if pruned > 0 {
		slog.Info("Pruned orphan lock files", "count", pruned)
	}

	return nil, nil
}

// checkOnlyResult inspects the results of a --check-only update run and
// returns (results, error) when any component would change or any lock file
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
		parts = append(parts, fmt.Sprintf("%d orphan lock file(s) would be pruned: %s",
			len(wouldPrune), strings.Join(wouldPrune, ", ")))
	}

	return display, fmt.Errorf("lock files are stale; %s. Run 'azldev component update -a' to refresh",
		strings.Join(parts, "; "))
}

// saveComponentLocks writes lock files for changed upstream commits.
func saveComponentLocks(store *lockfile.Store, results []UpdateResult, checkOnly bool) error {
	saved := make([]string, 0, len(results))

	// Log partially-saved components on any error so the user knows which
	// lock files were written before the failure.
	var retErr error

	defer func() {
		if retErr != nil && len(saved) > 0 {
			slog.Info("Lock files saved before failure", "components", saved)
		}
	}()

	for idx := range results {
		if results[idx].Error != "" || results[idx].Skipped {
			continue
		}

		written, err := updateComponentLock(store, &results[idx], checkOnly)
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

// updateComponentLock writes one changed lock file. The returned 'written'
// flag is always false in check-only mode.
func updateComponentLock(
	store *lockfile.Store, result *UpdateResult, checkOnly bool,
) (bool, error) {
	if !result.Changed {
		return false, nil
	}

	lock, lockErr := store.GetOrNew(result.Component)
	if lockErr != nil {
		return false, fmt.Errorf("loading lock for %#q:\n%w", result.Component, lockErr)
	}

	lock.UpstreamCommit = result.UpstreamCommit

	// In check-only mode the caller wants to know what *would* change without
	// touching disk. Skip the write but keep result.Changed flipped so the
	// caller can build the user-visible diff list.
	if checkOnly {
		return false, nil
	}

	if saveErr := store.Save(result.Component, lock); saveErr != nil {
		return false, fmt.Errorf("saving lock file for %#q:\n%w", result.Component, saveErr)
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
			"%d component(s) failed to resolve; lock files not updated:\n  %s",
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

// filterDisplayResults returns changed and skipped results for table display.
// Up-to-date components (not Changed, not Skipped) are excluded — they
// represent the common "nothing to do" case and would dominate the output.
func filterDisplayResults(results []UpdateResult) []UpdateResult {
	var tableResults []UpdateResult

	for idx := range results {
		if results[idx].Changed || results[idx].Skipped {
			tableResults = append(tableResults, results[idx])
		}
	}

	return tableResults
}

func resolveUpstreamCommitsParallel(
	env *azldev.Env,
	comps []components.Component,
	store *lockfile.Store,
) []UpdateResult {
	results := make([]UpdateResult, len(comps))

	progressEvent := env.StartEvent("Resolving upstream commits", "count", len(comps))
	defer progressEvent.End()

	workerEnv, cancel := env.WithCancel()
	defer cancel()

	// Resolve every selected component instead of inferring freshness from a
	// hash of snapshot, distro, and pin inputs stored in the lock. Removing that
	// duplicate resolution state makes update behavior direct: the provider is
	// the authority for the current identity, and the resulting identity is
	// compared with the lock before deciding whether to write.
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
	store *lockfile.Store,
	result *UpdateResult,
) {
	// Drop populated lock data so the source provider re-resolves
	// from upstream instead of short-circuiting with the locked value.
	comp.GetConfig().Locked = nil

	commit, resolveErr := resolveUpstreamCommit(ctx, env, comp)
	if resolveErr != nil {
		result.Error = resolveErr.Error()

		// Cancel remaining goroutines on first real failure.
		cancel()

		return
	}

	result.UpstreamCommit = commit

	// Check existing lock to determine if the component changed.
	checkLockChanged(store, comp.GetName(), result)
}

// checkLockChanged compares the resolved upstream commit against the existing
// lock file to determine if the component changed. For new components (no lock
// file), marks as Changed unconditionally. For existing locks, compares
// UpstreamCommit values.
func checkLockChanged(store *lockfile.Store, componentName string, result *UpdateResult) {
	exists, existsErr := store.Exists(componentName)
	if existsErr != nil {
		result.Error = fmt.Sprintf("checking lock: %v", existsErr)

		return
	}

	if !exists {
		result.Changed = true

		return
	}

	existingLock, loadErr := store.Get(componentName)
	if loadErr != nil {
		result.Error = fmt.Sprintf("loading lock: %v", loadErr)

		return
	}

	result.PreviousCommit = existingLock.UpstreamCommit
	result.Changed = existingLock.UpstreamCommit != result.UpstreamCommit
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
