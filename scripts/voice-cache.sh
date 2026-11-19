# shellcheck shell=bash
# Shared checks for scripts/voice-toolchain.sh and scripts/voice-env.sh.
# The cached downloads are the reference for the installed files. Verify
# their pinned digests before opening them, then verify the installed bytes
# against the corresponding members. A digest kept beside an installed file
# would let a replaced file pass if its digest were replaced as well.

voice_archive_matches() {
    python3 -I - "$@" <<'PY'
import hashlib
from pathlib import Path
import sys
import tarfile
import zipfile


def digest(stream):
    sha256 = hashlib.sha256()
    for chunk in iter(lambda: stream.read(1024 * 1024), b""):
        sha256.update(chunk)
    return sha256.hexdigest()


archive, pin, installed = sys.argv[1:]
try:
    with open(archive, "rb") as source:
        if digest(source) != pin:
            sys.exit(1)
        source.seek(0)
        if archive.endswith(".tar.gz"):
            with tarfile.open(fileobj=source, mode="r:gz") as tar:
                members = [m for m in tar if m.isfile() and m.name.rsplit("/", 1)[-1] == "cargo-deny"]
                if len(members) != 1:
                    sys.exit(1)
                with tar.extractfile(members[0]) as member, open(installed, "rb") as executable:
                    sys.exit(digest(member) != digest(executable))
        tree = Path(installed)
        with zipfile.ZipFile(source) as zip_archive:
            for name in ("webrtc.ninja", "lib/libwebrtc.a"):
                with zip_archive.open(f"{tree.name}/{name}") as member, (tree / name).open("rb") as file:
                    if digest(member) != digest(file):
                        sys.exit(1)
except (OSError, EOFError, KeyError, tarfile.TarError, zipfile.BadZipFile):
    sys.exit(1)
PY
}

voice_deny_matches() {
    [ -x "$1/cargo-deny" ] &&
        voice_archive_matches "$1/release.tar.gz" "$2" "$1/cargo-deny"
}

voice_mac_webrtc_matches() {
    voice_archive_matches "$1/release.zip" "$2" "$1/mac-arm64-release"
}
