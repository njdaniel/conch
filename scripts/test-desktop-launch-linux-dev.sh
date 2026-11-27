#!/usr/bin/env bash
# Exercises the Linux dev launcher without Electron or any devices.
set -euo pipefail
umask 077

root=$(cd -- "$(dirname -- "$0")/.." && pwd -P)
launcher=$root/scripts/desktop-launch-linux-dev.sh
# Use the system temp directory. A caller's TMPDIR may be group-writable.
tmp=$(mktemp -d /tmp/conch-desktop-launch-test.XXXXXXXX)
trap 'rm -rf "$tmp"' EXIT

fail() {
    printf 'desktop-launch-linux-dev-test: FAIL: %s\n' "$*" >&2
    exit 1
}

export XDG_CONFIG_HOME="$tmp/dev config"
export CONCH_DEV_LAUNCH_TEST_RECEIPT="$tmp/launch-args"
app="$tmp/private/build output"
helper="$app/resources/conch-voice"
mkdir -p "$app/resources"
touch "$app/resources/app.asar"
cat >"$app/conch-desktop" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" >"$CONCH_DEV_LAUNCH_TEST_RECEIPT"
EOF
printf '#!/bin/sh\nexit 0\n' >"$helper"
chmod 755 "$app/conch-desktop" "$helper"

expect_refusal() {
    local message=$1 output
    shift
    rm -f "$CONCH_DEV_LAUNCH_TEST_RECEIPT"
    if output=$("$launcher" "$@" 2>&1); then
        fail "accepted an invalid launch: $*"
    fi
    [[ $output == *"$message"* ]] || fail "unexpected refusal: $output"
    [[ ! -e $CONCH_DEV_LAUNCH_TEST_RECEIPT ]] || fail 'ran the app before refusing'
}

expect_refusal 'Use a profile name' ../default "$app"
chmod g+w "$tmp/private"
expect_refusal 'can be changed by other users' demo "$app"
expect_refusal 'copy an unpacked build you trust' demo "$app"
chmod g-w "$tmp/private"

"$launcher" demo "$app"
[[ $(cat "$CONCH_DEV_LAUNCH_TEST_RECEIPT") == "--user-data-dir=$XDG_CONFIG_HOME/conch-desktop-dev/demo" ]] ||
    fail 'did not pass the isolated profile as one argument'

chmod g+w "$app/resources/app.asar"
expect_refusal 'contains a link, a writable path' demo "$app"
chmod g-w "$app/resources/app.asar"

chmod g+w "$helper"
expect_refusal 'can be changed by other users' demo "$app"
mv "$helper" "$tmp/real-helper"
ln -s "$tmp/real-helper" "$helper"
expect_refusal 'is not a regular executable file' demo "$app"
rm "$helper"
"$launcher" demo "$app"
[[ -f $CONCH_DEV_LAUNCH_TEST_RECEIPT ]] || fail 'refused a text-only development build'

rm "$CONCH_DEV_LAUNCH_TEST_RECEIPT"
newline_app="$tmp/"$'build with a newline\n'
mv -- "$app" "$newline_app"
"$launcher" demo "$newline_app"
[[ -f $CONCH_DEV_LAUNCH_TEST_RECEIPT ]] || fail 'refused a private build whose path ends in a newline'

echo 'desktop-launch-linux-dev-test: OK'
