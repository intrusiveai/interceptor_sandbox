# Model-response qualification

The release workflow runs `scripts/qualify_release.py --model-response
--adapter-language python` against downloaded Linux archives on both native
architectures. The existing target image, provider fixture, native smoke client,
contract identity checks and archive evidence verifier are reused. Publication
continues to depend on the complete packaged-runtime job.

For local testing, build an archive from the committed source using
`scripts/build_release.py` (macOS imports a matching native Linux archive), build
`examples/local-api-smoke`, and supply `--archive`, `--version`, `--source`,
`--image python:3.12-slim`, `--client` and `--output` to the qualification script.
Keep local outputs in `../maintainers/interceptor_sandbox/`, outside the checkout.
Use the same source revision for the client and archive. The invoked CLI runs
outside the source tree without Go in PATH. No live provider credentials are used.

Inspect the retained native archive, original/delivered blobs, diagnostic feedback,
provider calls and target results. Expected results are a shipped order, a
`KeyError` for the malformed name and a `JSONDecodeError` for malformed arguments.
These observations show dispatcher rejection, not code execution. The script
removes its sessions, wrapper and target images. Unit/API tests additionally
cover storage failures, partial writes and the remaining generation codecs.
