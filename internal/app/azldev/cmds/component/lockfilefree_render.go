// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/sources"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/rpm/spec"
	"github.com/microsoft/azure-linux-dev-tools/internal/rpm/spectool"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	gitutils "github.com/microsoft/azure-linux-dev-tools/internal/utils/git"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/parmap"
)

const (
	changelogFilename       = "changelog"
	noChangeRebuildFilename = ".no_change_rebuild"
	rpmautospecProcessedTag = "RPMAUTOSPEC: autorelease, autochangelog"
	rpmspecChangelogFormat  = "[* %{CHANGELOGTIME:date} %{CHANGELOGNAME}\n%{CHANGELOGTEXT}\n\n]"
	rpmspecNoChangelogTrim  = "_changelog_trimage 0"
	autoreleaseShellInput   = "%autorelease -n\n"
)

var (
	changelogSectionPattern = regexp.MustCompile(`(?m)^%changelog(?:\s.*)?$`)
	autoreleaseBasePattern  = regexp.MustCompile(`(?:^|\s)-b\s+[^}\s]+`)
	releaseNumberPattern    = regexp.MustCompile(`^\d+$`)
)

type protectedSpecFields struct {
	release      string
	changelog    []byte
	hasChangelog bool
}

type lockfileFreeRenderState struct {
	commitMessage string
	protected     protectedSpecFields
}

func lockfileFreeComponentUnchanged(
	env *azldev.Env,
	comp components.Component,
	distGitDir string,
) (bool, error) {
	if !env.WithoutLockfile() {
		return false, nil
	}

	existing, err := fileutils.DirExists(env.FS(), distGitDir)
	if err != nil {
		return false, fmt.Errorf("checking existing dist-git directory %#q:\n%w",
			distGitDir, err)
	}

	if !existing {
		return false, nil
	}

	repo, repoRoot, err := openChangedRepo(env)
	if err != nil {
		return false, err
	}

	distGitRelPath, err := projectRelativePath(repoRoot, distGitDir, "dist-git")
	if err != nil {
		return false, err
	}

	renderedCommit, err := gitutils.RunInDir(
		env,
		env,
		repoRoot,
		"log",
		"-1",
		"--format=%H",
		"--",
		distGitRelPath,
	)
	if err != nil {
		return false, fmt.Errorf("finding latest local dist-git change:\n%w", err)
	}

	if renderedCommit == "" {
		return false, nil
	}

	currentCommit, err := resolveCommitHash(repo, "HEAD")
	if err != nil {
		return false, fmt.Errorf("resolving current project commit:\n%w", err)
	}

	projectRelDir, err := repoRelPath(repoRoot, env.ProjectDir())
	if err != nil {
		return false, fmt.Errorf("resolving project directory within repository:\n%w", err)
	}

	return componentUnchangedAtCommits(
		env,
		repo,
		projectRelDir,
		comp.GetName(),
		renderedCommit,
		currentCommit,
	)
}

func componentUnchangedAtCommits(
	env *azldev.Env,
	repo *gogit.Repository,
	projectRelDir string,
	componentName string,
	renderedCommit string,
	currentCommit string,
) (bool, error) {
	renderedTree, err := resolveTree(repo, renderedCommit)
	if err != nil {
		return false, fmt.Errorf("resolving last rendered project tree:\n%w", err)
	}

	currentTree, err := resolveTree(repo, currentCommit)
	if err != nil {
		return false, fmt.Errorf("resolving current project tree:\n%w", err)
	}

	renderedProject, err := loadHistoricalProject(env, renderedTree, projectRelDir)
	if err != nil {
		return false, fmt.Errorf("loading project at last rendered commit:\n%w", err)
	}

	currentProject, err := loadHistoricalProject(env, currentTree, projectRelDir)
	if err != nil {
		return false, fmt.Errorf("loading project at current commit:\n%w", err)
	}

	if _, ok := renderedProject.comparisonInputs[componentName]; !ok {
		return false, nil
	}

	if _, ok := currentProject.comparisonInputs[componentName]; !ok {
		return false, nil
	}

	result, err := classifyHistoricalComponent(
		componentName,
		renderedProject.comparisonInputs,
		currentProject.comparisonInputs,
	)
	if err != nil {
		return false, fmt.Errorf("comparing component configuration:\n%w", err)
	}

	return result.ChangeType == changeTypeUnchanged, nil
}

func writeNoChangeRebuildMarker(env *azldev.Env, componentDir string) error {
	timestamp, err := runRenderHostCommand(env, env, "date", "-Is")
	if err != nil {
		return err
	}

	markerPath := filepath.Join(componentDir, noChangeRebuildFilename)
	if err := fileutils.WriteFile(
		env.FS(), markerPath, []byte(timestamp+"\n"), fileperms.PublicFile,
	); err != nil {
		return fmt.Errorf("writing no-change rebuild marker %#q:\n%w", markerPath, err)
	}

	return nil
}

func prepareLockfileFreeRender(
	ctx context.Context,
	env *azldev.Env,
	comp components.Component,
	workingDir string,
	existingDistGitDir string,
) (*lockfileFreeRenderState, error) {
	componentName := comp.GetName()

	specPath, err := findSpecFile(env.FS(), workingDir, componentName)
	if err != nil {
		return nil, err
	}

	calculation := comp.GetConfig().Release.Calculation
	if calculation == "" {
		calculation = projectconfig.ReleaseCalculationAuto
	}

	if calculation == projectconfig.ReleaseCalculationStatic {
		return nil, fmt.Errorf(
			"component %#q cannot use 'release.calculation = \"static\"' "+
				"with '--without-lockfile component render'; use 'auto', 'autorelease', or 'manual'",
			componentName)
	}

	if comp.GetConfig().Spec.SourceType != projectconfig.SpecSourceTypeUpstream {
		return prepareLocalLockfileFreeRender(env.FS(), specPath, componentName)
	}

	return prepareUpstreamLockfileFreeRender(
		ctx, env, comp, specPath, workingDir, existingDistGitDir,
	)
}

func prepareLocalLockfileFreeRender(
	fs opctx.FS,
	specPath string,
	componentName string,
) (*lockfileFreeRenderState, error) {
	protected, err := readProtectedSpecFields(fs, specPath)
	if err != nil {
		return nil, err
	}

	return &lockfileFreeRenderState{
		commitMessage: simpleRenderCommitMessage(componentName, ""),
		protected:     protected,
	}, nil
}

func prepareUpstreamLockfileFreeRender(
	ctx context.Context,
	env *azldev.Env,
	comp components.Component,
	specPath string,
	workingDir string,
	existingDistGitDir string,
) (*lockfileFreeRenderState, error) {
	componentName := comp.GetName()

	existing, err := fileutils.DirExists(env.FS(), existingDistGitDir)
	if err != nil {
		return nil, fmt.Errorf("checking existing dist-git directory %#q:\n%w",
			existingDistGitDir, err)
	}

	upstreamRelease, err := sources.GetReleaseTagValue(env.FS(), specPath)
	if err != nil {
		return nil, fmt.Errorf("reading upstream Release value:\n%w", err)
	}

	calculation := comp.GetConfig().Release.Calculation
	if calculation == "" {
		calculation = projectconfig.ReleaseCalculationAuto
	}

	usesAutorelease := sources.ReleaseUsesAutorelease(upstreamRelease)
	currentUpstreamCommit := comp.GetConfig().EffectiveUpstreamCommit()

	upstreamChanged, upstreamMessages, err := renderedUpstreamChanges(
		ctx,
		env,
		comp.GetConfig(),
		componentName,
		workingDir,
		existingDistGitDir,
		existing,
		currentUpstreamCommit,
	)
	if err != nil {
		return nil, err
	}

	message := simpleRenderCommitMessage(componentName, upstreamMessages)

	if err := manageUpstreamRenderRelease(
		ctx,
		env,
		specPath,
		workingDir,
		existingDistGitDir,
		existing,
		upstreamChanged,
		upstreamRelease,
		message,
		calculation,
		usesAutorelease,
	); err != nil {
		return nil, err
	}

	protected, err := readProtectedSpecFields(env.FS(), specPath)
	if err != nil {
		return nil, err
	}

	return &lockfileFreeRenderState{
		commitMessage: message,
		protected:     protected,
	}, nil
}

func manageUpstreamRenderRelease(
	ctx context.Context,
	env *azldev.Env,
	specPath string,
	workingDir string,
	existingDistGitDir string,
	existing bool,
	upstreamChanged bool,
	upstreamRelease string,
	message string,
	calculation projectconfig.ReleaseCalculation,
	usesAutorelease bool,
) error {
	switch calculation {
	case projectconfig.ReleaseCalculationManual:
		return nil
	case projectconfig.ReleaseCalculationAutorelease:
		return prepareAutoreleaseSpec(
			ctx,
			env,
			specPath,
			workingDir,
			existingDistGitDir,
			existing,
			upstreamChanged,
			usesAutorelease,
		)
	case projectconfig.ReleaseCalculationStatic:
		return errors.New(
			"'release.calculation = \"static\"' is not supported for lock-file-free rendering",
		)
	case projectconfig.ReleaseCalculationAuto:
		if usesAutorelease {
			return prepareAutoreleaseSpec(
				ctx,
				env,
				specPath,
				workingDir,
				existingDistGitDir,
				existing,
				upstreamChanged,
				true,
			)
		}

		return prepareStaticReleaseSpec(
			ctx,
			env,
			specPath,
			workingDir,
			existingDistGitDir,
			existing,
			upstreamChanged,
			upstreamRelease,
			message,
		)
	default:
		return fmt.Errorf("unknown release calculation mode %#q", calculation)
	}
}

func renderedUpstreamChanges(
	ctx context.Context,
	env *azldev.Env,
	config *projectconfig.ComponentConfig,
	componentName string,
	workingDir string,
	existingDistGitDir string,
	existing bool,
	currentUpstreamCommit string,
) (changed bool, messages string, err error) {
	if !existing {
		return false, "", nil
	}

	previousUpstreamCommit, err := previousRenderedUpstreamCommit(
		ctx, env, config, componentName, existingDistGitDir,
	)
	if err != nil {
		return false, "", err
	}

	changed = previousUpstreamCommit != "" && currentUpstreamCommit != previousUpstreamCommit
	if !changed {
		return false, "", nil
	}

	messages, err = gitutils.RunInDir(
		ctx,
		env,
		workingDir,
		"log",
		"--oneline",
		previousUpstreamCommit+".."+currentUpstreamCommit,
	)
	if err != nil {
		return false, "", fmt.Errorf("collecting upstream change messages for component %#q:\n%w",
			componentName, err)
	}

	return true, messages, nil
}

func prepareAutoreleaseSpec(
	ctx context.Context,
	env *azldev.Env,
	specPath string,
	workingDir string,
	existingDistGitDir string,
	existing bool,
	upstreamChanged bool,
	updateBase bool,
) error {
	if err := initializeAutoreleaseSpec(
		ctx, env, specPath, workingDir, existingDistGitDir, existing,
	); err != nil {
		return err
	}

	if updateBase && (!existing || upstreamChanged) {
		return updateAutoreleaseBase(ctx, env, specPath, workingDir)
	}

	return nil
}

func initializeAutoreleaseSpec(
	ctx context.Context,
	env *azldev.Env,
	specPath string,
	workingDir string,
	existingDistGitDir string,
	existing bool,
) error {
	if !existing {
		changelog, err := runRenderHostCommand(
			ctx, env, "rpmautospec", "generate-changelog", workingDir,
		)
		if err != nil {
			return err
		}

		changelogPath := filepath.Join(workingDir, changelogFilename)
		if err := fileutils.WriteFile(
			env.FS(), changelogPath, []byte(changelog+"\n"), fileperms.PublicFile,
		); err != nil {
			return fmt.Errorf("writing generated changelog %#q:\n%w", changelogPath, err)
		}

		return setAutoreleaseChangelog(env.FS(), specPath)
	}

	existingSpecPath := filepath.Join(existingDistGitDir, filepath.Base(specPath))

	processed, err := specContainsRpmautospecProcessedTag(env.FS(), existingSpecPath)
	if err != nil {
		return err
	}

	if processed {
		existingRelease, err := extractProcessedAutoreleaseState(
			ctx, env, existingSpecPath, existingDistGitDir, workingDir,
		)
		if err != nil {
			return err
		}

		currentRelease, err := sources.GetReleaseTagValue(env.FS(), specPath)
		if err != nil {
			return fmt.Errorf("reading new autorelease value:\n%w", err)
		}

		updatedRelease, err := setAutoreleaseBase(currentRelease, existingRelease)
		if err != nil {
			return err
		}

		return setReleaseTag(env.FS(), specPath, updatedRelease)
	}

	if err := preserveAutoreleaseChangelog(env.FS(), existingDistGitDir, workingDir); err != nil {
		return err
	}

	existingRelease, err := sources.GetReleaseTagValue(env.FS(), existingSpecPath)
	if err != nil {
		return fmt.Errorf("reading existing autorelease value:\n%w", err)
	}

	return setReleaseTag(env.FS(), specPath, existingRelease)
}

func specContainsRpmautospecProcessedTag(fs opctx.FS, specPath string) (bool, error) {
	content, err := fileutils.ReadFile(fs, specPath)
	if err != nil {
		return false, fmt.Errorf("reading existing spec %#q:\n%w", specPath, err)
	}

	return bytes.Contains(content, []byte(rpmautospecProcessedTag)), nil
}

func extractProcessedAutoreleaseState(
	ctx context.Context,
	env *azldev.Env,
	existingSpecPath string,
	existingDistGitDir string,
	workingDir string,
) (string, error) {
	changelog, err := runRenderHostCommandRaw(
		ctx,
		env,
		"rpmspec",
		"--srpm",
		"-D",
		"_sourcedir "+existingDistGitDir,
		"-D",
		rpmspecNoChangelogTrim,
		"-q",
		"--qf",
		rpmspecChangelogFormat,
		existingSpecPath,
	)
	if err != nil {
		return "", err
	}

	changelogPath := filepath.Join(workingDir, changelogFilename)
	if err := fileutils.WriteFile(
		env.FS(), changelogPath, []byte(changelog), fileperms.PublicFile,
	); err != nil {
		return "", fmt.Errorf("writing extracted changelog %#q:\n%w", changelogPath, err)
	}

	release, err := queryProcessedAutoreleaseRelease(
		ctx, env, existingSpecPath, existingDistGitDir,
	)
	if err != nil {
		return "", err
	}

	return release, nil
}

func queryProcessedAutoreleaseRelease(
	ctx context.Context,
	cmdFactory opctx.CmdFactory,
	existingSpecPath string,
	existingDistGitDir string,
) (string, error) {
	const commandName = "rpmspec"

	if !cmdFactory.CommandInSearchPath(commandName) {
		return "", fmt.Errorf("required command %#q was not found in PATH", commandName)
	}

	args := []string{
		"-D",
		"_sourcedir " + existingDistGitDir,
		"--shell",
		existingSpecPath,
	}
	rawCmd := exec.CommandContext(ctx, commandName, args...)
	rawCmd.Stdin = strings.NewReader(autoreleaseShellInput)
	rawCmd.Stderr = io.Discard

	cmd, err := cmdFactory.Command(rawCmd)
	if err != nil {
		return "", fmt.Errorf("creating command '%s %s':\n%w",
			commandName, strings.Join(args, " "), err)
	}

	output, err := cmd.RunAndGetOutput(ctx)
	if err != nil {
		return "", fmt.Errorf("command '%s %s' failed:\n%w",
			commandName, strings.Join(args, " "), err)
	}

	var releaseLines []string

	for line := range strings.SplitSeq(output, "\n") {
		if !strings.HasPrefix(line, ">") {
			releaseLines = append(releaseLines, line)
		}
	}

	release := strings.TrimSpace(strings.Join(releaseLines, "\n"))
	if release == "" {
		return "", errors.New("rpmspec returned an empty existing autorelease value")
	}

	return release, nil
}

func preserveAutoreleaseChangelog(
	fs opctx.FS,
	existingDistGitDir string,
	workingDir string,
) error {
	existingChangelog := filepath.Join(existingDistGitDir, changelogFilename)

	exists, err := fileutils.Exists(fs, existingChangelog)
	if err != nil {
		return fmt.Errorf("checking existing changelog %#q:\n%w", existingChangelog, err)
	}

	if !exists {
		return nil
	}

	content, err := fileutils.ReadFile(fs, existingChangelog)
	if err != nil {
		return fmt.Errorf("reading existing changelog %#q:\n%w", existingChangelog, err)
	}

	if err := fileutils.WriteFile(
		fs,
		filepath.Join(workingDir, changelogFilename),
		content,
		fileperms.PublicFile,
	); err != nil {
		return fmt.Errorf("preserving existing changelog:\n%w", err)
	}

	return nil
}

func updateAutoreleaseBase(
	ctx context.Context,
	env *azldev.Env,
	specPath string,
	workingDir string,
) error {
	release, err := runRenderHostCommand(
		ctx, env, "rpmautospec", "calculate-release", "--number-only", workingDir,
	)
	if err != nil {
		return err
	}

	release, err = parseReleaseNumber(release)
	if err != nil {
		return err
	}

	currentRelease, err := sources.GetReleaseTagValue(env.FS(), specPath)
	if err != nil {
		return fmt.Errorf("reading autorelease value before updating '-b':\n%w", err)
	}

	updatedRelease, err := setAutoreleaseBase(currentRelease, release)
	if err != nil {
		return err
	}

	return setReleaseTag(env.FS(), specPath, updatedRelease)
}

func parseReleaseNumber(output string) (string, error) {
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return "", errors.New("rpmautospec returned an empty release number")
	}

	release := fields[len(fields)-1]
	if !releaseNumberPattern.MatchString(release) {
		return "", fmt.Errorf("rpmautospec returned invalid release number %#q", strings.TrimSpace(output))
	}

	return release, nil
}

func prepareStaticReleaseSpec(
	ctx context.Context,
	env *azldev.Env,
	specPath string,
	workingDir string,
	existingDistGitDir string,
	existing bool,
	upstreamChanged bool,
	upstreamRelease string,
	message string,
) error {
	if existing {
		existingSpecPath := filepath.Join(existingDistGitDir, filepath.Base(specPath))

		existingRelease, err := sources.GetReleaseTagValue(env.FS(), existingSpecPath)
		if err != nil {
			return fmt.Errorf("reading existing static release value:\n%w", err)
		}

		if err := setReleaseTag(env.FS(), specPath, existingRelease); err != nil {
			return err
		}

		if err := copyChangelogSection(env.FS(), existingSpecPath, specPath); err != nil {
			return err
		}
	}

	if upstreamChanged {
		if err := setReleaseTag(env.FS(), specPath, upstreamRelease); err != nil {
			return err
		}
	}

	return runDeterministicRenderBumpSpec(
		ctx, env, workingDir, specPath, message,
	)
}

func setAutoreleaseChangelog(fs opctx.FS, specPath string) error {
	content, err := fileutils.ReadFile(fs, specPath)
	if err != nil {
		return fmt.Errorf("reading spec %#q:\n%w", specPath, err)
	}

	changelog := []byte("%changelog\n%autochangelog\n")
	updated := replaceChangelogSection(content, changelog, true)

	return writeExistingMode(fs, specPath, updated)
}

func runDeterministicRenderBumpSpec(
	ctx context.Context,
	env *azldev.Env,
	workingDir string,
	specPath string,
	message string,
) error {
	userString, datestamp, err := projectHeadBumpMetadata(env)
	if err != nil {
		return err
	}

	return runRenderBumpSpec(
		ctx, env, workingDir, specPath, message, userString, datestamp,
	)
}

func runRenderBumpSpec(
	ctx context.Context,
	env *azldev.Env,
	workingDir string,
	specPath string,
	message string,
	userString string,
	datestamp string,
) error {
	if !env.CommandInSearchPath(sources.RPMDevBumpspecBinary) {
		return fmt.Errorf("required command %#q was not found in PATH", sources.RPMDevBumpspecBinary)
	}

	args := []string{
		"-c", message,
		"--userstring", userString,
		"--datestamp", datestamp,
		specPath,
	}
	rawCmd := exec.CommandContext(ctx, sources.RPMDevBumpspecBinary, args...)
	rawCmd.Dir = workingDir

	var stderr bytes.Buffer

	rawCmd.Stderr = &stderr

	cmd, err := env.Command(rawCmd)
	if err != nil {
		return fmt.Errorf("creating command '%s %s':\n%w",
			sources.RPMDevBumpspecBinary, strings.Join(args, " "), err)
	}

	if err := cmd.Run(ctx); err != nil {
		return fmt.Errorf("command '%s %s' failed:\n%s\n%w",
			sources.RPMDevBumpspecBinary, strings.Join(args, " "), stderr.String(), err)
	}

	return nil
}

func projectHeadBumpMetadata(env *azldev.Env) (userString string, datestamp string, err error) {
	repo, err := gitutils.OpenProjectRepo(env.ProjectDir())
	if err != nil {
		return "", "", fmt.Errorf("opening project repository for release metadata:\n%w", err)
	}

	head, err := repo.Head()
	if err != nil {
		return "", "", fmt.Errorf("resolving project HEAD for release metadata:\n%w", err)
	}

	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return "", "", fmt.Errorf("reading project HEAD for release metadata:\n%w", err)
	}

	return bumpSpecMetadata(commit.Author)
}

func bumpSpecMetadata(author object.Signature) (userString string, datestamp string, err error) {
	name := strings.TrimSpace(author.Name)
	email := strings.TrimSpace(author.Email)

	switch {
	case name != "" && email != "":
		userString = name + " <" + email + ">"
	case name != "":
		userString = name
	case email != "":
		userString = "<" + email + ">"
	default:
		return "", "", errors.New("project HEAD author has no name or email")
	}

	return userString, author.When.UTC().Format("Mon Jan 02 2006"), nil
}

func previousRenderedUpstreamCommit(
	ctx context.Context,
	cmdFactory opctx.CmdFactory,
	config *projectconfig.ComponentConfig,
	componentName string,
	distGitDir string,
) (string, error) {
	configFile := config.UpstreamCommitConfigFile()
	if configFile == nil || configFile.SourcePath() == "" {
		return "", nil
	}

	projectRepo, err := gitutils.OpenProjectRepo(filepath.Dir(configFile.SourcePath()))
	if err != nil {
		return "", fmt.Errorf("opening project repository:\n%w", err)
	}

	worktree, err := projectRepo.Worktree()
	if err != nil {
		return "", fmt.Errorf("opening project worktree:\n%w", err)
	}

	projectRepoDir := worktree.Filesystem.Root()

	distGitRelPath, err := projectRelativePath(projectRepoDir, distGitDir, "dist-git")
	if err != nil {
		return "", err
	}

	projectCommit, err := gitutils.RunInDir(
		ctx, cmdFactory, projectRepoDir, "log", "-1", "--format=%H", "--", distGitRelPath,
	)
	if err != nil {
		return "", fmt.Errorf("finding latest local dist-git change:\n%w", err)
	}

	if projectCommit == "" {
		return "", nil
	}

	configRelPath, err := projectRelativePath(
		projectRepoDir, configFile.SourcePath(), "upstream config",
	)
	if err != nil {
		return "", err
	}

	upstreamCommit, err := sources.UpstreamCommitAtCommit(
		projectRepo, projectCommit, configRelPath, componentName,
	)
	if err != nil {
		if errors.Is(err, object.ErrFileNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
			return "", nil
		}

		return "", fmt.Errorf("reading prior upstream commit from project history:\n%w", err)
	}

	return upstreamCommit, nil
}

func readProtectedSpecFields(fs opctx.FS, specPath string) (protectedSpecFields, error) {
	release, err := sources.GetReleaseTagValue(fs, specPath)
	if err != nil {
		return protectedSpecFields{}, fmt.Errorf("reading protected Release value:\n%w", err)
	}

	content, err := fileutils.ReadFile(fs, specPath)
	if err != nil {
		return protectedSpecFields{}, fmt.Errorf("reading spec %#q:\n%w", specPath, err)
	}

	changelog, hasChangelog := extractChangelogSection(content)

	return protectedSpecFields{
		release:      release,
		changelog:    changelog,
		hasChangelog: hasChangelog,
	}, nil
}

func restoreProtectedSpecFields(
	fs opctx.FS,
	specPath string,
	protected protectedSpecFields,
) error {
	if err := setReleaseTag(fs, specPath, protected.release); err != nil {
		return err
	}

	content, err := fileutils.ReadFile(fs, specPath)
	if err != nil {
		return fmt.Errorf("reading overlaid spec %#q:\n%w", specPath, err)
	}

	updated := replaceChangelogSection(content, protected.changelog, protected.hasChangelog)
	if bytes.Equal(content, updated) {
		return nil
	}

	return writeExistingMode(fs, specPath, updated)
}

func copyChangelogSection(fs opctx.FS, sourceSpecPath, targetSpecPath string) error {
	source, err := fileutils.ReadFile(fs, sourceSpecPath)
	if err != nil {
		return fmt.Errorf("reading existing spec %#q:\n%w", sourceSpecPath, err)
	}

	changelog, hasChangelog := extractChangelogSection(source)
	if !hasChangelog {
		return nil
	}

	target, err := fileutils.ReadFile(fs, targetSpecPath)
	if err != nil {
		return fmt.Errorf("reading upstream spec %#q:\n%w", targetSpecPath, err)
	}

	return writeExistingMode(fs, targetSpecPath, replaceChangelogSection(target, changelog, true))
}

func extractChangelogSection(content []byte) ([]byte, bool) {
	location := changelogSectionPattern.FindIndex(content)
	if location == nil {
		return nil, false
	}

	return bytes.Clone(content[location[0]:]), true
}

func replaceChangelogSection(content, changelog []byte, hasChangelog bool) []byte {
	location := changelogSectionPattern.FindIndex(content)
	if location == nil {
		if !hasChangelog {
			return bytes.Clone(content)
		}

		result := bytes.Clone(content)
		if len(result) > 0 && result[len(result)-1] != '\n' {
			result = append(result, '\n')
		}

		return append(result, changelog...)
	}

	result := bytes.Clone(content[:location[0]])
	if hasChangelog {
		result = append(result, changelog...)
	}

	return result
}

func setReleaseTag(fs opctx.FS, specPath, release string) error {
	content, err := fileutils.ReadFile(fs, specPath)
	if err != nil {
		return fmt.Errorf("reading spec %#q:\n%w", specPath, err)
	}

	opened, err := spec.OpenSpec(bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("parsing spec %#q:\n%w", specPath, err)
	}

	if err := opened.UpdateExistingTag("", "Release", release); err != nil {
		return fmt.Errorf("updating Release tag in %#q:\n%w", specPath, err)
	}

	var output bytes.Buffer
	if err := opened.Serialize(&output); err != nil {
		return fmt.Errorf("serializing spec %#q:\n%w", specPath, err)
	}

	return writeExistingMode(fs, specPath, output.Bytes())
}

func writeExistingMode(fs opctx.FS, path string, content []byte) error {
	info, err := fs.Stat(path)
	if err != nil {
		return fmt.Errorf("stating file %#q:\n%w", path, err)
	}

	if err := fileutils.WriteFile(fs, path, content, info.Mode().Perm()); err != nil {
		return fmt.Errorf("writing file %#q:\n%w", path, err)
	}

	return nil
}

func setAutoreleaseBase(releaseValue, base string) (string, error) {
	if !sources.ReleaseUsesAutorelease(releaseValue) {
		return "", fmt.Errorf("release value %#q does not use %%autorelease", releaseValue)
	}

	if autoreleaseBasePattern.MatchString(releaseValue) {
		return autoreleaseBasePattern.ReplaceAllString(releaseValue, " -b "+base), nil
	}

	if index := strings.Index(releaseValue, "autorelease"); index >= 0 {
		insertAt := index + len("autorelease")

		return releaseValue[:insertAt] + " -b " + base + releaseValue[insertAt:], nil
	}

	return "", fmt.Errorf("failed to update %%autorelease value %#q", releaseValue)
}

func simpleRenderCommitMessage(componentName, upstreamMessages string) string {
	message := "Update " + componentName
	if upstreamMessages == "" {
		return message
	}

	return message + "\n\nUpstream changes:\n" + upstreamMessages
}

func runRenderHostCommand(
	ctx context.Context,
	cmdFactory opctx.CmdFactory,
	name string,
	args ...string,
) (string, error) {
	output, err := runRenderHostCommandRaw(ctx, cmdFactory, name, args...)

	return strings.TrimSpace(output), err
}

func runRenderHostCommandRaw(
	ctx context.Context,
	cmdFactory opctx.CmdFactory,
	name string,
	args ...string,
) (string, error) {
	if !cmdFactory.CommandInSearchPath(name) {
		return "", fmt.Errorf("required command %#q was not found in PATH", name)
	}

	rawCmd := exec.CommandContext(ctx, name, args...)

	var stderr bytes.Buffer

	rawCmd.Stderr = &stderr

	cmd, err := cmdFactory.Command(rawCmd)
	if err != nil {
		return "", fmt.Errorf("creating command '%s %s':\n%w", name, strings.Join(args, " "), err)
	}

	output, err := cmd.RunAndGetOutput(ctx)
	if err != nil {
		return "", fmt.Errorf("command '%s %s' failed:\n%s\n%w",
			name, strings.Join(args, " "), stderr.String(), err)
	}

	return output, nil
}

func processSpecFilesOnHost(
	ctx context.Context,
	cmdFactory opctx.CmdFactory,
	componentDir string,
	specPath string,
) ([]string, error) {
	output, err := runRenderHostCommand(
		ctx,
		cmdFactory,
		"spectool",
		"--define",
		"_sourcedir "+componentDir,
		"-l",
		"-a",
		specPath,
	)
	if err != nil {
		return nil, err
	}

	return spectool.ParseSpectoolOutput(output), nil
}

func processPreparedSpecsOnHost(
	env *azldev.Env,
	stagingDir string,
	prepared []*preparedComponent,
) map[string]*sources.ComponentMockResult {
	if len(prepared) == 0 {
		return nil
	}

	results := parmap.Map(
		env,
		env.CPUBoundConcurrency(),
		prepared,
		nil,
		func(ctx context.Context, prep *preparedComponent) sources.ComponentMockResult {
			componentName := prep.comp.GetName()
			componentDir := filepath.Join(stagingDir, componentName)
			specPath := filepath.Join(componentDir, prep.specFilename)

			specFiles, err := processSpecFilesOnHost(ctx, env, componentDir, specPath)

			return sources.ComponentMockResult{
				Name:      componentName,
				SpecFiles: specFiles,
				Error:     err,
			}
		},
	)

	resultMap := make(map[string]*sources.ComponentMockResult, len(results))
	for idx := range results {
		if results[idx].Cancelled {
			componentName := prepared[idx].comp.GetName()
			resultMap[componentName] = &sources.ComponentMockResult{
				Name:  componentName,
				Error: context.Canceled,
			}

			continue
		}

		result := results[idx].Value
		resultMap[result.Name] = &result
	}

	return resultMap
}

func commitLockfileFreeRenderResults(
	env *azldev.Env,
	prepared []*preparedComponent,
	results []*RenderResult,
) error {
	for _, prep := range prepared {
		if prep.lockfileFreeState == nil {
			continue
		}

		result := results[prep.index]
		if result == nil || result.Status != renderStatusOK || !result.Changed {
			continue
		}

		if err := commitRenderedDistGit(
			env,
			env,
			prep.compOutputDir,
			prep.comp.GetConfig(),
			prep.lockfileFreeState.commitMessage,
		); err != nil {
			return fmt.Errorf("committing rendered component %#q:\n%w",
				prep.comp.GetName(), err)
		}
	}

	return nil
}

func commitRenderedDistGit(
	ctx context.Context,
	cmdFactory opctx.CmdFactory,
	distGitDir string,
	config *projectconfig.ComponentConfig,
	message string,
) error {
	projectRepo, err := gitutils.OpenProjectRepo(filepath.Dir(distGitDir))
	if err != nil {
		return fmt.Errorf("opening project repository for rendered dist-git:\n%w", err)
	}

	worktree, err := projectRepo.Worktree()
	if err != nil {
		return fmt.Errorf("opening project worktree:\n%w", err)
	}

	projectRepoDir := worktree.Filesystem.Root()

	paths, err := renderCommitPaths(projectRepoDir, distGitDir, config)
	if err != nil {
		return err
	}

	addArgs := append([]string{"add", "--"}, paths...)
	if _, err := gitutils.RunInDir(ctx, cmdFactory, projectRepoDir, addArgs...); err != nil {
		return fmt.Errorf("staging rendered dist-git paths %q:\n%w", paths, err)
	}

	commitArgs := []string{"commit", "--only", "-m", message, "--"}
	commitArgs = append(commitArgs, paths...)

	if _, err := gitutils.RunInDir(
		ctx, cmdFactory, projectRepoDir, commitArgs...,
	); err != nil {
		return fmt.Errorf("committing rendered dist-git paths %q:\n%w", paths, err)
	}

	return nil
}

func renderCommitPaths(
	projectRepoDir string,
	distGitDir string,
	config *projectconfig.ComponentConfig,
) ([]string, error) {
	distGitRelPath, err := projectRelativePath(projectRepoDir, distGitDir, "dist-git")
	if err != nil {
		return nil, err
	}

	paths := []string{distGitRelPath}
	if config.Spec.SourceType != projectconfig.SpecSourceTypeUpstream {
		return paths, nil
	}

	configFile := config.UpstreamCommitConfigFile()
	if configFile == nil || configFile.SourcePath() == "" {
		return paths, nil
	}

	configRelPath, err := projectRelativePath(
		projectRepoDir, configFile.SourcePath(), "upstream config",
	)
	if err != nil {
		return nil, err
	}

	return append(paths, configRelPath), nil
}

func projectRelativePath(projectRepoDir, targetPath, description string) (string, error) {
	relativePath, err := filepath.Rel(projectRepoDir, targetPath)
	if err != nil {
		return "", fmt.Errorf("resolving project-relative %s path:\n%w", description, err)
	}

	if !filepath.IsLocal(relativePath) {
		return "", fmt.Errorf("%s path %#q is outside project repository %#q",
			description, targetPath, projectRepoDir)
	}

	return relativePath, nil
}
