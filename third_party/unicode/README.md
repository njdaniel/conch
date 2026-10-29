# Vendored Unicode emoji data

`emoji-test.txt` is the Unicode Consortium's emoji keyboard/display test data
for one pinned Unicode Emoji version (UTS #51). It is the one source of the
emoji catalog Conch ships (`docs/design/message-reactions.md` §3.1):
`cmd/emoji-gen` generates `pkg/schema/emoji_catalog_gen.go` (the table the
server canonicalizes reaction keys with, and `schema.EmojiVersion`) and
`web/src/emoji/catalog.json` (the browser picker's catalog) from it in one run
of `go generate ./pkg/schema`.

| | |
| --- | --- |
| Version | Unicode Emoji 18.0 (the file's `# Version:` header) |
| Source | <https://www.unicode.org/Public/18.0.0/emoji/emoji-test.txt> |
| SHA-256 | `emoji-test.sha256`, in `sha256sum -c` form |
| Licence | Unicode-3.0 (`LICENSE`, from <https://www.unicode.org/license.txt>); accepted by Nick on 2026-10-29 for issue #953 |

Since Unicode 17.0 the emoji data files are published under the Unicode
version's own directory, `unicode.org/Public/<major>.<minor>.0/emoji/`;
`unicode.org/Public/emoji/<version>/` holds versions up to 16.0 only.

The generator refuses to run when the file's hash differs from the pin, so a
hand edit to the data cannot reach the catalog. Nothing here is edited by
hand; a new version comes in through the update script alone:

```sh
scripts/emoji-update.sh <version> <sha256>
# for example: scripts/emoji-update.sh 19.0 <the published file's sha256>
```

The script downloads the file for that version, refuses it when its SHA-256
is not the one given (and writes nothing), checks its `# Version:` header,
replaces the file and the pin, and runs `go generate ./pkg/schema`. Upgrading
is a compatible change (it widens the set of accepted reaction keys); the
generator fails if two fully-qualified sequences of the new version collide
once their presentation selectors are stripped, since the lookup table would
then be ambiguous.
