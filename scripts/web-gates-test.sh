#!/usr/bin/env bash
# Tests of the two gates that guard what goes into the web client. A gate that
# cannot fail is not a gate, so each is shown failing on what it exists to stop.
# Run by `make web-check`. Needs no network and no Node.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail() {
    echo "web-gates-test: FAIL: $*"
    exit 1
}

# A small client: one runtime and one development dependency, both listed.
mkdir "$tmp/web"
echo 24.18.0 >"$tmp/web/.nvmrc"
printf '# test list\nreact\nvite\n' >"$tmp/web/deps-allowlist.txt"
cat >"$tmp/web/package.json" <<'JSON'
{
  "name": "test",
  "private": true,
  "engines": { "node": "24.18.0" },
  "dependencies": { "react": "19.3.0" },
  "devDependencies": { "vite": "8.3.4" }
}
JSON
cat >"$tmp/web/package-lock.json" <<'JSON'
{
  "lockfileVersion": 3,
  "packages": {
    "": { "name": "test" },
    "node_modules/react": {
      "version": "19.3.0",
      "resolved": "https://registry.npmjs.org/react/-/react-19.3.0.tgz",
      "integrity": "sha512-AAAA",
      "license": "MIT"
    },
    "node_modules/vite": {
      "version": "8.3.4",
      "resolved": "https://registry.npmjs.org/vite/-/vite-8.3.4.tgz",
      "integrity": "sha512-BBBB",
      "dev": true,
      "license": "(MIT OR GPL-3.0)"
    },
    "node_modules/vite/node_modules/dual": {
      "version": "1.0.0",
      "resolved": "https://registry.npmjs.org/dual/-/dual-1.0.0.tgz",
      "integrity": "sha512-CCCC",
      "dev": true,
      "license": "MIT AND ISC"
    }
  }
}
JSON
printf '# test list\nMIT\nISC\n' >"$tmp/licences.txt"
cp -r "$tmp/web" "$tmp/clean"

depgate() { ./scripts/web-depgate.sh "$tmp/web" 2>&1; }
licences() { ./scripts/web-licences.sh "$tmp/web/package-lock.json" "$tmp/licences.txt" 2>&1; }
restore() { rm -rf "$tmp/web" && cp -r "$tmp/clean" "$tmp/web"; }
edit() { # edit <file> <jq filter>
    jq "$2" "$1" >"$1.new" && mv "$1.new" "$1"
}

depgate >/dev/null || fail "the dependency gate refused a client whose dependencies are listed and exact: $(depgate)"
licences >/dev/null || fail "the licence gate refused a lockfile whose licences are allowed: $(licences)"

# 1. The dependency gate refuses a dependency that is not on the list, of any
#    kind, and names it.
edit "$tmp/web/package.json" '.devDependencies["left-pad"] = "1.3.0" | .optionalDependencies = {"also-not-listed": "1.0.0"}'
if out=$(depgate); then
    fail "the dependency gate passed a manifest naming unlisted packages"
fi
grep -q 'left-pad is not in' <<<"$out" || fail "the dependency gate did not name the unlisted package: $out"
grep -q 'also-not-listed is not in' <<<"$out" || fail "the dependency gate ignored an optional dependency: $out"
grep -q 'react is not in' <<<"$out" && fail "the dependency gate refused a listed package: $out"
restore

# 2. ... and every version that is not one exact version.
for range in '^19.3.0' '~19.3.0' '>=19' '19.x' '*' 'latest' '19' 'file:../react' 'github:facebook/react' 'https://example.com/react.tgz' '19.3.0 || 20.0.0'; do
    edit "$tmp/web/package.json" ".dependencies.react = \"$range\""
    if out=$(depgate); then
        fail "the dependency gate passed the version \"$range\""
    fi
    grep -qF "react is \"$range\"" <<<"$out" || fail "the dependency gate failed \"$range\" for another reason: $out"
done
edit "$tmp/web/package.json" '.dependencies.react = "19.3.0-rc.1"'
depgate >/dev/null || fail "the dependency gate refused an exact pre-release version: $(depgate)"
restore

# 3. ... and "overrides", which swaps a package for another copy.
edit "$tmp/web/package.json" '.overrides = {"react": "npm:not-react@1.0.0"}'
if out=$(depgate); then
    fail "the dependency gate passed a manifest with overrides"
fi
grep -q 'has "overrides"' <<<"$out" || fail "the dependency gate failed overrides for another reason: $out"
restore

# 4. ... and a Node pin in engines that is not the one in .nvmrc.
edit "$tmp/web/package.json" '.engines.node = ">=24"'
if out=$(depgate); then
    fail "the dependency gate passed engines that do not match .nvmrc"
fi
grep -q 'engines.node' <<<"$out" || fail "the dependency gate failed engines for another reason: $out"
restore

# 5. ... and a package from anywhere but the registry, or with no hash.
edit "$tmp/web/package-lock.json" '.packages["node_modules/react"].resolved = "https://example.com/react-19.3.0.tgz"'
if out=$(depgate); then
    fail "the dependency gate passed a package from another host"
fi
grep -q 'node_modules/react in .* comes from "https://example.com' <<<"$out" || fail "the dependency gate failed another host for another reason: $out"
restore
edit "$tmp/web/package-lock.json" 'del(.packages["node_modules/vite"].integrity)'
if out=$(depgate); then
    fail "the dependency gate passed a package with no integrity hash"
fi
grep -q 'node_modules/vite in .* has no integrity hash' <<<"$out" || fail "the dependency gate failed a missing hash for another reason: $out"
restore

# 6. The licence gate refuses a licence that is not on the list, naming the
#    licence and the package, transitive ones included.
edit "$tmp/web/package-lock.json" '.packages["node_modules/vite/node_modules/dual"].license = "GPL-3.0-only"'
if out=$(licences); then
    fail "the licence gate passed a GPL-3.0-only package"
fi
grep -q '"GPL-3.0-only" is not in .*; it is the licence of dual@1.0.0' <<<"$out" || fail "the licence gate did not name the licence and the package: $out"
restore

# 7. ... and a package with no licence field.
edit "$tmp/web/package-lock.json" 'del(.packages["node_modules/react"].license)'
if out=$(licences); then
    fail "the licence gate passed a package with no licence"
fi
grep -q 'react@19.3.0 has no license field' <<<"$out" || fail "the licence gate failed a missing licence for another reason: $out"
restore

# 8. ... and an AND with one side not allowed, or an expression it cannot read.
for expr in 'MIT AND GPL-3.0' '(MIT OR ISC) AND GPL-3.0' '(MIT AND GPL-3.0)'; do
    edit "$tmp/web/package-lock.json" ".packages[\"node_modules/react\"].license = \"$expr\""
    if out=$(licences); then
        fail "the licence gate passed \"$expr\""
    fi
    grep -qF "\"$expr\" is not in" <<<"$out" || fail "the licence gate failed \"$expr\" for another reason: $out"
done
restore

echo "web-gates-test: OK"
