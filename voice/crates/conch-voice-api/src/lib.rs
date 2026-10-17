//! The client of `conchd` that `conch-voice` uses, and nothing else: no audio and no LiveKit.
//!
//! Input: a server address, the login `conch login` stored (or the value of `CONCH_TOKEN`),
//! and a channel; and the lines of a managed helper's control protocol.
//! Output: who the login is, voice sessions, presence snapshots and the presence stream,
//! and the answers to transmit reports, as Rust copies of the wire types in `pkg/schema`;
//! or one [`Error`] that says which refusal or failure it was. The managed helper's
//! commands, read, and its events, written (`conch.voice_control.v1`). `pkg/schema` is the
//! single source of truth; the types here are checked against its golden fixtures.
//! Owns: nothing that outlives a call, other than the connections a [`Client`] keeps open
//! and the socket a [`PresenceStream`] reads. It never writes the credentials file, never
//! reads the environment, never retries and never logs.
//!
//! Secrets never reach output. The login token, every join token and every room name are
//! held in a [`Secret`], whose `Debug` is a placeholder and which has no `Display`; no
//! [`Error`] carries one, a request header, or the body of a session. That holds against a
//! peer that sends the request back, too: a document that fails to decode is described by
//! a position and never by its contents, and the client's own token is taken out of every
//! string an error or an end-of-stream reason keeps from an answer. A public type added
//! to this crate gets a line in `tests/secrets.rs`.
//!
//! The token goes only to the server the user named: in the `Authorization` header, never
//! after a redirect, never through a proxy from the environment, never over plain HTTP to
//! an `https` address, and never to a host other than the one its login is stored under.
//!
//! - [`ServerAddress`]: the address, and the key a login is stored under, as Go makes it.
//! - [`resolve_token`], [`default_config_dir`]: the stored login; [`stored_servers`]: the
//!   servers one is stored for.
//! - [`Client`]: [`Client::whoami`], [`Client::session`], [`Client::transmit`],
//!   [`Client::presence`], [`Client::presence_stream`].
//! - [`parse_command`], [`parse_event`], [`to_line`]: the managed helper's control
//!   protocol (`docs/design/tui-voice.md`).
//! - [`PresenceStream`]: snapshots in order, then a [`StreamEnd`].
//!
//! Design: `docs/design/conch-voice.md` §2, §5 to §8; the server it talks to is
//! `docs/design/voice-control-plane.md` §4, §6 and §8.

// CLAUDE.md: no unwrap/expect outside tests and main.
#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

mod client;
mod control;
mod error;
mod login;
mod presence;
mod server;
mod types;

pub use client::Client;
pub use control::{
    VOICE_CONTROL_ACK_TIMEOUT_MS, VOICE_CONTROL_CAPTURE_STOP_MS, VOICE_CONTROL_EVENT_FD,
    VOICE_CONTROL_EVENT_QUEUE_LINES, VOICE_CONTROL_GATE_SHUT_MS, VOICE_CONTROL_HELLO_TIMEOUT_MS,
    VOICE_CONTROL_LEAVE_EXIT_MS, VOICE_CONTROL_MAX_CHANNEL_BYTES,
    VOICE_CONTROL_MAX_HELPER_VERSION_BYTES, VOICE_CONTROL_MAX_ID, VOICE_CONTROL_MAX_IN_FLIGHT,
    VOICE_CONTROL_MAX_LINE_BYTES, VOICE_CONTROL_MAX_RETRY_IN_MS, VOICE_CONTROL_MAX_SERVER_BYTES,
    VOICE_CONTROL_MAX_VERSIONS, VOICE_CONTROL_OWNER_LOSS_EXIT_MS, VOICE_CONTROL_OWNER_POLL_MS,
    VOICE_CONTROL_SCHEMA_V1, VOICE_CONTROL_START_TIMEOUT_MS, VOICE_CONTROL_STOP_KILL_AFTER_MS,
    VOICE_CONTROL_STOP_TERM_AFTER_MS, VOICE_CONTROL_SUBCOMMAND, VoiceControlAckV1,
    VoiceControlBlock, VoiceControlCapture, VoiceControlCommand, VoiceControlCommandKind,
    VoiceControlCredential, VoiceControlErrorCode, VoiceControlEvent, VoiceControlEventKind,
    VoiceControlGetStateV1, VoiceControlHelloV1, VoiceControlLeaveV1, VoiceControlMessage,
    VoiceControlPhase, VoiceControlProtocolErrorV1, VoiceControlPtt, VoiceControlPttProblem,
    VoiceControlReason, VoiceControlRefusal, VoiceControlSetDeafenV1, VoiceControlSetMuteV1,
    VoiceControlStartV1, VoiceControlStateV1, parse_command, parse_event, to_line,
};
pub use error::{Error, Refusal, code};
pub use login::{TOKEN_ENV, credentials_path, default_config_dir, resolve_token, stored_servers};
pub use presence::{PresenceEvent, PresenceStream, StreamEnd};
pub use server::ServerAddress;
pub use types::{
    Audience, AudienceKind, ErrorBody, MAX_AUDIENCE_PRINCIPALS, Secret, Timestamp,
    VOICE_PRESENCE_SCHEMA_V1, VoiceParticipant, VoicePresenceRoom, VoicePresenceV1, VoiceRoomGrant,
    VoiceSessionResponseV1, VoiceTransmitReportV1, VoiceTransmitState, WhoAmIResponseV1,
};

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
