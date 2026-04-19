#!/usr/bin/env bash
# check-changelog.sh — local mirror of the GitHub Actions CHANGELOG
# guard (.github/workflows/changelog-check.yml). Runs the same diff
# rule against the current branch vs the base, so developers can
# verify compliance before pushing.
#
# Usage:
#   ./scripts/check-changelog.sh             # default base is origin/main
#   ./scripts/check-changelog.sh main        # custom base
#
# Exit codes:
#   0 — no code change, or CHANGELOG updated
#   1 — code change present without CHANGELOG update
#   2 — usage / git error
#
# The --skip env knob lets a developer ack a pure-refactor PR that
# doesn't warrant a CHANGELOG entry. The CI equivalent is the
# `skip-changelog` label.
#   CHANGELOG_CHECK_SKIP=1 ./scripts/check-changelog.sh

set -euo pipefail

if [[ "${CHANGELOG_CHECK_SKIP:-}" == "1" ]]; then
  echo "CHANGELOG_CHECK_SKIP=1 set; bypassing check."
  exit 0
fi

base="${1:-origin/main}"

if ! git rev-parse --verify "$base" >/dev/null 2>&1; then
  echo "check-changelog: base ref '$base' not found locally. Run 'git fetch origin' first." >&2
  exit 2
fi

changed=$(git diff --name-only "$base"...HEAD)

if [[ -z "$changed" ]]; then
  echo "No changes vs $base. Nothing to check."
  exit 0
fi

code_changed=0
if echo "$changed" | grep -Eq '^(internal/|cmd/).*\.go$'; then
  code_changed=1
fi

changelog_changed=0
if echo "$changed" | grep -Eq '^CHANGELOG\.md$'; then
  changelog_changed=1
fi

if [[ "$code_changed" -eq 1 && "$changelog_changed" -eq 0 ]]; then
  echo "check-changelog: PR touches internal/ or cmd/ code but does not update CHANGELOG.md." >&2
  echo "" >&2
  echo "Add an entry under [Unreleased] describing the user-visible change." >&2
  echo "If the change is genuinely invisible (refactor / tests / docs), run with CHANGELOG_CHECK_SKIP=1." >&2
  exit 1
fi

echo "CHANGELOG check passed."
