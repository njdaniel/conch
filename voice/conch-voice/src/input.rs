//! Where presses come from besides the key device (`keydev.rs`): lines on standard input,
//! and signals.
//!
//! Standard input carries one command per line (`down`, `up`, `mute`, `deafen`, `quit`;
//! `docs/design/conch-voice.md` §4). It is how the tests drive the client and the fallback
//! when there is no key device; it is read whether or not a key device is watched. A line
//! that is not a command is reported as such and is not kept: nothing typed into the
//! client is echoed.

use conch_voice_control::LineCommand;
use tokio::signal::unix::{SignalKind, signal};
use tokio::sync::mpsc;

use crate::session::Input;

/// What the end of standard input means.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AtEnd {
    /// A release and then `quit`: standard input is the talk key, and a client must not
    /// outlive a wrapper that was driving it.
    Quit,
    /// A release and no more: a key device is watched, so a client started with no
    /// standard input runs on until `quit`, Ctrl-C or SIGTERM.
    Release,
}

/// Reads standard input on a thread of its own, for as long as it lasts. Its end is
/// [`Input::Eof`], or with [`AtEnd::Release`] an `up`; either way a wrapper that dies after
/// `down` cannot leave the gate open.
pub fn stdin_lines(inputs: mpsc::UnboundedSender<Input>, at_end: AtEnd) {
    std::thread::spawn(move || {
        for line in std::io::stdin().lines() {
            let Ok(line) = line else { break };
            let input = match line.parse::<LineCommand>() {
                Ok(command) => Input::Line(command),
                Err(_) => Input::UnknownLine,
            };
            if inputs.send(input).is_err() {
                return;
            }
        }
        let _ = inputs.send(match at_end {
            AtEnd::Quit => Input::Eof,
            AtEnd::Release => Input::Line(LineCommand::Up),
        });
    });
}

/// Turns SIGINT (Ctrl-C) and SIGTERM into [`Input::Signal`]. Must be called inside the
/// runtime.
///
/// # Errors
///
/// What the operating system said, if a handler could not be installed.
pub fn signals(inputs: mpsc::UnboundedSender<Input>) -> std::io::Result<()> {
    let mut interrupt = signal(SignalKind::interrupt())?;
    let mut terminate = signal(SignalKind::terminate())?;
    tokio::spawn(async move {
        loop {
            tokio::select! {
                _ = interrupt.recv() => {}
                _ = terminate.recv() => {}
            }
            if inputs.send(Input::Signal).is_err() {
                return;
            }
        }
    });
    Ok(())
}
