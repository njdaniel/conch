//! The client of `conchd` that `conch-voice` uses, and nothing else: no audio and no LiveKit.
//!
//! Input: a server address, the login `conch login` stored (or `CONCH_TOKEN`), and a channel.
//! Output: voice sessions, presence documents, and the answers to transmit reports, as Rust
//! copies of the wire types in `pkg/schema`. `pkg/schema` is the single source of truth; the
//! types here are checked against its golden fixtures.
//! Owns: nothing that outlives a call. It never writes the credentials file and never retries.
//!
//! Design: `docs/design/conch-voice.md` §2, §5 to §8. Empty until issue #180.

// CLAUDE.md: no unwrap/expect outside tests and main.
#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

/// The crate's version, as built.
pub const VERSION: &str = env!("CARGO_PKG_VERSION");

#[cfg(test)]
mod tests {
    #[test]
    fn version_is_the_workspace_version() {
        assert_eq!(super::VERSION, env!("CARGO_PKG_VERSION"));
        assert!(!super::VERSION.is_empty());
    }
}
