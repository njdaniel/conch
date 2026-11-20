//! The desktop watcher event defined in `pkg/schema/mute_key_event.go`.

use serde::Serialize;

const SCHEMA: &str = "conch.mute_key_event.v1";

/// One press of the configured mute key, with no physical key information.
#[derive(Debug, Serialize)]
pub struct MuteKeyEventV1 {
    schema: &'static str,
    press: bool,
}

impl MuteKeyEventV1 {
    /// The sole event the watcher reports.
    pub const PRESS: Self = Self {
        schema: SCHEMA,
        press: true,
    };
}
