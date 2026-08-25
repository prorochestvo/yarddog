#!/usr/bin/env bash
# Text-level review rules that no AST linter can express.
#
# Comments are invisible to go/analysis-based linters, and "a mock type without
# a compile-time interface assertion" is an absence, which ruleguard cannot
# match. Both are cheap and exact as text scans, so they live here rather than
# being forced into the wrong tool.
#
# Usage:
#   scripts/lint-checks.sh              scan the whole tree — the worklist
#   scripts/lint-checks.sh <base-rev>   scan only files changed since <base-rev>
#
# The second form is what makes these checks usable as a merge gate. golangci-lint
# carries --new-from-rev for exactly this reason: the tree has standing findings
# being worked off, and a gate that fails on them fails always, which trains
# everyone to ignore it. These checks need the same treatment or they reintroduce
# the problem that flag exists to solve.
#
# Exit code 1 on any violation, 2 if the check could not run.
set -uo pipefail

root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$root" || exit 1

base="${1:-}"

if [ -n "$base" ] && ! git rev-parse --verify --quiet "$base" >/dev/null; then
	printf 'lint-checks: base revision %s does not resolve\n' "$base" >&2
	exit 2
fi

status=0

report() {
	printf '%s\n' "$1" >&2
	status=1
}

# scan_list prints the files to check, one per line, keeping only those whose path
# matches the extended regex in $1.
#
# Without a base it walks the tree. With one it asks git what moved, filtering to
# ACMR: a path deleted by the change under review cannot be read, and its removal
# is not a violation the change introduced.
#
# Untracked files are unioned in deliberately. `git diff <rev>` cannot see a file
# that has never been added, so a gate built on it alone passes every brand-new
# file until the moment it is committed — silently green exactly when the author
# is writing the code the gate exists to check. CI would catch it later, which is
# the wrong place to find out.
scan_list() {
	local pattern="$1"
	if [ -z "$base" ]; then
		git ls-files -- . 2>/dev/null | grep -E "$pattern"
		return
	fi
	{
		git diff --name-only --diff-filter=ACMR "$base" -- 2>/dev/null
		git ls-files --others --exclude-standard -- . 2>/dev/null
	} | sort -u \
		| grep -E "$pattern" \
		| while IFS= read -r f; do [ -f "$f" ] && printf '%s\n' "$f"; done
}

# grep_files runs an extended-regex grep over a newline-separated file list on
# stdin. It is a function rather than an inline pipe because grep with an empty
# argument list reads stdin and would hang the whole script on a change that
# touched no matching file.
grep_files() {
	local pattern="$1"
	local files
	files=$(cat)
	[ -n "$files" ] || return 1
	printf '%s\n' "$files" | tr '\n' '\0' | xargs -0 grep -nE "$pattern" 2>/dev/null
}

# Section-divider comments. They restate structure the file already has.
if hits=$(scan_list '\.go$' | grep_files '^\s*//\s*[-=*#]{3,}'); then
	report "section-divider comments (structure is not documentation):"
	printf '%s\n' "$hits" >&2
fi

# Plan and task references outside the plans/ tree. The reason belongs in the
# code; the plan number is meaningless to whoever reads the file later.
#
# .claude/agent-memory is exempt: it is written by agents to record where a piece
# of context came from, so a plan reference there is the point rather than a
# leak. The rest of .claude/ is hand-authored and stays under the rule.
if hits=$(scan_list '\.(go|sql|md)$' \
	| grep -vE '^(plans/|logs/|tmp/|build/|backups/|.*/agent-memory/)' \
	| grep_files '(plans/[0-9]{3}-|PLAN-[0-9]+|task #[0-9]+)'); then
	report "plan or task references outside plans/ (write the reason, not the ticket):"
	printf '%s\n' "$hits" >&2
fi

# A settings failure must not log the parser error.
#
# dsninjector.Parse embeds its whole input in the error it returns, and
# BEACON_TELEGRAMBOT_DSN is the bot token while the AI DSNs carry API keys.
# internal.NewLogger points the standard logger at the persisted log file, so a
# single %v here writes credentials to disk and they stay there. Name the
# variable that failed and drop the error text.
#
# This predates golangci-lint adoption and survives it: the defect is a specific
# string in a specific argument position, not a shape an AST linter can match.
if hits=$(scan_list '^(cmd|internal)/.*\.go$' \
	| grep -v '_test\.go$' \
	| grep_files '(log\.Fatalf?|infraFail)\("settings[^"]*",[^)]*\berr'); then
	report "a settings failure logs the parser error (this writes credentials to the log file):"
	printf '%s\n' "$hits" >&2
fi

# Mock and stub types in tests must carry a compile-time interface assertion,
# so an interface change breaks the build instead of the test's meaning.
while IFS= read -r file; do
	[ -n "$file" ] || continue
	types=$(grep -oE '^type (mock|stub|fake)[A-Za-z0-9_]* ' "$file" 2>/dev/null | awk '{print $2}')
	[ -n "$types" ] || continue
	while IFS= read -r t; do
		[ -n "$t" ] || continue
		if ! grep -qE "^var _ .* = \(?\*?&?$t\b" "$file"; then
			report "$file: $t has no compile-time interface assertion (var _ Iface = (*$t)(nil))"
		fi
	done <<<"$types"
done < <(scan_list '_test\.go$')

exit $status
