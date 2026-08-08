# CLAUDE.md — Blackfuel fork notes

This file collects the fork-specific workflows that agents (and humans) should
know before touching this repo. Upstream contributing rules live in
`CONTRIBUTING.md`.

## Fork layout

- **Active trunk is `bf/v0.7`**, not `main` (which tracks upstream). PRs target
  `bf/v0.7`; our changes carry `bf/` or topic branch prefixes and merge there.
- Every merge to `bf/v0.7` triggers a release build tagged `v0.7.0-bfNN`
  (images published to `ghcr.io/blackfuel-ai/ai-gateway-{controller,extproc}`).

## Validating extproc/translator changes end-to-end (ai-platform local-dev)

Unit tests (`make test`) are necessary but not sufficient for translator
changes: the anthropic↔openai translation (`internal/translator/anthropic_openai.go`,
`openai_helper.go`) only shows its real behaviour through a gateway data path.
The deterministic end-to-end loop — test-first — is:

1. **Write the failing guard in the ai-platform repo first.**
   `ai-platform/local-dev/envoy-gateway/` runs a local minikube AI-gateway
   cluster with a programmable mock upstream
   (`local-dev/envoy-gateway/manifests/17-streaming-usage.yaml`). The mock's
   response shape is selected per request: a `MODE:<name>` prefix in the last
   user message (survives anthropic→OpenAI translation as plain content, use
   this from httpx tests) or the model name `streaming-usage-<mode>` with one
   `AIGatewayRoute` per model (needed for real-CLI tests that can't carry the
   prefix). Add a mode for the production shape you are fixing (vLLM chunk
   patterns, vendor fields, usage placement, …) and a pytest suite under
   `tests/` asserting the expected `/v1/messages` (or other) surface behavior.
   Mark guards `pytest.mark.xfail(strict=True)`: against the released image
   they XFAIL (CI green); once a fixed release ships and the chart tag in
   `local-dev/envoy-gateway/manifests/09-ai-gateway-ci-override.yaml` bumps, the
   XPASS becomes a CI failure that forces the marker drop in the same PR —
   the guard can never be silently stranded. See
   `tests/test_anthropic_stop_reason_usage.py` (ai-platform PR #9019) for the
   canonical example, including the companion "control" tests that must pass
   on the broken AND the fixed build (negative cases / fallbacks).
   Open that ai-platform PR with the test only.
2. **Fix here, then validate against a local build** — before merging:
   build the controller AND extproc images from your branch, swap them into the
   minikube cluster, and watch the guards flip to XPASS. The full procedure
   (build flags, image load force-reload gotcha, `imagePullPolicy: Never`,
   cleanup) is `ai-platform/local-dev/envoy-gateway/docs/local-ai-gateway-build.md`.
3. **Validate, then merge.** Mixing released and locally built images per side
   does not work, see "Version stamp coupling" below.
4. **After merge + release**, go back to the ai-platform PR: bump the image
   tag in `09-ai-gateway-ci-override.yaml`; the strict-XPASS failure enforces
   dropping the xfail markers in the same commit.

### Version stamp coupling (controller ↔ extproc)

The extproc rejects filter configs whose embedded version differs from its own
build stamp (`config version mismatch: ... CrashLoopBackOff`). Released
`ghcr.io` images all stamp `dev` (CI builds don't inject git ldflags), while a
local `make docker-build.*` stamps `git describe` output
(e.g. `92978e8a (v0.7.0+bf25, +3)`). Consequence: **even for extproc-only
changes, build and swap BOTH `local/ai-gateway-controller:local` and
`local/ai-gateway-extproc:local`** — released/released and local/local are the
only working combinations.

### Build prerequisites

- `go` must be on `PATH` when invoking `make docker-build.*` — the Makefile
  derives `GOARCH_LIST` from `$(shell go env GOARCH)`; with Go missing the
  binary build silently no-ops and docker fails with
  `"/out/extproc-linux-amd64": not found`.
- Do not commit the image override edit to
  `09-ai-gateway-ci-override.yaml` — those images exist only in your minikube.

## Release flow

Merging a fix PR to `bf/v0.7` cuts `v0.7.0-bfNN(+1)` automatically. The
ai-platform local-dev override and any xfail guards pick it up via the tag
bump described above.
