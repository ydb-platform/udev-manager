# Release flow

The changelog is the version source of truth. This follows the operator's
release-PR flow, without building or uploading images, charts, or binaries.

## Record a change

Install Changie v1.26.0 and run `changie new`. Commit the resulting YAML fragment
under `.changes/unreleased/` with the code change. These are unreleased changes,
not uncommitted files: GitHub Actions can only see files committed to the repo.

## Prepare and publish

1. Run **create-release-pr** in GitHub Actions on `main`; select patch, minor,
   or major. It runs `changie batch`, merges `CHANGELOG.md`, and opens a
   `release/vX.Y.Z` PR. No tag or release is created yet.
2. Review the notes and version. GitHub may require **Approve workflows to run**
   for CI on the bot-created PR. Merge the release PR normally after CI passes.
3. **publish-release** tags that exact merged commit as `X.Y.Z` and creates a
   GitHub release using `.changes/vX.Y.Z.md`. It uploads no build artifacts;
   GitHub's automatic source archives remain available.

The repository must allow GitHub Actions to create pull requests. No additional
token is required. Merge as a maintainer, not through another workflow's
`GITHUB_TOKEN`, which suppresses the `pull_request: closed` trigger.

Prepare one release PR at a time. If more fragments land before it merges,
rerun preparation with the same bump type to update the existing release branch,
then review and approve CI again. Do not manually rename its branch: the publisher
checks that the branch version matches the changelog.

To correct the version before publication, close the unmerged release PR and
rerun preparation with the desired bump type. To correct release notes, edit
the version file and run `changie merge` in that release PR.

After publication, tags are immutable. Publish a new version for code changes.
If publication fails, rerun the failed **publish-release** workflow; it keeps
the original merge commit, refuses conflicting tags, and leaves existing
releases unchanged. A tag without a release can be completed by the rerun.

## Initial version

Existing tags such as `0.43` remain unchanged. `.changes/v0.43.0.md` is only a
baseline for Changie. The next patch is `0.43.1`; the next minor is `0.44.0`.
Merging this setup PR does not publish the baseline release.
