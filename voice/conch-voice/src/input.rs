//! Where presses come from in this issue: lines on standard input, and signals.
//!
//! Standard input carries one command per line (`down`, `up`, `mute`, `deafen`, `quit`;
//! `docs/design/conch-voice.md` §4). It is how the tests drive the client and the fallback
//! when there is no key device; reading a real key is issue #185. A line that is not a
//! command is reported as such and is not kept: nothing typed into the client is echoed.

use conch_voice_control::LineCommand;
use tokio::signal::unix::{SignalKind, signal};
use tokio::sync::mpsc;

use crate::session::Input;

/// Reads standard input on a thread of its own, for as long as it lasts. Its end is
/// [`Input::Eof`], so a client never outlives a wrapper that was driving it with the gate
/// left open.
pub fn stdin_lines(inputs: mpsc::UnboundedSender<Input>) {
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
        let _ = inputs.send(Input::Eof);
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
