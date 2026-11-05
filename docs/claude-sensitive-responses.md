# Claude sensitive stop reason in Responses

## Problem and scope

At baseline `1eee59dc7144b95dfd5b8ecc81ee55c40882bf06`, the Claude-to-Responses translator classifies an upstream `sensitive` stop as a completed answer. The Chat Completions translator already maps both `refusal` and `sensitive` to content filtering. A compatible upstream can therefore return a partial structured answer with a normal HTTP 200 and `message_stop`, while Responses incorrectly reports success.

The fix adds `sensitive` to the existing incomplete/content_filter mapping shared by streaming and non-streaming conversion. Partial output and usage are preserved. No request, effort, token budget, credential routing, filtering behavior, or retry policy changes.

This is a locally reproduced protocol bug. A separate live candidate-matching diagnostic returned truncated JSON with a completed status, but its original provider stop reason is unavailable. That incident is not yet proven to have this cause.

## Verification

- Added streaming regression for sensitive, terminal event order, incomplete reason, and preserved usage.
- Added non-streaming regression using incomplete JSON, asserting incomplete/content_filter, exact partial output, and preserved input/cache/output usage.
- Both sensitive cases failed before the implementation change.
- After the fix: `go test ./internal/translator/claude/openai/responses ./internal/translator/claude/openai/chat-completions -count=1` passed.
- Server build passed: `go build -o /tmp/cdemo-cli-proxy-api-sensitive-check ./cmd/server`.
- No deployment or runtime configuration changes.

## Observability and acceptance

This restores the existing response-status contract and uses existing usage metadata; no new analytics system, telemetry event, or prompt logging is added. No secrets, real resumes, or chain-of-thought content are used by the regression fixtures.

Clients now receive an incomplete response and must handle it as such. This does not make the filtered answer complete or establish any latency improvement. A production incident still requires the original upstream stop reason and the deployed proxy revision before attributing causality.

## Executor and consumer follow-up (2026-11-03)

The focused executor regression now observes the final HTTP request with a synthetic upstream. The 24 effort cases cover max/xhigh/high/medium, streaming/non-streaming, and preserve/override/filter policies. The default Opus 5.5 model preserves four distinct efforts; final payload overrides win and filtered effort fields are not restored. Two additional cases verify that a sensitive stop remains incomplete/content_filter through the full executor, preserves partial text and usage, and emits exactly one stream terminal event.

Validation: `go test ./internal/runtime/executor -run 'TestClaudeResponses(FinalWireEffort|SensitiveStopThroughExecutor)$' -count=1` passed, and `go build -o /tmp/cdemo-cpa-final-wire-check ./cmd/server` passed. These are local synthetic tests for the base model, not observations of the deployed baidu alias, live configuration, or provider acceptance.

An offline consumer replay using the published Pi AI 1.3.0 Responses parser and the existing Clawd event translator/completion projector confirmed the consequence: completed partial JSON becomes Pi stop / gateway success, whereas incomplete/content_filter becomes Pi error / gateway failure. Both preserve the partial text. The valid-completion control passes; a max-output control maps to Pi length and still needs the business layer's strict JSON validation. No retry loop, live provider call, real incident recovery, or latency gain is established by this replay. Its fixtures and package lock remain in the private experiment directory; no credentials or real resumes were used.

## Full local process check (2026-11-03)

A separate loopback-only harness ran the actual Pi 1.3.0 CLI, CLIProxyAPI provider 1.4.15, this server build, and a synthetic Anthropic upstream. Both imported Persona instruction files were physically loaded, without appending them to an older cloud image. A local alias mapped `claude-opus-5-5-baidu` to `claude-opus-5-5`; no real provider credentials, business tools, or external inference were used.

All nine cases passed: four efforts for each of two instruction files, plus a partial JSON response ending in sensitive. Each effort reached the final HTTP request unchanged with adaptive thinking and max_tokens=128000. The sensitive case ended with Pi error / incomplete.content_filter, one upstream request, and zero automatic retries in the full agent loop. This extends the consumer replay evidence, but the alias configuration and upstream were synthetic. It does not establish the deployed alias behavior, actual medical stop reason, live retry behavior, or quality/latency improvement. The harness, package lock, and metadata-only result remain in the private experiment directory.

## Delivery revalidation (2026-11-05)

The remote fork's `main` was refreshed and remains at baseline `1eee59dc7144b95dfd5b8ecc81ee55c40882bf06`. With Go 1.26.10, both affected translator packages passed again, as did the 24 final-wire effort cases, both executor sensitive-stop cases, and a fresh `./cmd/server` build. Changed Go files are gofmt-clean. No external provider request or deployment was made during this check.

The fork currently inherits active workflows that retarget `main` pull requests to `dev` and reject changes under `internal/translator/`. These repository gates are separate from local test results and the documented WRITE-permission exception. The fix must not be merged by bypassing these checks; maintainers must resolve the intended contribution path before it can reach `main` and a deployed proxy artifact. Until then, the verified patch does not change live stop reasons or establish that the original truncation incident is fixed.
