# --net=ops "Principal review" — $(touch pwned-title) `touch pwned-title-tick`

**Date:** 2026-10-13 · **Author:** a fixture, written to be hostile to a shell script
**Headline:** nothing in this file may be run, expanded or read as an option.

## Merged work (3 PRs)

- #101 one
- #102 two
  - nested under #102, so not an item of its own
- #103 three

## Invariant status

| Rule | Status |
|---|---|
| 1. Single-binary | OK |
| 2. Schema-first | OK; `$(touch pwned-table)` |

```sh
## Decisions needed from Nick
- this heading and this item are inside a fence, so they are neither
```

## Backlog delta

Nothing this week.

## Decisions needed from Nick

- --server http://127.0.0.1:1 is text, not an option
-n
-e \n \t \\ \0 %s %d %n
--
1. $(touch pwned-dollar-paren) and ${HOME} and $HOME and $((6*7)) and $'\x41'
2. `touch pwned-backtick` and 'single' and "double" and \"escaped\"
3. ; touch pwned-semicolon && touch pwned-and || touch pwned-or | cat > pwned-redirect < /dev/null &
4. * ? [a-z] {a,b} ~ ! # <tag attr="v"> & </tag> é — → 日本語
5. 'an unterminated " quote, a ` tick and a $( paren
