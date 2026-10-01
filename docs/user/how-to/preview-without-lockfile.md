# How To: Preview the Lock-File-Free Mode

`--without-lockfile` is a **preview** global flag. It selects an alternative way of
tracking a component's resolved upstream commit: instead of per-component lock
files, azldev records the commit in generated component TOML that the project
includes like any other config file.

The flag is opt-in and defaults to off. Without it, azldev behaves exactly as it
always has — lock files, `component update`, `component history`, and
`component query` are unchanged. Nothing in the preview mode is stable yet; both
the command surface and the generated file layout may change.

```bash
# Default behavior: lock files.
azldev component render -p curl

# Preview behavior: generated upstream-commit config.
azldev --without-lockfile component render -p curl
```

Pass the flag on every invocation that should use the preview mode, before the
command name. `--without-lockfile=false` explicitly selects the default mode.

## What Changes

| Area | Default | `--without-lockfile` |
|------|---------|----------------------|
| Resolved commit storage | `locks/<name>.lock` | `base/upstream-commits/<name>.toml` |
| Refresh command | `azldev component update` | `azldev component refresh-upstream-commit` |
| Inspecting resolved state | `azldev component history`, `azldev component query` | read the generated TOML; no equivalent commands |
| Lock consistency checks | On, with `--skip-lock-validation` to opt out | Not applicable; the flag is not registered |
| `component changed` | Compares stored input fingerprints | Compares project configuration resolved at each ref |
| Rendered dist-git history | Synthetic history derived from lock-file fingerprint changes | Replaced directly from local content or a fresh upstream clone |
| Agent skills and MCP tools | Describe the lock-file workflow | Describe the upstream-commit workflow |

`component update`, `component history`, and `component query` remain registered
in preview mode as hidden no-ops so that existing scripts report clearly that the
commands do nothing, rather than failing with "unknown command".

## Configure the Project

Include the generated directory **before** the component-specific TOML, so that a
component definition can still override the generated pin:

```toml
includes = [
    "base/upstream-commits/*.toml",
    "base/components/*.toml",
]
```

Generated files hold only `spec.upstream-commit`; the component's own TOML
supplies the source type and everything else. Because a single file may hold a
partial component definition in this mode, component validation runs after all
config files have been merged.

An existing `[project] lock-dir` setting is accepted and ignored in preview mode,
so the same project config works in both modes.

## Refresh a Component

```bash
# Resolve and record the upstream commit for one component.
azldev --without-lockfile component refresh-upstream-commit -p curl

# Refresh everything and prune generated files for components that no longer exist.
azldev --without-lockfile component refresh-upstream-commit -a

# CI gate: exit 1 when any generated file is out of date.
azldev --without-lockfile component refresh-upstream-commit -a --check-only -q
```

Refresh after changing a commit pin, upstream distro or version, or snapshot.
Overlay, build-config, and metadata changes do not affect the resolved commit, so
they need only a re-render.

When refreshing multiple components, resolution failures are reported per
component without discarding successful work. TOML files for successfully
resolved components are created or updated before the command exits with an
error. Failed components remain unchanged, and orphan pruning is skipped for
that run.

Run `component render` after refreshing. In preview mode, render commits the
generated upstream-commit TOML and changed rendered dist-git directory together.

## Render Components

Lock-file-free rendering requires `rpmautospec`, `rpmdev-bumpspec`, `rpmspec`,
and `spectool` on the host. It does not create synthetic git history or use mock
for release and changelog preparation.

Before rendering an existing component, azldev parses the project TOML at the
current commit and at the commit that most recently changed the component's
rendered dist-git directory. If the resolved build inputs are unchanged, render
prints a warning, skips the component, and exits successfully. Commit component
configuration and overlay-source changes before rendering so they are included
in this comparison.

Use `--allow-no-change` to force an unchanged component to render. The forced
render writes the output of `date -Is` to
`<rendered-specs-dir>/<letter>/<component>/.no_change_rebuild`, replacing the
file when it already exists. The marker ensures the rebuild is recorded in the
rendered dist-git commit.

For a local component, render copies the configured local content, applies the
normal overlays and transformations, and replaces
`<rendered-specs-dir>/<letter>/<component>/`.

For an upstream component, render:

1. Clones the configured upstream dist-git branch and checks out the generated
   `upstream-commit`.
2. Preserves the existing rendered release and changelog state when appropriate.
3. When `%autorelease` is used, creates or preserves the `changelog` file and
   sets `%autorelease -b` from `rpmautospec calculate-release --number-only`.
   During migration from lockfile rendering, if the existing rendered spec has
   already been expanded by rpmautospec, render uses host `rpmspec` to recover
   its autorelease value and full, untrimmed changelog before replacing the
   directory.
4. For static releases, runs `rpmdev-bumpspec`. When the upstream commit moved,
   the changelog message includes `git log --oneline` output for the upstream
   range.
5. Applies overlays while preserving the release and `%changelog` prepared
   above.
6. Replaces the rendered component directory and commits it. For upstream
   components, the generated upstream-commit TOML is included in the same
   commit.

The temporary commit message is `Update <component>`, followed by the upstream
change messages when available. A later change will replace this with richer
project-derived messages.

## Detect Changed Components

```bash
azldev --without-lockfile component changed --from main -a -q -O json
```

In preview mode this loads the project configuration independently at both refs
and compares the resolved component build inputs: normalized component
configuration, upstream commit or local spec-directory contents, overlay source
filenames and contents, and the effective distro release version. Documentation,
publishing, test-selection, scheduling-hint, snapshot-time, and checkout-path-only
fields do not mark a component as changed.

## Emit Agent Files for the Preview Mode

`azldev docs agent install` emits the content for the mode it runs in, so pass the
flag when the target repository uses the preview workflow:

```bash
azldev --without-lockfile docs agent install
```

## Reference Documentation

The generated CLI reference under [reference/cli/](../reference/cli/azldev.md)
documents azldev's default mode. Use `azldev --without-lockfile <command> --help`
to see the preview mode's command surface and help text.
