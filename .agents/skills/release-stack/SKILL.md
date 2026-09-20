---
name: release-stack
description: Publish or prepare an autok-stack release and update its dedicated Homebrew tap, following the same source-archive distribution process as Logal. Use for stack version bumps, GitHub releases, and Homebrew release updates.
---

# Release stack

Release the Go source archive, then update the Homebrew formula to reference those exact bytes.
Follow the user's publication scope. A preparation-only request ends with local artifacts and a summary of the remaining publication steps.
When publication is already authorized, complete both repositories without asking for the same permission again.
Creating or editing this skill does not itself request a release.

## Distribution contract

| Item | Value |
|---|---|
| Source repository | `CaliLuke/autok-stack` |
| Release branch | `main` |
| Tag | `vX.Y.Z` |
| Release asset | `autok-stack-X.Y.Z.tar.gz` |
| Archive root | `autok-stack-X.Y.Z/` |
| License | MIT, in `LICENSE` |
| Tap repository | `CaliLuke/homebrew-stack` |
| Formula | `Formula/stack.rb`, class `Stack` |
| Install command | `brew install caliluke/stack/stack` |
| Executable | `stack` |
| Version injection | `-X main.version=X.Y.Z` |

This is a dedicated tap like Logal's. Do not move the formula into `CaliLuke/homebrew-tap` or submit it to Homebrew core as part of an ordinary release.
Homebrew builds from source. Do not replace the archive with prebuilt binaries unless the user requests that change.

## 1. Select the release

- Read the repository's `AGENTS.md`, current formula, and latest GitHub release.
- Fetch source and tap remotes. Inspect both working trees before making changes.
- Use the requested version. Otherwise choose the next version from the actual changes and state the choice.
- Review changes since the previous tag. Include only intended release changes and preserve unrelated local work.
- Check whether the intended tag, release, or asset already exists before creating it.

Useful inspection commands:

```sh
git fetch origin --tags
git status --short --branch
gh release list --repo CaliLuke/autok-stack --limit 5
gh api repos/CaliLuke/homebrew-stack/contents/Formula/stack.rb --jq .content | base64 --decode
```

Use `brew --repository caliluke/stack` to locate an installed tap checkout.
If no checkout exists, clone the tap under an ignored workspace directory such as `.tmp/homebrew-stack`.
Do not recreate the tap repository.

## 2. Validate the intended source

Set `STACK_RELEASE_VERSION` to the selected version without its `v` prefix.
Run these commands from the source repository root:

```sh
mkdir -p .tmp/releases
go test ./...
go vet ./...
go build ./...
go build -ldflags "-X main.version=${STACK_RELEASE_VERSION}" -o .tmp/releases/stack .
.tmp/releases/stack --version
.tmp/releases/stack --help
git diff --check
```

Check that the version output matches the selected version.
Help and version checks must exit without finding a service configuration or starting services.
For UI changes, also follow `../tui-snapshot-iteration/SKILL.md` and inspect the relevant snapshots.
Snapshots cannot validate terminal colors or mouse behavior in a real terminal.

Keep installation instructions current. Write release notes from the actual diff, including upgrade requirements and validation results.
Store the notes at `.tmp/releases/notes-vX.Y.Z.md`; do not place unpublished notes in an unrelated tracked file.
Commit the intended source changes on `main` before creating the tag.
Follow the repository's commit conventions and omit attribution trailers.

## 3. Package and publish

Use an annotated tag for a new release. Create the archive from that tag, never from the working directory:

```sh
git tag -a "v${STACK_RELEASE_VERSION}" -m "Stack v${STACK_RELEASE_VERSION}"
git archive --format=tar.gz \
  --prefix="autok-stack-${STACK_RELEASE_VERSION}/" \
  -o ".tmp/releases/autok-stack-${STACK_RELEASE_VERSION}.tar.gz" \
  "v${STACK_RELEASE_VERSION}"
shasum -a 256 ".tmp/releases/autok-stack-${STACK_RELEASE_VERSION}.tar.gz"
tar -tzf ".tmp/releases/autok-stack-${STACK_RELEASE_VERSION}.tar.gz"
```

Check that the archive includes `LICENSE`, `go.mod`, `go.sum`, and the application source.
The archive must exclude local binaries, `.tmp`, and consumer service configurations.
Record the checksum of the exact file to upload.

For an authorized publication:

```sh
git push origin main "refs/tags/v${STACK_RELEASE_VERSION}"
gh release create "v${STACK_RELEASE_VERSION}" \
  ".tmp/releases/autok-stack-${STACK_RELEASE_VERSION}.tar.gz" \
  --repo CaliLuke/autok-stack --verify-tag \
  --title "Stack v${STACK_RELEASE_VERSION}" \
  --notes-file ".tmp/releases/notes-v${STACK_RELEASE_VERSION}.md"
```

Download the published asset into a separate directory and compare its SHA-256 with the local archive before updating the tap.
Use the release asset URL, not GitHub's automatically generated source archive URL.

## 4. Update and test the tap

Update the formula's release asset URL and SHA-256. Preserve these requirements:

- `license "MIT"` and `depends_on "go" => :build`.
- Linux dependencies on `lsof` and `procps`.
- The conflict with `haskell-stack`, which also installs a `stack` executable.
- `std_go_args(ldflags: "-X main.version=#{version}")` for the root Go package.
- Package tests for the exact version, help output, and an empty `stack.toml` that reports missing service blocks.

Test the edited tap checkout before pushing it. Run Homebrew commands sequentially: developer commands can update shared Ruby dependencies.

```sh
HOMEBREW_NO_AUTO_UPDATE=1 brew style caliluke/stack/stack
HOMEBREW_NO_AUTO_UPDATE=1 brew audit --strict caliluke/stack/stack
HOMEBREW_NO_AUTO_UPDATE=1 brew reinstall --build-from-source caliluke/stack/stack
HOMEBREW_NO_AUTO_UPDATE=1 brew test caliluke/stack/stack
"$(brew --prefix stack)/bin/stack" --version
```

Use `brew install --build-from-source` for a first installation.
If you edited a separate clone, copy only the intended formula change into the installed tap checkout after checking for local changes there.
Keep auto-update disabled during these checks so it cannot replace the formula under test.
If Homebrew tooling fails before a package test starts, resolve the tooling failure and rerun the failed check.

Commit and push the validated formula change to the tap's `main` branch.
Check that the published formula has the intended URL and checksum, and that both repositories contain the intended commits.

## Partial releases and completion

If a step fails, inspect the remote state before retrying. Resume from the first incomplete step.
For an existing tag, check its commit. For an existing asset, download it and check its checksum.
Reuse matching published artifacts. Do not move a published tag or overwrite a published asset to fix a mismatch.
If published source or archive bytes are wrong, explain the mismatch and use a new version for the correction.
If only the formula is wrong, correct the formula without replacing the release asset.

Report the release link, version, installation command, and validation results.
For existing users, give `brew update` followed by `brew upgrade caliluke/stack/stack`.
Check `which -a stack` before claiming the user's default command runs the new version.
An older `~/.local/bin/stack` can take precedence over Homebrew. Report that fact without deleting the old binary or changing `PATH` automatically.
If publication is incomplete, identify the completed artifacts and the remaining step.
