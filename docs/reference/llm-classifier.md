# LLM classifier fallback

Opt-in, local-only second-pass classifier consulted by the `PreToolUse`
guardrail when the static rule table returns `ClassUnknown`.

**Status:** Stage 0 code landed; default off.

## Quick reference

```sh
heimdall-mcp configure --llm-classifier-model=llama3.2:3b
ollama pull llama3.2:3b
export HEIMDALL_LLM_CLASSIFIER=1        # required to enable
export HEIMDALL_GUARDRAILS=shadow       # stage-1 soak; warn/block later
```

- **Recommended model:** `llama3.2:3b` (primary).
- **Fallback model:** `qwen2.5-coder:3b` if the primary underperforms on
  shell-token-heavy commands.
- **Default:** off. With `HEIMDALL_LLM_CLASSIFIER` unset or `0` the hook
  path is identical to today's static-only behavior.
- **Not a security boundary.** See design doc §1.2 — paper-cut prevention
  only. An adversary can bypass the classifier by renaming the binary,
  piping through `bash -c`, or exploiting prompt injection.

## Full design

See [`docs/plans/hooks/11a-design-decisions.md`](../plans/hooks/11a-design-decisions.md)
for the decisions punch list and trade-offs, and
[`docs/plans/hooks/11-llm-classification-fallback.md`](../plans/hooks/11-llm-classification-fallback.md)
for the parent plan (gating rule, latency budget, failure modes, rollout).
