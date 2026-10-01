// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components/components_testutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/sources"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/testutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestSetAutoreleaseBase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		release string
		base    string
		want    string
		wantErr bool
	}{
		{
			name:    "bare",
			release: "%autorelease",
			base:    "12",
			want:    "%autorelease -b 12",
		},
		{
			name:    "braced",
			release: "%{autorelease}",
			base:    "7",
			want:    "%{autorelease -b 7}",
		},
		{
			name:    "preserves arguments",
			release: "%{autorelease -e preview}",
			base:    "4",
			want:    "%{autorelease -b 4 -e preview}",
		},
		{
			name:    "updates existing base",
			release: "%autorelease -b 2 -p",
			base:    "9",
			want:    "%autorelease -b 9 -p",
		},
		{
			name:    "updates existing braced base",
			release: "%{autorelease -b 2 -p}",
			base:    "9",
			want:    "%{autorelease -b 9 -p}",
		},
		{
			name:    "rejects static release",
			release: "3%{?dist}",
			base:    "9",
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			actual, err := setAutoreleaseBase(test.release, test.base)
			if test.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, test.want, actual)
		})
	}
}

func TestPrepareLockfileFreeRender_FirstAutorelease(t *testing.T) {
	testEnv := testutils.NewTestEnvWithoutLockfile(t)
	testEnv.CmdFactory.RegisterCommandInSearchPath("rpmautospec")

	const workingDir = "/work/curl"

	writeTestSpec(t, testEnv, "%autorelease", "%autochangelog\n")

	var calls [][]string

	testEnv.CmdFactory.RunAndGetOutputHandler = func(cmd *exec.Cmd) (string, error) {
		calls = append(calls, cmd.Args)

		switch {
		case len(cmd.Args) >= 2 && cmd.Args[1] == "generate-changelog":
			return "* Tue Jan 01 2026 Upstream <upstream@example.com> - 1-1\n- Initial", nil
		case len(cmd.Args) >= 2 && cmd.Args[1] == "calculate-release":
			return "Calculated release number: 8", nil
		default:
			t.Fatalf("unexpected command: %v", cmd.Args)

			return "", nil
		}
	}

	comp := newRenderTestComponent(t, "curl", projectconfig.SpecSourceTypeUpstream, "abc1234")
	state, err := prepareLockfileFreeRender(
		context.Background(), testEnv.Env, comp, workingDir, "/specs/c/curl",
	)
	require.NoError(t, err)

	release, err := sources.GetReleaseTagValue(testEnv.TestFS, filepath.Join(workingDir, "curl.spec"))
	require.NoError(t, err)
	assert.Equal(t, "%autorelease -b 8", release)

	changelog, err := fileutils.ReadFile(testEnv.TestFS, filepath.Join(workingDir, changelogFilename))
	require.NoError(t, err)
	assert.Equal(t,
		"* Tue Jan 01 2026 Upstream <upstream@example.com> - 1-1\n- Initial\n",
		string(changelog))
	assert.Equal(t, "Update curl", state.commitMessage)
	assert.Len(t, calls, 2)
}

func TestPrepareAutoreleaseSpec_ExistingProcessedSpec(t *testing.T) {
	for _, testCase := range []struct {
		name            string
		initialRelease  string
		upstreamChanged bool
		expectedRelease string
	}{
		{
			name:            "unchanged upstream uses extracted autorelease base",
			initialRelease:  "%autorelease",
			upstreamChanged: false,
			expectedRelease: "%autorelease -b 1",
		},
		{
			name:            "unchanged upstream replaces existing autorelease base",
			initialRelease:  "%autorelease -b 99 -p",
			upstreamChanged: false,
			expectedRelease: "%autorelease -b 1 -p",
		},
		{
			name:            "changed upstream recalculates autorelease base",
			initialRelease:  "%autorelease",
			upstreamChanged: true,
			expectedRelease: "%autorelease -b 8",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			testEnv := testutils.NewTestEnvWithoutLockfile(t)
			testEnv.CmdFactory.RegisterCommandInSearchPath("rpmspec")

			if testCase.upstreamChanged {
				testEnv.CmdFactory.RegisterCommandInSearchPath("rpmautospec")
			}

			const (
				workingDir         = "/work/curl"
				existingDistGitDir = "/specs/c/curl"
			)

			specPath := writeTestSpec(
				t, testEnv, testCase.initialRelease, "%autochangelog\n",
			)
			require.NoError(t, fileutils.MkdirAll(testEnv.TestFS, existingDistGitDir))

			existingSpecPath := filepath.Join(existingDistGitDir, "curl.spec")
			processedSpec := strings.Join([]string{
				"Name: curl",
				"Version: 1",
				"Release: 7.azl3",
				"# RPMAUTOSPEC: autorelease, autochangelog",
				"",
				"%description",
				"test",
				"",
				"%changelog",
				"* Tue Jan 01 2026 Existing <existing@example.com> - 1-7",
				"- Existing change",
				"",
			}, "\n")
			require.NoError(t, fileutils.WriteFile(
				testEnv.TestFS,
				existingSpecPath,
				[]byte(processedSpec),
				fileperms.PublicFile,
			))
			require.NoError(t, fileutils.WriteFile(
				testEnv.TestFS,
				filepath.Join(workingDir, changelogFilename),
				[]byte("stale changelog\n"),
				fileperms.PublicFile,
			))

			const extractedChangelog = "* Tue Jan 01 2026 Existing <existing@example.com>\n" +
				"- Existing change\n\n"

			var calls [][]string

			testEnv.CmdFactory.RunAndGetOutputHandler = func(cmd *exec.Cmd) (string, error) {
				calls = append(calls, cmd.Args)

				switch {
				case cmd.Args[0] == "rpmspec" && cmd.Args[3] == "--shell":
					stdin, err := io.ReadAll(cmd.Stdin)
					require.NoError(t, err)
					assert.Equal(t, autoreleaseShellInput, string(stdin))
					assert.Equal(t, io.Discard, cmd.Stderr)

					return "> \n1\n> \n", nil
				case cmd.Args[0] == "rpmspec" && cmd.Args[8] == rpmspecChangelogFormat:
					return extractedChangelog, nil
				case cmd.Args[0] == "rpmautospec":
					return "Calculated release number: 8", nil
				default:
					t.Fatalf("unexpected command: %v", cmd.Args)

					return "", nil
				}
			}

			err := prepareAutoreleaseSpec(
				context.Background(),
				testEnv.Env,
				specPath,
				workingDir,
				existingDistGitDir,
				true,
				testCase.upstreamChanged,
			)
			require.NoError(t, err)

			release, err := sources.GetReleaseTagValue(testEnv.TestFS, specPath)
			require.NoError(t, err)
			assert.Equal(t, testCase.expectedRelease, release)

			changelog, err := fileutils.ReadFile(
				testEnv.TestFS, filepath.Join(workingDir, changelogFilename),
			)
			require.NoError(t, err)
			assert.Equal(t, extractedChangelog, string(changelog))

			expectedCallCount := 2
			if testCase.upstreamChanged {
				expectedCallCount++
			}

			assert.Len(t, calls, expectedCallCount)
			assert.Equal(t, []string{
				"rpmspec",
				"--srpm",
				"-D",
				"_sourcedir " + existingDistGitDir,
				"-D",
				rpmspecNoChangelogTrim,
				"-q",
				"--qf",
				rpmspecChangelogFormat,
				existingSpecPath,
			}, calls[0])
			assert.Equal(t, []string{
				"rpmspec",
				"-D",
				"_sourcedir " + existingDistGitDir,
				"--shell",
				existingSpecPath,
			}, calls[1])
		})
	}
}

func TestPrepareLockfileFreeRender_FirstStaticRelease(t *testing.T) {
	testEnv := testutils.NewTestEnvWithoutLockfile(t)
	testEnv.CmdFactory.RegisterCommandInSearchPath("rpmdev-bumpspec")

	const workingDir = "/work/curl"

	specPath := writeTestSpec(
		t, testEnv, "4%{?dist}",
		"%changelog\n* Tue Jan 01 2026 Upstream <upstream@example.com> - 1-4\n- Upstream\n",
	)

	testEnv.CmdFactory.RunAndGetOutputHandler = func(cmd *exec.Cmd) (string, error) {
		require.Equal(t,
			[]string{"rpmdev-bumpspec", "-c", "Update curl", specPath},
			cmd.Args)

		content := strings.Join([]string{
			"Name: curl",
			"Version: 1",
			"Release: 5%{?dist}",
			"",
			"%description",
			"test",
			"",
			"%changelog",
			"* Wed Jan 02 2026 Test <test@example.com> - 1-5",
			"- Update curl",
			"* Tue Jan 01 2026 Upstream <upstream@example.com> - 1-4",
			"- Upstream",
			"",
		}, "\n")
		require.NoError(t, fileutils.WriteFile(
			testEnv.TestFS, specPath, []byte(content), fileperms.PublicFile,
		))

		return "", nil
	}

	comp := newRenderTestComponent(t, "curl", projectconfig.SpecSourceTypeUpstream, "abc1234")
	state, err := prepareLockfileFreeRender(
		context.Background(), testEnv.Env, comp, workingDir, "/specs/c/curl",
	)
	require.NoError(t, err)

	release, err := sources.GetReleaseTagValue(testEnv.TestFS, specPath)
	require.NoError(t, err)
	assert.Equal(t, "5%{?dist}", release)
	assert.Contains(t, string(state.protected.changelog), "- Update curl")
}

func TestRestoreProtectedSpecFields(t *testing.T) {
	testEnv := testutils.NewTestEnvWithoutLockfile(t)

	specPath := writeTestSpec(
		t, testEnv, "5%{?dist}",
		"%changelog\n* Wed Jan 02 2026 Test <test@example.com> - 1-5\n- Prepared\n",
	)

	protected, err := readProtectedSpecFields(testEnv.TestFS, specPath)
	require.NoError(t, err)

	writeTestSpec(
		t, testEnv, "99%{?dist}",
		"%changelog\n* Thu Jan 03 2026 Overlay <overlay@example.com> - 1-99\n- Replaced\n",
	)

	require.NoError(t, restoreProtectedSpecFields(testEnv.TestFS, specPath, protected))

	release, err := sources.GetReleaseTagValue(testEnv.TestFS, specPath)
	require.NoError(t, err)
	assert.Equal(t, "5%{?dist}", release)

	content, err := fileutils.ReadFile(testEnv.TestFS, specPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "- Prepared")
	assert.NotContains(t, string(content), "- Replaced")
}

func TestPreserveAutoreleaseChangelog(t *testing.T) {
	testEnv := testutils.NewTestEnvWithoutLockfile(t)
	require.NoError(t, fileutils.MkdirAll(testEnv.TestFS, "/specs/c/curl"))
	require.NoError(t, fileutils.MkdirAll(testEnv.TestFS, "/work/curl"))
	require.NoError(t, fileutils.WriteFile(
		testEnv.TestFS,
		"/specs/c/curl/changelog",
		[]byte("existing changelog\n"),
		fileperms.PublicFile,
	))

	require.NoError(t, preserveAutoreleaseChangelog(
		testEnv.TestFS, "/specs/c/curl", "/work/curl",
	))

	content, err := fileutils.ReadFile(testEnv.TestFS, "/work/curl/changelog")
	require.NoError(t, err)
	assert.Equal(t, "existing changelog\n", string(content))
}

func TestComponentUnchangedAtCommits(t *testing.T) {
	rootConfig := []byte(`includes = ["component.toml"]

[project]
default-distro = { name = "testdistro", version = "1.0" }
rendered-specs-dir = "SPECS"

[distros.testdistro]
description = "Test distro"

[distros.testdistro.versions."1.0"]
release-ver = "1.0"
`)
	originalComponent := []byte(`[components.curl]
spec = {
    type = "upstream",
    upstream-distro = { name = "testdistro", version = "1.0" },
    upstream-name = "curl",
    upstream-commit = "abcdef1234567",
}
build = { defines = { feature = "enabled" } }
`)
	changedComponent := []byte(`[components.curl]
spec = {
    type = "upstream",
    upstream-distro = { name = "testdistro", version = "1.0" },
    upstream-name = "curl",
    upstream-commit = "abcdef1234567",
}
build = { defines = { feature = "disabled" } }
`)

	repo, hashes := testRepoWithCommits(t, []testRepoCommit{
		{files: map[string][]byte{
			"azldev.toml":    rootConfig,
			"component.toml": originalComponent,
		}},
		{files: map[string][]byte{
			"README.md": []byte("unrelated change\n"),
		}},
		{files: map[string][]byte{
			"component.toml": changedComponent,
		}},
	})

	testEnv := testutils.NewTestEnvWithoutLockfile(t)

	unchanged, err := componentUnchangedAtCommits(
		testEnv.Env, repo, ".", "curl", hashes[0], hashes[1],
	)
	require.NoError(t, err)
	assert.True(t, unchanged)

	unchanged, err = componentUnchangedAtCommits(
		testEnv.Env, repo, ".", "curl", hashes[0], hashes[2],
	)
	require.NoError(t, err)
	assert.False(t, unchanged)
}

func TestWriteNoChangeRebuildMarker(t *testing.T) {
	testEnv := testutils.NewTestEnvWithoutLockfile(t)
	testEnv.CmdFactory.RegisterCommandInSearchPath("date")
	require.NoError(t, fileutils.MkdirAll(testEnv.TestFS, "/work/curl"))
	require.NoError(t, fileutils.WriteFile(
		testEnv.TestFS,
		"/work/curl/"+noChangeRebuildFilename,
		[]byte("old timestamp\n"),
		fileperms.PublicFile,
	))

	testEnv.CmdFactory.RunAndGetOutputHandler = func(cmd *exec.Cmd) (string, error) {
		require.Equal(t, []string{"date", "-Is"}, cmd.Args)

		return "2026-10-01T21:37:10+00:00", nil
	}

	require.NoError(t, writeNoChangeRebuildMarker(testEnv.Env, "/work/curl"))

	content, err := fileutils.ReadFile(
		testEnv.TestFS, "/work/curl/"+noChangeRebuildFilename,
	)
	require.NoError(t, err)
	assert.Equal(t, "2026-10-01T21:37:10+00:00\n", string(content))
}

func TestProjectRelativePath(t *testing.T) {
	t.Parallel()

	path, err := projectRelativePath("/project", "/project/specs/c/curl", "dist-git")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("specs", "c", "curl"), path)

	_, err = projectRelativePath("/project", "/outside/specs/c/curl", "dist-git")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside project repository")
}

func newRenderTestComponent(
	t *testing.T,
	name string,
	sourceType projectconfig.SpecSourceType,
	upstreamCommit string,
) *components_testutils.MockComponent {
	t.Helper()

	ctrl := gomock.NewController(t)
	comp := components_testutils.NewMockComponent(ctrl)
	config := &projectconfig.ComponentConfig{
		Name: name,
		Spec: projectconfig.SpecSource{
			SourceType:     sourceType,
			UpstreamCommit: upstreamCommit,
		},
	}

	comp.EXPECT().GetName().AnyTimes().Return(name)
	comp.EXPECT().GetConfig().AnyTimes().Return(config)

	return comp
}

func writeTestSpec(
	t *testing.T,
	testEnv *testutils.TestEnv,
	release string,
	tail string,
) string {
	t.Helper()

	const (
		dir  = "/work/curl"
		name = "curl"
	)

	require.NoError(t, fileutils.MkdirAll(testEnv.TestFS, dir))

	content := strings.Join([]string{
		"Name: " + name,
		"Version: 1",
		"Release: " + release,
		"",
		"%description",
		"test",
		"",
		tail,
	}, "\n")

	specPath := filepath.Join(dir, name+".spec")
	require.NoError(t, fileutils.WriteFile(
		testEnv.TestFS, specPath, []byte(content), fileperms.PublicFile,
	))

	return specPath
}
