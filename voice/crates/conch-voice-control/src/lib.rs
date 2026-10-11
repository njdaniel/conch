//! The decision logic of `conch-voice`, with no I/O of its own.
//!
//! Input: the configuration file's text, bytes read from a keyboard's event device, line
//! commands, connection outcomes, and a clock.
//! Output: commands for the transmit gate, transmit reports to send, and what to do after a
//! disconnect (stop, ask for a session now, or retry after a wait).
//! Owns: the push-to-talk state machine and the connection policy. It keeps nothing about any
//! key other than the configured ones.
//!
//! Design: `docs/design/conch-voice.md` §4, §5 and §8. Empty until issue #182.

// CLAUDE.md: no unwrap/expect outside tests and main.
#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

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
