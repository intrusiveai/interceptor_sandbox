#!/usr/bin/env bash
set -euo pipefail

: "${CONTENT_BUCKET:?CONTENT_BUCKET is required}"
: "${CLOUDFRONT_DISTRIBUTION_ID:?CLOUDFRONT_DISTRIBUTION_ID is required}"
: "${RELEASE_ORIGIN:=https://releases.intrusive.ai}"

entry="${1:?usage: publish-catalog.sh ENTRY_JSON [PRODUCT]}"
product="${2:-attack_harness}"
case "$product" in
  operator_sandbox|attack_harness|interceptor_sandbox) ;;
  *) echo "unsupported catalog product: $product" >&2; exit 1 ;;
esac
key="docs/releases/$product.json"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
go run ./cmd/catalog-publisher catalog merge --product "$product" --entry "$entry" --output "$work/validated.json"

published=false
for attempt in 1 2 3 4 5; do
  # Listing the exact key also distinguishes an absent catalog from access errors.
  aws s3api list-objects-v2 --bucket "$CONTENT_BUCKET" --prefix "$key" > "$work/list.json"
  current_exists=false
  merge_args=(--product "$product" --entry "$entry" --output "$work/next.json")
  condition=(--if-none-match '*')
  if jq -e --arg key "$key" 'any(.Contents[]?; .Key == $key)' "$work/list.json" >/dev/null; then
    aws s3api get-object --bucket "$CONTENT_BUCKET" --key "$key" "$work/current.json" > "$work/current-head.json"
    condition=(--if-match "$(jq -er '.ETag' "$work/current-head.json")")
    current_exists=true
    merge_args+=(--current "$work/current.json")
  fi
  go run ./cmd/catalog-publisher catalog merge "${merge_args[@]}"
  if [[ "$current_exists" == true ]] && cmp -s "$work/current.json" "$work/next.json"; then
    published=true
    break
  fi
  if aws s3api put-object --bucket "$CONTENT_BUCKET" --key "$key" \
    --body "$work/next.json" --content-type application/json \
    --cache-control 'public,max-age=0,must-revalidate' --checksum-algorithm SHA256 \
    "${condition[@]}" > "$work/result.json" 2> "$work/error.txt"; then
    jq -e '.ChecksumSHA256 and .VersionId' "$work/result.json" >/dev/null
    published=true
    break
  fi
  if ! grep -Eq 'PreconditionFailed|ConditionalRequestConflict' "$work/error.txt"; then
    cat "$work/error.txt" >&2
    exit 1
  fi
  # Another writer won. Re-read and re-merge rather than replacing its changes.
done
if [[ "$published" != true ]]; then
  echo 'catalog changed during all five attempts; retry publication from the product pipeline' >&2
  exit 1
fi

aws s3api get-object --bucket "$CONTENT_BUCKET" --key "$key" "$work/readback.json" > "$work/readback-head.json"
cmp "$work/next.json" "$work/readback.json"
jq -e '.ContentType == "application/json" and .CacheControl == "public,max-age=0,must-revalidate" and (.ContentEncoding == null)' "$work/readback-head.json" >/dev/null
aws cloudfront create-invalidation --distribution-id "$CLOUDFRONT_DISTRIBUTION_ID" --paths "/$key" >/dev/null
curl --fail --silent --show-error --max-time 20 --max-redirs 0 \
  --header 'Cache-Control: no-cache' --output "$work/public.json" "${RELEASE_ORIGIN%/}/$key"
cmp "$work/next.json" "$work/public.json"
printf 'published %s\n' "${RELEASE_ORIGIN%/}/$key"
