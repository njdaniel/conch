//! The decision logic of `conch-voice`, with no I/O of its own.
//!
//! Input: the configuration file's text, bytes read from a keyboard's event device, line
//! commands, the conditions a transmission depends on (connected, allowed to publish, a
//! microphone, a key device), connection outcomes, and the time, which every call is given.
//! Output: commands for the transmit gate, transmit reports to send, what to show, and what
//! to do after a disconnect (stop, ask for a session now, or retry after a wait).
//! Owns: the push-to-talk state machine, with `max_transmit_secs`, and the connection
//! policy. It keeps nothing about any key other than the configured ones.
//!
//! It opens no file, device or socket, starts no thread, reads no clock and draws no random
//! number, and it logs nothing. It depends on neither `conch-voice-api` nor
//! `conch-voice-audio`: its inputs and outputs are its own enums, and the binary maps the
//! other crates' types onto them.
//!
//! - [`Config`]: the TOML configuration, its defaults, and layers of overrides.
//! - [`KeyDecoder`]: `input_event` records reduced to [`KeyEvent`]s of the configured keys.
//! - [`Ptt`]: the push-to-talk state machine.
//! - [`ConnectionPolicy`]: what to do after each [`Outcome`].
//!
//! Design: `docs/design/conch-voice.md` §3 to §6 and §8.

// CLAUDE.md: no unwrap/expect outside tests and main.
#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

// `struct input_event` begins with a `struct timeval`, which is two machine words. On a
// 32-bit system the record is 16 bytes, not 24, and the decoder would read every event out
// of step. Linux on 64 bits is the only target (ADR-006).
#[cfg(not(all(target_pointer_width = "64", target_endian = "little")))]
compile_error!(
    "conch-voice-control decodes the 24-byte little-endian input_event of 64-bit Linux and \
     must not be built for any other pointer width or byte order"
);

mod config;
mod error;
mod keys;
mod policy;
mod ptt;

pub use config::{
    AudioConfig, Config, ConfigOverrides, DEFAULT_MAX_TRANSMIT_SECS, DEFAULT_RELEASE_TAIL_MS,
    DEFAULT_SERVER, KeysConfig, MAX_MAX_TRANSMIT_SECS, MAX_RELEASE_TAIL_MS,
};
pub use error::Error;
pub use keys::{KEY_CODE_MAX, Key, KeyAction, KeyBindings, KeyCode, KeyDecoder, KeyEvent};
pub use policy::{
    BACKOFF_MAX, BACKOFF_MIN, ConnectionPolicy, EXIT_STOPPED, NextStep, Outcome, STABLE_AFTER,
};
pub use ptt::{
    GateCommand, LineCommand, Ptt, PttInput, PttOutput, PttStatus, ShutReason, ShutReasons,
    TransmitReport,
};

/// Bytes in one Linux `input_event` record on a 64-bit system: 16 of time, then a `u16`
/// type, a `u16` code and an `i32` value.
pub const INPUT_EVENT_LEN: usize = 24;

#[cfg(test)]
mod tests {
    #[test]
    fn an_input_event_is_24_bytes() {
        assert_eq!(super::INPUT_EVENT_LEN, 16 + 2 + 2 + 4);
    }
}
