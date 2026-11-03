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
