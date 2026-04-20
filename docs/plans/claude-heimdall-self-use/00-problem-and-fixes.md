# Claude's self-use of heimdall — problem, analysis, fixes

**Status:** ready for next session, context-fresh.
**Source:** observed across two independent Claude Code sessions (2026-04-19), both admitted under-using heimdall proactively despite CLAUDE.md and MCP server instructions telling them to.

---

## The symptom

Both Claude sessions, when asked "have you been using heimdall?", admitted:

- ❌ No `heimdall_recall` at session start
- ❌ No direct `heimdall_search` before Read/Grep/Bash exploration
- ❌ No `heimdall_index_text` on the handoff docs / review files they read
- ❌ No `heimdall_remember` on the non-obvious decisions they made (Mac localhost→::1 binding, `-X theirs` for binary-index conflicts, PR-per-problem being the wrong design, v0.0.3 auto-register regression root cause, etc.)
- ✅ Only heimdall_status + heimdall_projects used — and only when the user asked directly
- ✅ Sub-agent prompts instructed to use heimdall, but coordinator didn't use it personally

Net: Claude leans on the UserPromptSubmit hook's auto-injected context to feel informed, and delegates active heimdall use to sub-agents. Coordinator-tier heimdall use stays near zero.

---

## Why it happens

1. **Hook auto-injection creates a false sense of coverage.** The `UserPromptSubmit` hook dumps 5 relevant hits at the bottom of every user turn. That signal "I have context" masks "I didn't actually query." Claude feels pre-informed and skips the active search.

2. **"ALWAYS use X" is abstract — needs trigger→action pairs.** The MCP instruction block says use `heimdall_search` first, but without concrete "when X happens, call Y" rules Claude drifts. Abstract rules don't fire; paired triggers do.

3. **Delegation dilutes the habit.** When Claude puts "use heimdall" in an agent prompt, the coordinator ticks a mental box without using heimdall itself. The agent's findings don't surface back to the coordinator, so nothing reinforces the habit.

4. **"Unavailable (index model mismatch)" kills all write paths.** One stale-index error makes Claude treat the whole MCP as broken. But `heimdall_remember` and `heimdall_index_text` don't need search to work. The error scope is misleading.

5. **Session start/end is one-sided.** SessionStart hook auto-injects recall, which feels like the recall is done. Session end has no hook to prompt `heimdall_remember` on what was learned — so non-obvious decisions evaporate at session boundary.

6. **No visible cost of skipping.** Nothing penalizes drift. If sessions began with "last session you missed 7 remember-worthy events" Claude would adjust.

7. **"Remember-worthy" signal is fuzzy.** "Important decision" is subjective. A concrete heuristic: after any user frustration/correction, after any non-obvious workaround discovered, after any user-taught fact — remember.

8. **Index_text rule is vague.** "Substantial doc" has no threshold. Concrete rule needed: WebFetch > 500 chars / MCP external-content read (Jira, Slack, GitHub issue bodies) / handoff docs → index silently.

---

## Concrete fixes — prioritized

### P0 — Highest leverage

1. **SessionEnd review hook.** Fire a hook at session stop that shows the model a list of candidate "decisions / corrections / workarounds from this session" (derived from transcript or from user-correction events already logged) and forces at least one `heimdall_remember` call on non-obvious items. This directly closes the biggest leak — cross-session decay.

2. **Rewrite MCP server instructions as trigger→action pairs, not abstract rules.** Every rule names a concrete tool being used and what to call when:
   - "Before Read of a file > 200 lines → try `heimdall_search` first."
   - "After any user correction / 'actually', 'no, do X instead' → `heimdall_remember` with type=feedback."
   - "After any WebFetch or external MCP read > 500 chars → `heimdall_index_text` silently (no user mention)."
   - "At start of any task involving architecture / decisions / plans → `heimdall_recall` for relevant prior context."

3. **Scope the "unavailable" error message.** Change from "heimdall: unavailable (index model mismatch)" to "heimdall_search: unavailable (index mismatch). heimdall_remember / heimdall_index_text still work." So Claude keeps using writes even when reads are down.

### P1 — Medium leverage

4. **UserPromptSubmit hook nag-after-N-turns.** If Claude hasn't called any `heimdall_*` tool directly (not just received auto-injected context) in N turns, append a short "You haven't called heimdall in N turns — was that intentional?" nudge. Threshold N=10 reasonable.

5. **Promote `heimdall_remember` to the same tier as `TaskCreate` in CLAUDE.md.** Right now TaskCreate is everywhere and Claude reflexively follows it. heimdall_remember is sidelined. Add explicit trigger rules for when to remember, matching the TaskCreate pattern.

6. **Remove the generic "use ALL MCP tools proactively" one-liner from CLAUDE.md / system prompt.** Replace with per-tool trigger hooks. Generic rules are noise and don't fire.

### P2 — Nice to have

7. **Auto-index of read external content** — if the Read/WebFetch tool result contains identifiable external-source markers (Jira URL, Slack permalink, GitHub issue URL), the runtime could fire `heimdall_index_text` automatically, sidestepping the "did Claude remember to index?" question entirely.

8. **Transcript post-processor that flags skipped remember moments.** Scan the transcript for user frustration markers ("fuck this", "why", "stop"), surprise markers ("wait", "actually", "oh"), or correction markers ("no, do X"); log them as "candidate remember moments." Surface a count at session end.

---

## Out of scope (but worth noting)

- The auto-inject hook is actually GOOD — the bug is that it's TOO good and removes pressure to search. Removing it would regress other sessions. Fix: keep the hook, add the nag for direct-use (#4).
- Sub-agent heimdall use is already enforced via agent prompts. Focus is on coordinator-tier use.
- The v0.0.3 auto-register regression (PR #78) is unrelated — already fixed.

---

## Next-session kickoff prompt

Paste this into a fresh Claude Code session in this repo:

> Read `docs/plans/claude-heimdall-self-use/00-problem-and-fixes.md` end-to-end. It describes a drift pattern in Claude's self-use of heimdall and a prioritized list of concrete fixes. Propose an implementation plan for the P0 items (SessionEnd review hook, rewriting MCP server instructions as trigger→action pairs, scoping the "unavailable" error message). Use `superpowers:brainstorming` first to explore alternative framings, then `superpowers:writing-plans` to produce the plan. Do NOT start implementation until I approve the plan.
>
> Before brainstorming, call `heimdall_recall` for context on prior heimdall instruction/hook work, and `heimdall_search` for the existing MCP instruction block definition in this codebase.
>
> Skip the 3-reviewer / 95-score gate for the *plan doc* (it's meta about usage, not production code). When we get to implementation PRs, apply the normal quality gate.

---

## Success criteria (for the eventual implementation)

A Claude Code session in this repo, asked "have you been using heimdall?" one hour in, should be able to list at least 3 direct (non-sub-agent, non-hook-injected) heimdall calls with concrete reasons — and should have at least one `heimdall_remember` or `heimdall_index_text` write from that session.
