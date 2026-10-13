#!/usr/bin/env bash
# invisible-chars.sh — no tracked text file may contain a character a reader
# cannot see and a terminal or an editor may act on.
#
# Forbidden:
#   - Unicode bidirectional controls (U+061C, U+200E, U+200F, U+202A..U+202E,
#     U+2066..U+2069): they reorder what is displayed, which is how a change
#     can read as one thing and compile as another;
#   - the line and paragraph separators (U+2028, U+2029);
#   - C1 controls (U+0080..U+009F);
#   - C0 controls other than tab, line feed and carriage return, and DEL.
#
# Why this exists: a file-writing tool turned backslash-u escapes into the
# characters themselves, and three test files and a README carried raw
# separators and a right-to-left override before anyone saw them (issue #244).
# A test that needs such a character builds it by number, for example
# string(rune(0x202e)) in Go.
#
# Usage:
#   invisible-chars.sh            check the files tracked in this repository
#   invisible-chars.sh <dir>      check the files tracked in the repository at <dir>
#
# Binary files are skipped (git decides what is binary). A text file that must
# hold such a character goes in ALLOW below, with the reason.
#
# It needs git and perl and nothing else: the matching is done in perl, on
# decoded text, so it does not depend on the caller's locale or on how git was
# built.
set -euo pipefail

cd -- "${1:-$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)}"

# Paths (git pathspecs) allowed to contain forbidden characters, each with
# why. Empty on purpose: add to it only with a reason a reviewer can check.
ALLOW=()

pathspec=(.)
if [ "${#ALLOW[@]}" -gt 0 ]; then
    for allowed in "${ALLOW[@]}"; do
        pathspec+=(":(exclude)$allowed")
    done
fi

# One line per offending line, "path:line: U+XXXX, U+YYYY"; the line's own
# text is never printed. git lists the tracked files it does not consider
# binary (-I; the empty pattern matches every line); perl decodes each as
# UTF-8 and looks for the characters by code point.
# shellcheck disable=SC2016  # the dollars are perl's
SCAN='
    use strict;
    use warnings;
    my $bad = qr/[\x{061C}\x{200E}\x{200F}\x{202A}-\x{202E}\x{2066}-\x{2069}\x{2028}\x{2029}\x{0080}-\x{009F}\x00-\x08\x0B\x0C\x0E-\x1F\x7F]/;
    binmode(STDOUT, ":encoding(UTF-8)");
    for my $file (@ARGV) {
        open(my $fh, "<:raw", $file) or next;
        local $/ = "\n";
        while (my $line = <$fh>) {
            utf8::decode($line);
            my %seen;
            $seen{sprintf("U+%04X", ord($1))}++ while $line =~ /($bad)/g;
            print "$file:$.: ", join(", ", sort keys %seen), "\n" if %seen;
        }
        close($fh);
    }
'
# git grep exits 1 when it lists nothing (an empty repository, or only binary
# files), which is not a failure here; anything above 1 is.
hits=$({ git grep -I -l -z -e '' -- "${pathspec[@]}" || [ "$?" -eq 1 ]; } | xargs -0 -r perl -e "$SCAN" --)
if [ -z "$hits" ]; then
    echo "invisible-chars: OK"
    exit 0
fi

echo "invisible-chars: forbidden characters in tracked files:" >&2
printf '%s\n' "$hits" | sed 's/^/  /' >&2
echo "invisible-chars: build such a character by number where a test needs one (see the header of $0)" >&2
exit 1
