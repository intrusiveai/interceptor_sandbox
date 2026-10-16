# Release management

These instructions are for Interceptor Sandbox maintainers publishing and
promoting releases. They are not part of installing or running Interceptor.

## Candidate releases and promotion

Push to `candidate-v1.2.3`, including by merging another branch into it, to run
the repository tests and reproducibly build Linux and macOS archives for AMD64
and ARM64. A successful run publishes or replaces the GitHub **Pre-release**
named **Interceptor Sandbox 1.2.3** at tag `v1.2.3`. It then publishes that
candidate's `rc` catalog entry directly to the releases bucket using this
repository's local publisher, and waits for public-delivery verification to pass.
No workflow, checkout, site build, or deployment in the private releases repository
is involved.

Wait for the candidate workflow to finish and complete QA against those exact
downloaded artifacts before opening a pull request from
`candidate-v1.2.3` to `main`. Bring `main` into the candidate first so that the
merge does not change its file tree. Merging the same-repository pull request
promotes the existing GitHub release without rebuilding or moving its tag. It
also republishes the same artifact identities with the catalog label `release`,
which makes the version eligible for the public website listing. The reviewed
candidate merge is Interceptor's production-promotion gate: reviewers must confirm
QA of the current candidate, not merely passing build checks. Any candidate
rebuild requires qualification of the replacement bytes before promotion.

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
macOS archives additionally contain `interceptor-linux`, built for the same CPU
from the same source revision and release version. `release.json` covers its
filename, Linux platform and SHA-256 as `linux_companion`. Do not split this layout
when distributing or installing macOS releases. Reproducibility comparisons and
whole-archive asset digests include both executables. CI downloads its own archives,
checks the complete inventory/digests, compares each companion to the matching
Linux release executable, and runs native Linux wrapper qualification before
candidate publication. Promotion retains those exact complete archives.
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
The releases service must already be deployed. Step 5 configures its existing
role and environment; step 6 configures the required GitHub identity format.

### 1. Make the release workflows available on GitHub

1. Merge or push the release-workflow commits to the GitHub default branch,
   `main`.
2. In GitHub's **Code** tab, select `main` and confirm that
   `.github/workflows/verify-release.yml`,
   `.github/workflows/publish-catalog.yml`, `scripts/publish_release.py`, and the
   complete `tools/release-publisher` directory exist.
3. Create candidate branches from a commit containing those files. The catalog
   workflow and local publisher must exist on both the default branch and the
   candidate's eventual `v*` tag for GitHub to dispatch it.

### 2. Allow the workflow's actions

1. Open **Settings → Actions → General** and ensure Actions is enabled.
2. If the repository or organization restricts allowed actions, allow the pinned
   `actions/checkout`, `actions/setup-go`, and `actions/upload-artifact` actions
   (or enable **Allow actions created by GitHub**). Also allow
   `aws-actions/configure-aws-credentials@e1253824e5c10ff9df46874f81ed3ec929e19cfd`.
   Use the exact pins in both workflow files if your policy permits individual
   SHAs rather than GitHub-owned actions generally. Remove the obsolete
   `intrusiveai-engineering/releases/.github/workflows/publish-catalog.yml` exception;
   this repository no longer calls it or needs access to that private repository.
3. Save any changes. If an organization policy locks these settings, have the
   organization owner update the allowlist.

GitHub creates `GITHUB_TOKEN` for each job automatically. **Do not create a secret
named `GITHUB_TOKEN`.** The checked-in workflows request Contents and Actions
write access only for the publishing jobs; the repository's default workflow
permission can remain read-only. The catalog job requests Contents and Pull
requests read access plus `id-token: write` for temporary AWS credentials.

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

1. Ask the releases-service administrator to confirm that the existing Interceptor
   role's trust policy has been migrated to the product-local workflow and custom
   subject described in step 6. It must accept only this repository's numeric
   identity, the `production` environment, `v*` tags, the exact
   `.github/workflows/publish-catalog.yml` reference, and audience
   `sts.amazonaws.com`. Keep its existing catalog-only permissions and role ARN.
   This is a one-time IAM migration, not a routine release or site deployment.
   Obtain the deployed service values from that administrator.
   If you administer its initialized production Terraform checkout, run these
   read-only commands from the `intrusiveai-engineering/releases` checkout:

   ```sh
   terraform -chdir=infra/service output -raw content_bucket_name
   terraform -chdir=infra/service output -raw cloudfront_distribution_id
   terraform -chdir=infra/service output -json catalog_publisher_role_arns
   ```

   Take the role ARN from the map's `interceptor_sandbox` entry. Read the bucket
   region from the deployed production configuration's `aws_region` value. If the
   role is missing or still trusts a centralized reusable workflow, the service
   administrator must complete the product-local publisher migration first.
   Do not substitute Operator's or Attack Harness's role.
2. In **interceptor_sandbox → Settings → Environments**, open `production`, or
   click **New environment**, enter exactly `production`, and click
   **Configure environment**.
3. Under **Deployment branches and tags**, choose **Selected branches and tags**.
   Click **Add deployment branch or tag rule**, select **Tag**, enter `v*`, and
   add the rule. This is a tag rule, not a `candidate-v*` branch rule. Remove any
   branch rules (including `main`) or other tag patterns from this environment
   after checking for other consumers; it should permit only protected `v*` tags.
4. Retain existing required reviewers. If your release policy requires approval,
   add its maintainers under **Required reviewers** and save. An eligible reviewer
   must approve each waiting catalog run within its 30-minute publication timeout.
5. Under **Environment variables**, click **Add variable** for each row below,
   enter the exact name and deployed value, and save it. These are variables,
   not secrets.

   | Variable | Value to enter |
   | --- | --- |
   | `AWS_REGION` | `us-west-2` |
   | `CONTENT_BUCKET` | `intrusiveai-releases` |
   | `CLOUDFRONT_DISTRIBUTION_ID` | `E1ET1MODBEJLY6` |
   | `CATALOG_PUBLISHER_ROLE_ARN` | `arn:aws:iam::389547485478:role/releases-interceptor_sandbox-catalog-publisher` |

   These are the reviewed production values; confirm them against the deployed
   service before saving. Do not add a record-publisher role or AWS access keys.

Only the local catalog job obtains temporary AWS credentials through GitHub
OIDC. The environment belongs to
`intrusiveai/interceptor_sandbox`, not `intrusiveai-engineering/releases`.
See GitHub's [environment setup instructions](https://docs.github.com/en/actions/how-tos/deploy/configure-and-manage-deployments/manage-environments).

### 6. Set the repository's custom OIDC subject

OIDC lets GitHub obtain temporary AWS credentials. AWS checks a subject string
identifying the repository, environment, tag, and workflow file. The default
GitHub subject does not satisfy the new trust policy.

1. Inventory workflows requesting `id-token: write` and coordinate any other
   cloud roles before changing the subject. In this checkout, run:

   ```sh
   rg -n 'id-token:|environment:' .github/workflows
   ```

   This setting affects **every OIDC job in this repository**, not just releases.
   All such jobs must declare an environment for the new subject to be issued.
2. Sign in to `gh` with repository administrator access. Run this read-only
   command and retain its output with your configuration change record:

   ```sh
   gh api repos/intrusiveai/interceptor_sandbox/actions/oidc/customization/sub
   ```

3. After the administrator confirms the IAM migration in step 5, run:

   ```sh
   gh api --method PUT \
     repos/intrusiveai/interceptor_sandbox/actions/oidc/customization/sub \
     --input - <<'JSON'
   {"use_default":false,"include_claim_keys":["repository_id","repository_owner_id","environment","ref","workflow_ref"]}
   JSON
   ```

   Keep the exact key order. Use `workflow_ref`, not `job_workflow_ref`: the
   publisher is an ordinary job, not a reusable workflow.
4. Repeat the GET command from step 2 and confirm `use_default` is `false` and
   the five keys appear in the specified order. For tag `v0.0.1`, the subject
   expected by the role is:

   ```text
   repository_id:1384022338:repository_owner_id:296295422:environment:production:ref:refs/tags/v0.0.1:workflow_ref:intrusiveai/interceptor_sandbox/.github/workflows/publish-catalog.yml@refs/tags/v0.0.1
   ```

The subject binds the workflow file and ref, not its contents. Protect release
tags and review publisher/workflow changes before approving production jobs.
See GitHub's [repository OIDC configuration API](https://docs.github.com/en/rest/actions/oidc#set-the-customization-template-for-an-oidc-subject-claim-for-a-repository).

### 7. Confirm setup with the first candidate

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
5. Complete QA of the candidate, record the evidence on its reviewed pull
   request, then merge it into `main`. Approve the stable catalog job if prompted.
   Confirm the GitHub release is no longer a pre-release, its archives and tag
   are unchanged, and its catalog entry now has `tags: ["release"]`. Reload the
   [downloads website](https://releases.intrusive.ai/): the stable version should
   appear without a site build or deployment. Confirm prior catalog history is
   retained. A repeat publication of the same stable entry must be idempotent.

For an existing rolling candidate such as `candidate-v0.0.1`, first bring the
updated `main` into the candidate and let its normal candidate build publish the
new commit. Rerunning an old run executes the old workflow code. Do not manually
move an immutable or stable tag to install the publisher. The updated workflow
and bundle must be in the tagged source before a catalog retry can use them.

## Catalog publication and recovery

The parent release workflow uploads `release-index-entry.json`, dispatches
`.github/workflows/publish-catalog.yml` at the published `v*` tag, correlates the
resulting run, and waits for success. This dispatch stays in **this repository**;
it runs at the release tag to satisfy the AWS trust policy. The tagged job
independently rechecks the source, release state, artifact inventory, URLs, and
digests and validates the catalog entry before acquiring AWS credentials. It
then merges the entry using the committed local publisher. A dispatch
acknowledgement, skipped run, failed run, or timeout does not count as publication.

The public `/docs/releases/interceptor_sandbox.json` catalog retains candidates
and history. Candidates are readable but hidden from the website; only the first
two entries labeled exactly `release` are displayed. The publisher merges one
entry and refuses to demote or replace the artifacts of a final version. Never
upload a locally reconstructed full catalog. The publisher uses conditional S3
writes with bounded retries so a concurrent publication does not overwrite
another writer's changes. It checks stored bytes and headers, invalidates only
the catalog path, and verifies the public response.

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
a simulated GitHub service. It also runs the local publisher's race tests, vet,
and publication tests using simulated S3 and local HTTP delivery. The integration
test carries Interceptor's generated four-platform entry through candidate
replacement, promotion, and retry with that actual publisher. Local tests do not
publish or configure GitHub, AWS, the releases service, or the public website.

## Nonpublishing release-archive qualification

Use a working branch whose name does **not** start with `candidate-v`. Push only
that working branch, then dispatch `verify-release.yml` with `--ref <working-branch>`.
The workflow must already exist on `main`, but the run uses the selected branch's
workflow and source. Such a dispatch selects verification mode; publication is
also explicitly restricted to candidate branch pushes/dispatches. Do not dispatch
on a candidate branch for a nonpublishing test. A PR into the candidate can also
run verification without publication.

Download `release-candidate-3` from that run for macOS ARM64 (`-2` for AMD64).
Inspect its version and source using `scripts/publish_release.py verify-archive`.
On a native macOS host with matching Docker architecture and a local Linux image
containing `/bin/cat`, run:

```sh
go build -o /tmp/interceptor-release-smoke ./examples/local-api-smoke
python3 scripts/qualify_release.py --archive /absolute/path/darwin-arm64.tar.gz \
  --version 0.1.0 --source <workflow-head-sha> \
  --image busybox:1.37.0 --client /tmp/interceptor-release-smoke \
  --output /tmp/interceptor-release-qualification
```

Pull the test image beforehand if necessary. Go is used only to prepare the
existing qualification client, not by the extracted release. The output directory must not exist.
The script uses the documented command interface, extracts into a fresh temporary
directory outside the checkout, runs with only Docker on the CLI's `PATH`, builds
and verifies a wrapper, invokes the target, checks evidence, and compares the
actual in-container binary with the packaged executable. It cleans up its isolated
sessions and uniquely named Docker resources and retains result/evidence files.
It does not publish a release. Retain the CI run URL, archive SHA-256, result and
evidence; distinguish native macOS testing from the Linux CI qualification.

## Updating the local publisher

`tools/release-publisher` is the complete catalog-only export from reviewed
releases commit `c651c5fa47346f1ca02abe33791667f838b59e03`.
`publisher-source.json` records the source revision and each file's SHA-256.
The private module name is only a local Go import namespace; publication runs
with `GOWORK=off`, `GOPROXY=off`, and `GOTOOLCHAIN=local` and fetches no private code.
The export includes its upstream reference documentation and workflow example;
this file remains the repository-specific maintainer guide. Neither is included
in the standard public documentation or release archives.

1. Obtain a complete catalog-only export for `interceptor_sandbox` from an
   authorized releases maintainer at an explicitly reviewed commit. The
   maintainer runs `scripts/export-publisher.py --revision <full-commit-sha>
   --product interceptor_sandbox --output <new-directory>` in that checkout.
   Never use a `--working-tree` export for a product release.
2. Review the full diff, including removed files, and replace the old bundle
   with the complete new export and its provenance manifest. Do not edit vendor
   files in place or install only the shell script. Adapt the workflow example
   while retaining Interceptor's actual-release and promotion checks.
3. Run `make test`. Go 1.26.5, Bash, jq, curl, and Python 3 are required locally;
   publication additionally needs AWS CLI v2 with conditional `put-object`.
4. Review and commit the bundle, workflow, and any updated setup instructions.
   Deliver them to both `main` and the next candidate before its tag is built.
   Confirm live OIDC and public delivery using step 7; local tests alone cannot
   establish deployment readiness.
