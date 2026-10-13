# Product-local release publisher

Commit this complete directory to the product repository as
`tools/release-publisher`. Copy the applicable files from `publication/examples/`
to that repository's `.github/workflows/`. These are ordinary jobs that check out
only the product repository. There is no runtime download, action, reusable
workflow, or Go dependency from the private releases repository.

`publisher-source.json` identifies the exact source commit, product, file list,
and SHA-256 of each copied file. A `working_tree: true` export is for local tests
only. Obtain production exports from a reviewed commit with
`scripts/export-publisher.py` in an authorized developer's releases checkout;
updates use a new export and a reviewed diff in the product repository. Replace
the entire old publisher directory with the reviewed export, retaining local
integration changes explicitly rather than silently overwriting them. Do not add
a token that lets product workflows read the private releases repository.

## Dependencies and offline verification

Linux GitHub runner, Bash, Go 1.26.5 (see `go.mod`), AWS CLI v2 with conditional
`put-object` support (`--if-match` and `--if-none-match`), jq, curl, Python 3 for
tests, and standard shell tools including cmp. Go packages use only the standard
library. The module name is a local import namespace; Go does not fetch that
repository. Download products carry catalog code only; Attack Harness also
carries image, record, and HTTPS verification code. No website, Terraform, or
external Go dependency is included.

From this directory, with Go already installed:

```sh
export GOWORK=off GOPROXY=off GOTOOLCHAIN=local
go test -race ./...
go vet ./...
python3 -m unittest discover -s scripts/tests
```

Tests run actual merge/validation and publication scripts against simulated S3
and a local HTTP server. They cover candidate creation/replacement, unchanged
promotion, stale demotion/final replacement rejection, concurrent writes, bounded
retries, and history/order. Attack Harness also tests pending-qualification
admission, canonical records, create-only/idempotent/conflicting writes, image
identity over HTTP, and HTTPS delivery checks. They do not perform AWS writes or
prove a live GitHub OIDC exchange.

## Catalog publication (all products)

The ordinary job must run in `.github/workflows/publish-catalog.yml`, on a
protected `refs/tags/v*` ref, using the product's `production` environment. Set
`AWS_REGION`, `CONTENT_BUCKET`, `CLOUDFRONT_DISTRIBUTION_ID`, and
`CATALOG_PUBLISHER_ROLE_ARN`. Its one-time custom OIDC subject template, in order,
is `repository_id`, `repository_owner_id`, `environment`, `ref`, `workflow_ref`.
Assume the configured role with audience `sts.amazonaws.com`, then:

```sh
bash scripts/publish-catalog.sh /absolute/path/release-index-entry.json operator_sandbox
# Or interceptor_sandbox / attack_harness, matching this repository.
```

Generate the entry from actual published artifacts. Candidates use `["rc"]`;
qualified releases use `["release"]`. Promotion of an existing version retains
all artifact URLs, filenames, platforms, and digests, or the exact image reference
and digest. New versions prepend; updates keep their position. Final entries
cannot change artifact identity or lose the release label. The product pipeline
owns qualification and continuity when promoting to a *different* version.
The catalog publisher neither downloads all assets nor runs QA.

## Attack Harness records

Use `.github/workflows/approve-candidate.yml` with the same environment/tag/subject
settings and `RECORD_PUBLISHER_ROLE_ARN` (the same existing role as its catalog).
Produce real per-platform build facts, then run before assuming AWS credentials:

```sh
# Export these from the build job, not hand-written guesses:
# RECORD_JSON IMAGE_REFERENCE PLATFORM PLATFORM_MANIFEST_DIGEST
# IMAGE_CONFIG_DIGEST INDEX_DIGEST SOURCE_COMMIT EVIDENCE_URL
bash scripts/prepare-record.sh /absolute/path/record.json
# After the scoped OIDC role has been assumed:
bash scripts/publish-record.sh /absolute/path/record.json
```

`RECORD_JSON` is the canonical seven-field record; the key is its image **config**
digest, not an OCI index/manifest digest. `EVIDENCE_URL` must be public under
`https://github.com/intrusiveai/attack_harness/`. The verifier checks real GHCR
manifest/config/platform/index/source identity and public evidence reachability;
the source pipeline must validate the evidence contents and source provenance.
An optional index digest must be supplied when using a multi-platform index.
Candidate admission is allowed before Operator QA; only later promotion requires
production qualification. Never add tags or status fields to a record.

Both scripts check S3 readback and public delivery. Publication failures after a
successful write may be retried safely with the same inputs. Catalog scripts
re-read and re-merge concurrent changes; records permit existing identical bytes
only. Routine publication needs no Terraform, site build, or site deployment.
