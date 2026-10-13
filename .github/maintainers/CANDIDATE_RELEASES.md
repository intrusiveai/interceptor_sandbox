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

Complete these steps with administrator access to
[`intrusiveai/interceptor_sandbox`](https://github.com/intrusiveai/interceptor_sandbox).
All repository settings below belong to this repository unless stated otherwise.
The releases service must already be deployed; step 5 explains where to obtain
its configuration values.

### 1. Make the release workflows available on GitHub

1. Merge or push the release-workflow commits to the GitHub default branch,
   `main`.
2. In GitHub's **Code** tab, select `main` and confirm that
   `.github/workflows/verify-release.yml`,
   `.github/workflows/publish-catalog.yml`, and `scripts/publish_release.py` exist.
3. Create candidate branches from a commit containing those files. The catalog
   caller must exist on both the default branch and the candidate's eventual
   `v*` tag for GitHub to dispatch it.

### 2. Allow the workflow's actions

1. Open **Settings → Actions → General** and ensure Actions is enabled.
2. If the repository or organization restricts allowed actions, allow the pinned
   `actions/checkout`, `actions/setup-go`, and `actions/upload-artifact` actions,
   plus `intrusiveai-engineering/releases/.github/workflows/publish-catalog.yml`.
   The reusable publisher also uses `aws-actions/configure-aws-credentials`.
3. Save any changes. If an organization policy locks these settings, have the
   organization owner update the allowlist.

GitHub creates `GITHUB_TOKEN` for each job automatically. **Do not create a secret
named `GITHUB_TOKEN`.** The checked-in workflows request Contents and Actions
write access only for the publishing jobs; the repository's default workflow
permission can remain read-only. The catalog caller uses the reviewed publisher
at `ba4049627fa4a6602c5ea63efab6fb74c8643a61`, which must be accessible from this
repository.

### 3. Decide whether to add `RELEASE_TOKEN`

Start with the automatic `GITHUB_TOKEN` if candidate workflow files are already
on `main` and your tag rules permit it. No token creation is needed in that case.
Add `RELEASE_TOKEN` if a candidate will include workflow changes not yet on
`main`, GitHub rejects its tag write for missing workflow permission, or the tag
rules in step 4 require a permitted maintainer identity.

To add the optional token:

1. In your personal GitHub account, open **Settings → Developer settings →
   Personal access tokens → Fine-grained tokens → Generate new token**.
2. Name it `interceptor-release`, choose an expiration, and select **intrusiveai**
   as the resource owner. Under **Repository access**, choose **Only select
   repositories**, then select **interceptor_sandbox**.
3. Under **Repository permissions**, grant **Contents: Read and write**,
   **Workflows: Read and write**, and **Pull requests: Read-only**. Leave other
   permissions at their defaults. The token owner must have write access to the
   repository and any required tag-rule bypass.
4. Generate and copy the token. If organization approval is required, have an
   organization owner approve it before using it.
5. Return to the repository's **Settings → Secrets and variables → Actions →
   Secrets → New repository secret**. Name the secret exactly `RELEASE_TOKEN`,
   paste the token as its value, and click **Add secret**. Use a repository
   secret, since the GitHub release jobs do not run inside an environment.
6. Renew the token and update this secret before it expires. The workflow selects
   it automatically when present; no YAML edit is needed. Catalog dispatch still
   uses the automatic `GITHUB_TOKEN`, so this optional token needs no Actions
   permission.

See GitHub's [token creation instructions](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens#creating-a-fine-grained-personal-access-token).

### 4. Permit rolling candidate tags and release assets

1. Open **Settings → General → Releases**. Leave **Enable release immutability**
   unchecked; disable it if enabled. If enforced by the organization, ask the
   organization owner to exempt this repository before publishing candidates.
   Rolling candidates replace assets and move their `v*` tag on each update.
2. Open **Settings → Rules → Rulesets** and inspect the rules targeting `v*` tags.
   The publishing identity must be allowed to create and update these tags,
   including moving an existing tag to the next candidate commit.
3. If those rules restrict creation, updates, or force pushes, configure their
   **Bypass list** for the publishing identity. For a personal `RELEASE_TOKEN`,
   add an authorized maintainer team or role containing its owner and select
   **Always allow**. Token permissions alone do not bypass a ruleset. Keep the
   restrictions for other users; do not assume the automatic token can bypass them.
4. Review branch rules for `main` and `candidate-v*` so that only trusted
   maintainers can approve release-workflow changes or push candidate code.
   Preserve direct candidate pushes if you intend to use that supported path.

GitHub describes these controls in [ruleset setup](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/creating-rulesets-for-a-repository).

### 5. Create the `production` environment and add service values

1. Obtain the deployed service values from the releases-service administrator.
   If you administer its initialized production Terraform checkout, run these
   read-only commands from the `intrusiveai-engineering/releases` checkout:

   ```sh
   terraform -chdir=infra/service output -raw content_bucket_name
   terraform -chdir=infra/service output -raw cloudfront_distribution_id
   terraform -chdir=infra/service output -json catalog_publisher_role_arns
   ```

   Take the role ARN from the map's `interceptor_sandbox` entry. Read the bucket
   region from the deployed production configuration's `aws_region` value. If the
   role is missing, the service administrator must complete the service's
   [catalog publisher setup](https://github.com/intrusiveai-engineering/releases/blob/ba4049627fa4a6602c5ea63efab6fb74c8643a61/docs/deployment.md#enabling-direct-catalog-publication-on-an-existing-deployment).
   Do not substitute Operator's or Attack Harness's role.
2. In **interceptor_sandbox → Settings → Environments**, open `production`, or
   click **New environment**, enter exactly `production`, and click
   **Configure environment**.
3. Under **Deployment branches and tags**, choose **Selected branches and tags**.
   Click **Add deployment branch or tag rule**, select **Tag**, enter `v*`, and
   add the rule. This is a tag rule, not a `candidate-v*` branch rule.
4. Retain existing required reviewers. If your release policy requires approval,
   add its maintainers under **Required reviewers** and save. An eligible reviewer
   must approve each waiting catalog run within its 30-minute publication timeout.
5. Under **Environment variables**, click **Add variable** for each row below,
   enter the exact name and deployed value, and save it. These are variables,
   not secrets.

   | Variable | Value to enter |
   | --- | --- |
   | `AWS_REGION` | Deployed content bucket's region. |
   | `CONTENT_BUCKET` | `content_bucket_name` output. |
   | `CLOUDFRONT_DISTRIBUTION_ID` | `cloudfront_distribution_id` output. |
   | `CATALOG_PUBLISHER_ROLE_ARN` | `interceptor_sandbox` value from `catalog_publisher_role_arns`. |

Only the reusable publisher obtains temporary AWS credentials through GitHub
OIDC. Do not add AWS access keys. The environment belongs to
`intrusiveai/interceptor_sandbox`, not `intrusiveai-engineering/releases`.
See GitHub's [environment setup instructions](https://docs.github.com/en/actions/how-tos/deploy/configure-and-manage-deployments/manage-environments).

### 6. Confirm setup with the first candidate

1. Push the intended `candidate-v<version>` branch and open **Actions →
   verify-release-candidates**. Wait for verification and builds to finish and
   for the publish job to dispatch the catalog run.
2. Open the resulting **publish Interceptor release catalog** run. If it is
   waiting for environment approval, use **Review deployments**, select
   `production`, and approve it after reviewing the candidate.
3. Confirm both runs finish successfully. Under **Releases**, verify that
   `v<version>` is marked **Pre-release** and includes all four platform archives
   and `release-index-entry.json`.
4. Open the [public Interceptor catalog](https://releases.intrusive.ai/docs/releases/interceptor_sandbox.json)
   and confirm the version has `tags: ["rc"]` with the same archive URLs and
   digests. It should remain hidden from the stable download listing. A dispatch
   acknowledgement alone does not confirm successful setup.

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
