// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/sources"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	rpmspecfile "github.com/microsoft/azure-linux-dev-tools/internal/rpm/spec"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	gitutils "github.com/microsoft/azure-linux-dev-tools/internal/utils/git"
)

type renderReleaseAction int

const (
	renderReleaseActionNone renderReleaseAction = iota
	renderReleaseActionInitializeAutorelease
	renderReleaseActionBumpSpec
)

func validateWithoutLockfileRenderComponents(
	env *azldev.Env,
	componentsToRender []components.Component,
) error {
	if !env.WithoutLockfile() {
		return nil
	}

	for _, component := range componentsToRender {
		if err := validateWithoutLockfileRenderReleaseConfig(component.GetConfig()); err != nil {
			return err
		}
	}

	return nil
}

func validateWithoutLockfileRenderReleaseConfig(config *projectconfig.ComponentConfig) error {
	if config.Release.Calculation != projectconfig.ReleaseCalculationStatic {
		return nil
	}

	if config.Spec.SourceType == projectconfig.SpecSourceTypeLocal {
		return fmt.Errorf(
			"local component %#q cannot use 'release.calculation = \"static\"'; "+
				"use 'auto', 'autorelease', or 'manual'",
			config.Name)
	}

	return fmt.Errorf(
		"upstream component %#q cannot use 'release.calculation = \"static\"' "+
			"with '--without-lockfile component render'; use 'auto', 'autorelease', or 'manual'",
		config.Name)
}

func manageWithoutLockfileRenderRelease(
	env *azldev.Env,
	component components.Component,
	componentDir string,
	componentOutputDir string,
	specPath string,
) error {
	action, err := resolveRenderReleaseAction(component.GetConfig(), specPath, env)
	if err != nil {
		return err
	}

	switch action {
	case renderReleaseActionNone:
		return nil
	case renderReleaseActionInitializeAutorelease:
		existsAtHead, existsErr := renderedDirExistsAtHead(env, componentOutputDir)
		if existsErr != nil {
			return fmt.Errorf("checking rendered dist-git dir for component %#q at HEAD:\n%w",
				component.GetName(), existsErr)
		}

		if existsAtHead {
			return nil
		}

		return initializeAutoreleaseRender(env, componentDir, specPath)
	case renderReleaseActionBumpSpec:
		return runRPMDevBumpSpec(env, componentDir, specPath)
	default:
		return fmt.Errorf("unknown render release action %d", action)
	}
}

func resolveRenderReleaseAction(
	config *projectconfig.ComponentConfig,
	specPath string,
	env *azldev.Env,
) (renderReleaseAction, error) {
	calculation := config.Release.Calculation
	if calculation == "" {
		calculation = projectconfig.ReleaseCalculationAuto
	}

	if calculation == projectconfig.ReleaseCalculationStatic {
		return renderReleaseActionNone, validateWithoutLockfileRenderReleaseConfig(config)
	}

	if config.Spec.SourceType == projectconfig.SpecSourceTypeLocal {
		return renderReleaseActionNone, nil
	}

	switch calculation {
	case projectconfig.ReleaseCalculationManual:
		return renderReleaseActionNone, nil
	case projectconfig.ReleaseCalculationAutorelease:
		return renderReleaseActionInitializeAutorelease, nil
	case projectconfig.ReleaseCalculationStatic:
		return renderReleaseActionNone, validateWithoutLockfileRenderReleaseConfig(config)
	case projectconfig.ReleaseCalculationAuto:
		releaseValue, err := sources.GetReleaseTagValue(env.FS(), specPath)
		if err != nil {
			return renderReleaseActionNone, fmt.Errorf(
				"reading Release tag for component %#q:\n%w", config.Name, err)
		}

		if sources.ReleaseUsesAutorelease(releaseValue) {
			return renderReleaseActionInitializeAutorelease, nil
		}

		return renderReleaseActionBumpSpec, nil
	default:
		return renderReleaseActionNone, fmt.Errorf(
			"component %#q has unknown release calculation mode %#q",
			config.Name, calculation)
	}
}

func renderedDirExistsAtHead(env *azldev.Env, componentOutputDir string) (bool, error) {
	repo, err := gitutils.OpenProjectRepo(env.ProjectDir())
	if err != nil {
		return false, fmt.Errorf("opening project repository:\n%w", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return false, fmt.Errorf("getting project worktree:\n%w", err)
	}

	absoluteOutputDir := componentOutputDir
	if !filepath.IsAbs(absoluteOutputDir) {
		currentDir, getwdErr := env.OSEnv().Getwd()
		if getwdErr != nil {
			return false, fmt.Errorf("getting current directory:\n%w", getwdErr)
		}

		absoluteOutputDir = filepath.Join(currentDir, absoluteOutputDir)
	}

	return renderedDirExistsInHeadTree(repo, worktree.Filesystem.Root(), absoluteOutputDir)
}

func renderedDirExistsInHeadTree(
	repo *gogit.Repository,
	repoRoot string,
	absoluteOutputDir string,
) (bool, error) {
	relativeOutputDir, err := filepath.Rel(repoRoot, absoluteOutputDir)
	if err != nil {
		return false, fmt.Errorf("computing rendered dist-git path relative to repository:\n%w", err)
	}

	if relativeOutputDir == ".." ||
		strings.HasPrefix(relativeOutputDir, ".."+string(filepath.Separator)) {
		return false, nil
	}

	head, err := repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("resolving HEAD:\n%w", err)
	}

	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return false, fmt.Errorf("reading HEAD commit:\n%w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return false, fmt.Errorf("reading HEAD tree:\n%w", err)
	}

	_, err = tree.Tree(filepath.ToSlash(relativeOutputDir))
	if isFileNotFound(err) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("reading rendered dist-git dir %#q from HEAD:\n%w",
			relativeOutputDir, err)
	}

	return true, nil
}

func initializeAutoreleaseRender(
	env *azldev.Env,
	componentDir string,
	specPath string,
) error {
	if !env.CommandInSearchPath("rpmautospec") {
		return errors.New("'rpmautospec' is required to initialize an autorelease changelog")
	}

	command := exec.CommandContext(
		env, "rpmautospec", "generate-changelog", filepath.Base(specPath),
	)
	command.Dir = componentDir

	var stderr bytes.Buffer

	var stdout bytes.Buffer

	command.Stdout = &stdout
	command.Stderr = &stderr

	wrapped, err := env.Command(command)
	if err != nil {
		return fmt.Errorf("wrapping 'rpmautospec generate-changelog' command:\n%w", err)
	}

	if err := wrapped.Run(env); err != nil {
		if stderrText := strings.TrimSpace(stderr.String()); stderrText != "" {
			return fmt.Errorf("running 'rpmautospec generate-changelog':\n%s\n%w", stderrText, err)
		}

		return fmt.Errorf("running 'rpmautospec generate-changelog':\n%w", err)
	}

	changelogPath := filepath.Join(componentDir, "changelog")
	if err := fileutils.WriteFile(
		env.FS(), changelogPath, stdout.Bytes(), fileperms.PublicFile,
	); err != nil {
		return fmt.Errorf("writing generated changelog %#q:\n%w", changelogPath, err)
	}

	specData, err := fileutils.ReadFile(env.FS(), specPath)
	if err != nil {
		return fmt.Errorf("reading spec %#q:\n%w", specPath, err)
	}

	openedSpec, err := rpmspecfile.OpenSpec(bytes.NewReader(specData))
	if err != nil {
		return fmt.Errorf("parsing spec %#q:\n%w", specPath, err)
	}

	openedSpec.SetAutoreleaseChangelog()

	specFile, err := env.FS().OpenFile(specPath, os.O_WRONLY|os.O_TRUNC, fileperms.PublicFile)
	if err != nil {
		return fmt.Errorf("opening spec %#q for writing:\n%w", specPath, err)
	}

	if err := openedSpec.Serialize(specFile); err != nil {
		_ = specFile.Close()

		return fmt.Errorf("writing spec %#q:\n%w", specPath, err)
	}

	if err := specFile.Close(); err != nil {
		return fmt.Errorf("closing spec %#q:\n%w", specPath, err)
	}

	return nil
}

func runRPMDevBumpSpec(env *azldev.Env, componentDir string, specPath string) error {
	if !env.CommandInSearchPath("rpmdev-bumpspec") {
		return errors.New("'rpmdev-bumpspec' is required to update an automatic static release")
	}

	command := exec.CommandContext(env, "rpmdev-bumpspec", filepath.Base(specPath))
	command.Dir = componentDir

	var stderr bytes.Buffer

	command.Stderr = &stderr

	wrapped, err := env.Command(command)
	if err != nil {
		return fmt.Errorf("wrapping 'rpmdev-bumpspec' command:\n%w", err)
	}

	if err := wrapped.Run(env); err != nil {
		if stderrText := strings.TrimSpace(stderr.String()); stderrText != "" {
			return fmt.Errorf("running 'rpmdev-bumpspec':\n%s\n%w", stderrText, err)
		}

		return fmt.Errorf("running 'rpmdev-bumpspec':\n%w", err)
	}

	return nil
}
