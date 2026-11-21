#!/usr/bin/env bash
# Launch an existing Linux unpacked build with a separate, named dev profile.
set -euo pipefail

fail() {
    printf 'desktop-launch-linux-dev: %s\n' "$*" >&2
    exit 1
}

if [[ $# -lt 1 || $# -gt 2 ]]; then
    fail "Usage: $0 PROFILE [UNPACKED_APP_DIR]"
fi
profile=$1
if [[ ! $profile =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$ ]]; then
    fail 'Use a profile name of 1 to 64 ASCII letters, numbers, dashes or underscores, starting with a letter or number.'
fi

script_dir=${BASH_SOURCE[0]%/*}
if [[ $script_dir == "${BASH_SOURCE[0]}" ]]; then
    script_dir=.
fi
IFS= read -r -d '' app < <(realpath -ez -- "${2:-$script_dir/../desktop/dist/linux-unpacked}") ||
    fail 'Build the unpacked app with make desktop-build or pass the path to your unpacked build.'

unsafe() {
    printf 'desktop-launch-linux-dev: %q %s.\n' "$1" "$2" >&2
    fail 'Build in a private directory under your home, or copy an unpacked build you trust there and pass that directory as UNPACKED_APP_DIR.'
}

# Match the Linux ancestor rules in desktop/main/voice/spawn.ts. Walk the
# resolved directories that the app and its helper will actually run from.
check_ancestors() {
    local dir=$1 owner mode
    while :; do
        [[ -d $dir ]] || unsafe "$dir" 'is not a directory'
        read -r owner mode < <(stat -c '%u %a' -- "$dir") || unsafe "$dir" 'cannot be checked'
        (( owner == EUID || owner == 0 )) || unsafe "$dir" 'is not owned by you or root'
        if (( (8#$mode & 022) != 0 && (owner != 0 || (8#$mode & 01000) == 0) )); then
            unsafe "$dir" 'can be changed by other users'
        fi
        [[ $dir != / ]] || break
        dir=${dir%/*}
        dir=${dir:-/}
    done
}

check_executable() {
    local file=$1 owner mode
    [[ -f $file && -x $file && ! -L $file ]] || unsafe "$file" 'is not a regular executable file'
    read -r owner mode < <(stat -c '%u %a' -- "$file") || unsafe "$file" 'cannot be checked'
    (( owner == EUID || owner == 0 )) || unsafe "$file" 'is not owned by you or root'
    (( (8#$mode & 022) == 0 )) || unsafe "$file" 'can be changed by other users'
}

check_ancestors "$app"
check_executable "$app/conch-desktop"
[[ -d $app/resources && ! -L $app/resources ]] || unsafe "$app/resources" 'is not a regular directory'
helper=$app/resources/conch-voice
if [[ -e $helper || -L $helper ]]; then
    check_executable "$helper"
fi

# Electron also loads code and assets from this tree. Check those paths before
# launch, including resources/app.asar, and refuse links out of the build.
bad_entry=$(find "$app" \( ! \( -type d -o -type f \) -o \( ! -uid "$EUID" ! -uid 0 \) -o -perm /022 \) -printf 'unsafe' -quit) ||
    unsafe "$app" 'could not be checked'
[[ -z $bad_entry ]] || unsafe "$app" 'contains a link, a writable path, or a path with a different owner'

config_home=${XDG_CONFIG_HOME:-$HOME/.config}
[[ $config_home == /* ]] || fail 'XDG_CONFIG_HOME must be an absolute path.'
umask 077
exec "$app/conch-desktop" "--user-data-dir=$config_home/conch-desktop-dev/$profile"
