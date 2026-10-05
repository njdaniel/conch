# Voice spike (issue #87) — throwaway, never merged

- `lkctl/` — Go, standard library only: mints LiveKit tokens, calls the room API.
- `lkspike/` — Rust client on the LiveKit SDK: `tone`, `listen`, `multi`, `talk`.
- `exp-audience.sh` — the audience-enforcement experiments.
- `server.sh`, `talk.sh` — start a dev server; live mic/headphone session.

Build:

```sh
# one-off: a clang new enough for the SDK's libwebrtc (no root needed, ~610 MB)
mkdir -p ~/.cache/conch-spike/llvm && cd ~/.cache/conch-spike/llvm && \
curl -sL https://github.com/llvm/llvm-project/releases/download/llvmorg-22.1.8/LLVM-22.1.8-Linux-X64.tar.xz | \
tar -xJ --strip-components=1 --wildcards '*/bin/clang' '*/bin/clang++' '*/bin/clang-22' '*/bin/lld' '*/bin/ld.lld' '*/bin/llvm-ar' '*/lib/clang/*'

(cd lkctl && go build -o lkctl .)
(. ./env.sh && cd lkspike && cargo build --release)
```

Findings: `docs/reports/2026-10-05-voice-spike.md` (on the report branch / PR for #87).
