# Claude Relay Risk Research

Date: 2026-05-09

## Scope

This note records public-signal research and repository actions for reducing
Claude account risk in relay/reverse-proxy deployments without changing system
prompt injection, reducing model quality, or restricting downstream CLI clients.

## Public Signals

- Anthropic's Claude Code legal page says Pro/Max advertised limits assume
  ordinary individual use, OAuth is for native Anthropic apps, and third-party
  products should use API-key authentication rather than routing through
  Free/Pro/Max credentials.
  Source: https://code.claude.com/docs/en/legal-and-compliance
- Anthropic's help page says Pro/Max usage is shared across Claude and Claude
  Code, and Claude Code may switch to API credits only through explicit user
  action.
  Source: https://support.claude.com/en/articles/11145838-use-claude-code-with-your-pro-or-max-plan
- Hacker News and follow-up reporting around OpenClaw describe April 2026
  enforcement against third-party harnesses using Claude subscription limits.
  Sources:
  https://news.ycombinator.com/item?id=47633396
  https://www.techradar.com/pro/bad-news-claude-users-anthropic-says-youll-need-to-pay-to-use-openclaw-now
- Linux.do threads repeatedly attribute recent account failures to combinations
  of relay/sub2api usage, Claude Code usage, payment history, IP/proxy quality,
  region signals, and account reuse. The discussion is noisy, but it consistently
  treats "single header spoofing" as insufficient.
  Sources:
  https://linux.do/t/topic/1737548
  https://linux.do/t/topic/1768844
  https://linux.do/t/topic/1811609
  https://linux.do/t/topic/1826755

## Repository Findings

- The project already has substantial non-prompt hygiene: proxy/tracing header
  scrubbing, response header hygiene, utls/DoH transport, identity rewrite,
  request jitter, Claude Code header capture tests, and Anthropic shape
  preservation for native Claude Code requests.
- One concrete non-prompt mismatch was present: native Claude Code
  `metadata.user_id.session_id` could be preserved in the body while the
  gateway generated a fresh `X-Claude-Code-Session-Id` header. That creates an
  avoidable header/body inconsistency.

## Implemented Changes

- Added a gateway-local `UpstreamSessionKey` field to IR. This key is never sent
  upstream directly.
- Added `buildUpstreamSessionKey` in the server pipeline. It derives a stable
  session key from tenant/group/API key, client IP, inbound protocol/model, and
  the first user turn or conversation prefix.
- Changed the Claude provider to choose session IDs in this order:
  1. Preserve `session_id` from native Anthropic metadata, including JSON and
     legacy Claude Code formats.
  2. Generate a deterministic UUIDv4 from account ID plus gateway session key.
  3. Fall back to a deterministic account-scoped UUID, or random UUID only when
     no account exists.
- Added tests covering metadata-preserved session IDs, deterministic generated
  session IDs, and client-IP session-key separation.

## Explicit Non-Changes

- Did not change system prompt injection.
- Did not change billing attribution block injection or signing.
- Did not change model selection, max token defaults, thinking budgets,
  temperature handling, or output quality settings.
- Did not add downstream CLI restrictions.

## Remaining Non-Prompt Improvements

- Keep Claude Code version/header templates updateable from captured fixtures
  rather than hand editing constants.
- Add an observability panel for upstream-visible identity/session consistency:
  account ID, derived session key hash, metadata session source, and header
  session source.
- Add per-account soft concurrency and sustained-usage shaping so a single
  account does not exhibit impossible multi-user burst patterns.
- Add a config audit command that reports proxy-disclosure headers, reverse
  proxy response leaks, mismatched TLS mode, and missing identity persistence.
