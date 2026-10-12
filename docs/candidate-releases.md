# Candidate releases

## Normal workflow

1. Create `candidate-v1.2.3`. Push changes directly or merge feature branches
   into it. Every push tests the repository and reproducibly builds Linux and
   macOS archives for AMD64 and ARM64. A successful run publishes or replaces
   the GitHub **Pre-release** named **Interceptor Sandbox 1.2.3** at tag
   `v1.2.3`.
2. Wait for the candidate push's **publish** job to succeed, then open a pull
   request from `candidate-v1.2.3` to `main`. Bring `main` into the candidate
   before merging so that the merge does not change the candidate's files.
3. Merge the pull request. The merged event promotes that same GitHub release
   to stable. Its tag and uploaded bytes stay unchanged. Ordinary pushes to
   `main` verify the code but do not build replacement release archives.

The numeric executable version remains `1.2.3`; pre-release is the GitHub
release status, not a version suffix. The tag points to the candidate source
commit. Merge, squash, and rebase merges work when the resulting file tree is
identical to the published candidate.

## Repository setup

The workflow normally uses the job-scoped `GITHUB_TOKEN`. GitHub can require
workflow permission when a candidate changes workflow files relative to the
default branch. In that case, configure `RELEASE_TOKEN` as a repository secret
with Contents read/write and Workflows write; it must also be able to read pull
requests for promotion.

Rolling pre-releases require release immutability to be disabled and repository
rules to allow the publisher to create and update `v*` candidate tags. Protect
candidate branches and release workflow changes so only trusted maintainers can
publish. No repository settings or secrets are created by this implementation.

## Published files

Each release contains four archives:

- `linux-amd64.tar.gz`
- `linux-arm64.tar.gz`
- `darwin-amd64.tar.gz`
- `darwin-arm64.tar.gz`

Each archive contains `interceptor`, `release.json`, and `LICENSE`. The release
also contains `candidate.json`, which binds the archives to their source commit,
and `SHA256SUMS`. Promotion verifies the exact asset names and GitHub-reported
SHA-256 digests before changing the release from pre-release to stable.

## Failure and retry

- Failed tests or builds do not publish a candidate.
- An old workflow run cannot replace a newer candidate branch head.
- Replacement temporarily makes the release a draft. If upload is interrupted,
  rerun the candidate workflow to repair it.
- A missing or changed asset, changed tag, wrong candidate head, or different
  merge tree blocks promotion. Resolve differences before merging; do not rebuild
  on `main` to bypass the check.
- Once stable, a version cannot be replaced by another candidate push. Use a new
  version.
- Closed but unmerged pull requests, fork pull requests, ordinary `main` pushes,
  and direct local merges pushed to `main` do not promote a release.
- A manual workflow run on a candidate branch rebuilds and republishes its current
  head.

Run `make test` locally to exercise candidate replacement, failure recovery, and
promotion against a simulated GitHub service. Local tests do not publish releases
or configure GitHub.
