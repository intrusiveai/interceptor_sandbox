# Release management

These instructions are for Interceptor Sandbox maintainers publishing and
promoting releases. They are not part of installing or running Interceptor.

## Candidate releases and promotion

Push to `candidate-v1.2.3`, including by merging another branch into it, to run
the repository tests and reproducibly build Linux and macOS archives for AMD64
and ARM64. A successful run publishes or replaces the GitHub **Pre-release**
named **Interceptor Sandbox 1.2.3** at tag `v1.2.3`. It then publishes that
candidate's `rc` catalog entry through the releases service and waits for the
service's public-delivery verification to pass.

Wait for the candidate workflow to finish before opening a pull request from
`candidate-v1.2.3` to `main`. Bring `main` into the candidate first so that the
merge does not change its file tree. Merging the same-repository pull request
promotes the existing GitHub release without rebuilding or moving its tag. It
also republishes the same artifact identities with the catalog label `release`,
which makes the version eligible for the public website listing. The reviewed
candidate merge is Interceptor's production-promotion gate.

The executable version is `1.2.3`; pre-release is a GitHub release status rather
than a version suffix. The tag remains on the candidate source commit. Merge,
squash, and rebase merges are supported when their resulting tree is identical
to that commit. Direct pushes to `main` verify code but never promote a release.

## GitHub release contents

The managed release contains these immutable release files:

- `linux-amd64.tar.gz`
- `linux-arm64.tar.gz`
- `darwin-amd64.tar.gz`
- `darwin-arm64.tar.gz`
- `candidate.json`
- `SHA256SUMS`

Each platform archive contains `interceptor`, `release.json`, and `LICENSE`.
`candidate.json` binds the archives to their source commit. Promotion verifies
the exact asset names and GitHub-reported SHA-256 digests.

`release-index-entry.json` is mutable publication metadata outside that immutable
set. The workflow derives it from the actual four GitHub archive assets, including
their direct download URLs and digests. Candidate entries use `tags: ["rc"]`;
stable entries retain the exact artifact identities and use `tags: ["release"]`.

## One-time repository setup

The workflow normally uses the job-scoped `GITHUB_TOKEN`. GitHub can require
workflow permission when a candidate changes workflow files relative to the
default branch. In that case, configure `RELEASE_TOKEN` as a repository secret
with Contents read/write and Workflows write; it must also be able to read pull
requests for promotion.

Rolling pre-releases require GitHub release immutability to be disabled and rules
that allow the publisher to create and update `v*` candidate tags. Protect
`candidate-v*`, `v*`, and release workflow changes from unauthorized updates.
Both `.github/workflows/publish-catalog.yml` and its helper must exist on the
default branch and the selected release tag.

Configure the `production` environment in `intrusiveai/interceptor_sandbox` with:

| Variable | Purpose |
| --- | --- |
| `AWS_REGION` | Region of the existing releases service. |
| `CONTENT_BUCKET` | Existing releases content bucket. |
| `CLOUDFRONT_DISTRIBUTION_ID` | Existing releases distribution. |
| `CATALOG_PUBLISHER_ROLE_ARN` | Interceptor's catalog-only role from the releases service. |

The environment must permit protected `v*` tags and retain its configured
reviewers. The tagged caller invokes the reusable publisher at reviewed commit
`ba4049627fa4a6602c5ea63efab6fb74c8643a61`. Only that reusable workflow receives
OIDC permission. No AWS access-key secret, cross-repository write token, site
deployment, or per-release infrastructure change is required.

## Catalog publication and recovery

The parent release workflow uploads `release-index-entry.json`, dispatches
`.github/workflows/publish-catalog.yml` at the published `v*` tag, correlates the
resulting run, and waits for success. The tagged caller independently rechecks
the source, release state, artifact inventory, URLs, and digests before invoking
the releases service. A dispatch acknowledgement, skipped run, failed run, or
timeout does not count as publication.

The public `/docs/releases/interceptor_sandbox.json` catalog retains candidates
and history. Candidates are readable but hidden from the website; only the first
two entries labeled exactly `release` are displayed. The publisher merges one
entry and refuses to demote or replace the artifacts of a final version. Never
upload a locally reconstructed full catalog.

If GitHub publication succeeds but catalog publication fails, inspect the linked
catalog run and correct that failure without rebuilding stable artifacts. Rerun
the failed parent job, or retry from a reviewed checkout with:

```sh
GITHUB_REPOSITORY=intrusiveai/interceptor_sandbox \
  python3 scripts/publish_release.py catalog-sync \
  --tag v1.2.3 --source FULL_CANDIDATE_COMMIT_SHA
```

The command needs an authenticated GitHub token with Contents and Actions write.
Use the tagged candidate commit, not the `main` merge commit. A stable retry
revalidates the recorded promotion pull request and cannot revert the catalog to
`rc`. Publication waits up to 30 minutes, including environment approval; a known
run is cancelled on timeout. Confirm timed-out runs have stopped before retrying.

## Failure behavior and local verification

- Failed tests or builds do not publish a candidate.
- An old run cannot replace a newer candidate branch head.
- Interrupted asset replacement leaves a draft; rerun the candidate workflow.
- Missing or changed assets, a changed tag, the wrong candidate head, or a
  different merge tree blocks promotion.
- A stable version cannot be replaced by a later candidate push; use a new version.
- Closed but unmerged pull requests, fork pull requests, ordinary `main` pushes,
  and direct local merges pushed to `main` do not promote.

Run `make test` locally to exercise rolling release replacement, failure recovery,
catalog derivation, candidate-to-stable metadata continuity, and promotion against
a simulated GitHub service. Local tests do not publish or configure GitHub, AWS,
the releases service, or the public website.
