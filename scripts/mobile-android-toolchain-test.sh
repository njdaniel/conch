#!/usr/bin/env bash
# Exercise the Android installer with a stub launcher and a local JDK (11+).
set -euo pipefail

toolchain="$(cd "$(dirname "${BASH_SOURCE[0]}")/../spikes/mobile/android" && pwd)/toolchain.sh"
probe_root=$(mktemp -d)
trap 'rm -rf "$probe_root"' EXIT

fail() { echo "mobile-android-toolchain-test: FAIL: $*" >&2; exit 1; }

java_bin=$(command -v java) || fail 'a local JDK (11+) is required'
fixture_cache="$probe_root/cache with spaces"
jdk_version=$(sed -n 's/^JDK_VERSION="\(.*\)"$/\1/p' "$toolchain")
clt_version=$(sed -n 's/^CLT_VERSION="\(.*\)"$/\1/p' "$toolchain")
platform_version=$(sed -n 's/^PLATFORM_TOOLS_VERSION="\(.*\)"$/\1/p' "$toolchain")
launcher_dir="$fixture_cache/android-sdk/cmdline-tools/$clt_version"
jdk_bin="$fixture_cache/jdk-$jdk_version/bin"
platform_dir="$fixture_cache/android-sdk/platform-tools"
mkdir -p "$launcher_dir/bin" "$jdk_bin" "$platform_dir" "$probe_root"/{bin,home,ambient-tmp}
ln -s "$java_bin" "$jdk_bin/java"
ln -s /bin/false "$probe_root/bin/curl"
ln -s /bin/true "$probe_root/bin/sleep"
printf 'Pkg.Revision=%s\n' "$clt_version" >"$launcher_dir/source.properties"
printf 'Pkg.Revision=%s\n' "$platform_version" >"$platform_dir/source.properties"
touch "$platform_dir/adb"

cat >"$probe_root/RepoIndexTemp.java" <<'JAVA'
import java.nio.file.Files;
import java.nio.file.Path;

class RepoIndexTemp {
    public static void main(String[] args) throws Exception {
        String scratch = System.getProperty("java.io.tmpdir");
        if (!scratch.equals(args[0])) {
            throw new AssertionError("java.io.tmpdir is " + scratch + ", expected " + args[0]);
        }
        Path created = Files.createTempFile("repo-index-", ".xml");
        if (!created.getParent().equals(Path.of(scratch))) {
            throw new AssertionError("Java created a repo index outside scratch: " + created);
        }
    }
}
JAVA

cat >"$launcher_dir/bin/android" <<'BASH'
#!/usr/bin/env bash
set -euo pipefail
test "$1" = --no-metrics
test "$2" = "--sdk=$CONCH_CACHE/android-sdk"
test "$ANDROID_USER_HOME" = "$CONCH_CACHE/android-user-home"
case "$TMPDIR" in
    "$CONCH_CACHE"/dl.*) ;;
    *) echo "unexpected launcher TMPDIR: $TMPDIR" >&2; exit 1 ;;
esac
java "$TOOLCHAIN_PROBE_ROOT/RepoIndexTemp.java" "$TMPDIR"
if [[ -v _JAVA_OPTIONS || -v JDK_JAVA_OPTIONS ]]; then
    echo 'launcher inherited _JAVA_OPTIONS or JDK_JAVA_OPTIONS' >&2
    exit 1
fi
if [ "$3" = --version ]; then
    printf 'First-run notice\n1.2.3\nAndroid CLI update available: 1.2.4\n'
fi
BASH
chmod +x "$launcher_dir/bin/android" "$platform_dir/adb"

export TOOLCHAIN_PROBE_ROOT="$probe_root" PATH="$probe_root/bin:$PATH"
export HOME="$probe_root/home" TMPDIR="$probe_root/ambient-tmp"
export CONCH_CACHE="$fixture_cache" CONCH_ACCEPT_ANDROID_LICENCES=1 CONCH_ANDROID_NO_EMULATOR=1
export JAVA_TOOL_OPTIONS="-Djava.io.tmpdir=\"$TMPDIR\""
export _JAVA_OPTIONS="$JAVA_TOOL_OPTIONS" JDK_JAVA_OPTIONS="$JAVA_TOOL_OPTIONS"

version=$("$toolchain" --cli-version) || fail 'CLI version probe failed'
[ "$version" = 1.2.3 ] || fail "expected CLI version 1.2.3, got $version"
"$toolchain" || fail 'SDK install probe failed'
[ -s "$fixture_cache/android-env.sh" ] || fail 'installer did not write android-env.sh'
[ -z "$(find "$probe_root" -name 'repo-index-*.xml' -print -quit)" ] || fail 'a repo index survived cleanup'

quoted_cache="$probe_root/cache \"QA\""
if output=$(CONCH_CACHE="$quoted_cache" "$toolchain" --cli-version 2>&1); then
    fail 'installer accepted a cache path with a double quote'
fi
[[ $output == *'CONCH_CACHE must not contain a double quote'* ]] || fail "unexpected quote diagnostic: $output"
[ ! -e "$quoted_cache" ] || fail 'installer created the rejected cache directory'

echo 'mobile-android-toolchain-test: ok'
