# Release preparation

Ordinary development PRs add user-facing changes under `## Unreleased` in
`CHANGELOG.md`. `server.json` keeps the latest released version.

Before tagging a new version, open a release PR from a branch named
`release/vX.Y.Z` or title it `release: vX.Y.Z`. The name/title is the signal
that makes the PR check require that exact version. Without a release signal,
CI cannot distinguish an ordinary PR from a forgotten release PR.

1. Run `make prepare-release RELEASE_VERSION=vX.Y.Z`. This moves the current
   Unreleased notes into a dated version section and updates both the version
   and OCI identifier in `server.json`.
2. Review and edit the release notes. Run
   `make check-release RELEASE_VERSION=vX.Y.Z` locally.
   The release workflow exports that exact version's reviewed CHANGELOG body
   into the GitHub Release before the automatically generated change list.
   `scripts/release_metadata.py notes --expected vX.Y.Z --output <output-file>`
   can preview the body; it rejects mismatched versions and pending Unreleased
   changes before writing the output.
3. Merge only after the `Test` workflow passes. In GitHub branch protection,
   require `test (ubuntu-latest)`, `test (macos-latest)`, and
   `test (windows-latest)` on both `main` and `dev`. A workflow that merely
   runs does not prevent merging.
4. After the release PR is merged, tag that merged commit with `vX.Y.Z`.
   Committing and pushing still require the human approval described in
   `AGENTS.md`.

Both tag-triggered publishing workflows check the committed files against the
tag before building or publishing. They no longer rewrite `server.json` inside
the runner, which previously hid stale metadata in the repository.

## Releases that migrate stored data

The first release that enables encrypted storage must put a prominent upgrade
notice before its feature list, in both the versioned CHANGELOG section and the
GitHub Release body. Use the
[migration notice draft](release-notes/encrypted-storage-migration.md) only after
the migration feature is implemented and verified; a design document alone is
not a reason to announce that a version migrates data.

The notice must explicitly state that the new version migrates old persistent
files on first startup after key setup or unlock. Include what is migrated,
stopping old processes, backing up the configured data directory, extra disk
space, startup downtime and progress, interrupted migrations, plaintext cleanup,
key/backup retention, and the supported rollback procedure.

Review the runtime messages alongside the release notice: users must see when
migration starts, resumes, fails, completes, or leaves plaintext cleanup pending.
Web UI, foreground CLI, daemon startup, and MCP stdio must follow the
[migration notification contract](design/encrypted-storage.md#用户提示与升级告知).
Do not mark a release ready until these messages and its published promises
match the tested migration behavior.
