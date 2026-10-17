#!/usr/bin/env bash
# report-to-issue.sh — file a tester's problem report as a GitHub issue
# (issue #446, private alpha A13).
#
#   ops/report-to-issue.sh <report-id>
#
# It runs on the operator's own machine, never on the server: conchd never
# reaches GitHub, and the repository is public, so a report becomes an issue
# only after a person has read it. In order:
#
#   1. conch admin reports show <id> --markdown, into a private temporary file
#      (the bug template's sections, channel and person names taken out);
#   2. $EDITOR on that file, for the operator to read and trim;
#   3. a title and a yes, and only then
#      gh issue create --label type/bug --title <title> --body-file <file>;
#   4. conch admin reports file <id> <issue-url>, so the report is marked filed.
#
# Nothing is filed when the editor fails, the file is left empty, it still
# holds something that looks like a Conch credential, ticket or invite code,
# or the answer is not yes. The issue lands in the Project's Backlog by the
# repository's own automation (A2).
#
# Environment: EDITOR (default vi); CONCH_SERVER and CONCH_TOKEN as for conch;
# GH_REPO as for gh. CONCH and GH name the two programs (default conch and
# gh), for a test to stand in for them.
set -euo pipefail

usage() {
    echo "usage: $0 <report-id>" >&2
    exit 2
}

[ "$#" -eq 1 ] || usage
id=$1
case $id in
    '' | *[!0-9]*) usage ;;
esac

conch=${CONCH:-conch}
gh=${GH:-gh}
editor=${EDITOR:-vi}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
chmod 700 "$work"
body=$work/report-$id.md

"$conch" admin reports show "$id" --markdown >"$body"

# EDITOR may carry its own arguments ("code --wait"), so it is split on
# purpose.
# shellcheck disable=SC2086
if ! $editor "$body"; then
    echo "report-to-issue: the editor failed; nothing was filed" >&2
    exit 1
fi
if ! grep -q '[^[:space:]]' "$body"; then
    echo "report-to-issue: the report is empty; nothing was filed" >&2
    exit 1
fi
# A tester may have pasted a credential, a socket ticket or an invite link
# despite the form's warning. The repository is public.
if grep -Eq 'conch_[A-Za-z0-9_-]{8,}' "$body"; then
    echo "report-to-issue: the report still holds something that looks like a Conch credential, ticket or invite code; take it out and run this again. Nothing was filed." >&2
    exit 1
fi

# What may be private but is not certain to be is pointed out, not refused:
# a web address, an email address, an IP address. The operator decides.
warn=
grep -Eq 'https?://' "$body" && warn="$warn a web address,"
grep -Eq '[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}' "$body" && warn="$warn an email address,"
grep -Eq '(^|[^0-9.])([0-9]{1,3}\.){3}[0-9]{1,3}($|[^0-9.])' "$body" && warn="$warn an IP address,"
if [ -n "$warn" ]; then
    echo "report-to-issue: WARNING: the report holds what looks like${warn%,}. The issue will be public; look again before you answer yes (answer no, fix it, and run this again)." >&2
fi

default_title="Bug: report $id"
printf 'Issue title [%s]: ' "$default_title" >&2
read -r title || title=
title=${title:-$default_title}
printf 'File report %s as a public type/bug issue titled "%s"? [y/N] ' "$id" "$title" >&2
read -r answer || answer=
case $answer in
    y | Y | yes | YES) ;;
    *)
        echo "report-to-issue: not filed" >&2
        exit 1
        ;;
esac

out=$("$gh" issue create --label type/bug --title "$title" --body-file "$body")
url=$(printf '%s\n' "$out" | grep -E '^https?://' | tail -n 1 || true)
if [ -z "$url" ]; then
    echo "report-to-issue: gh did not print the new issue's address; mark the report by hand: conch admin reports file $id <issue-url>" >&2
    exit 1
fi
"$conch" admin reports file "$id" "$url"
echo "$url"
