//! Where presses come from besides the key device (`keydev.rs`): lines on standard input,
//! and signals.
//!
//! Standard input carries one command per line (`down`, `up`, `mute`, `deafen`, `quit`;
//! `docs/design/conch-voice.md` §4). It is how the tests drive the client and the fallback
//! when there is no key device; it is read whether or not a key device is watched. A line
//! that is not a command is reported as such and is not kept: nothing typed into the
//! client is echoed.
//!
//! A line is read up to [`MAX_LINE_BYTES`] and no further. Whatever writes to standard
//! input decides how long a line is, and a line with no end must not be collected for
//! ever: what is beyond the limit is discarded as it is read, up to the line's end, and the
//! line counts as one that is not a command.

use std::io::{BufRead, BufReader, Read};

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

/// The most of one line that is kept. The longest command is six letters.
pub const MAX_LINE_BYTES: usize = 256;

/// Reads `input` to its end, one line at a time, and hands each to `take`: a command, or
/// [`Input::UnknownLine`] for anything else, a line longer than [`MAX_LINE_BYTES`]
/// included. At the end of `input`, or when it cannot be read, it hands over
/// [`Input::Eof`]. It stops early if `take` returns false.
///
/// It holds at most one buffer of input at a time, however long a line is.
pub fn read_lines(input: impl Read, mut take: impl FnMut(Input) -> bool) {
    let mut input = BufReader::new(input);
    let mut line = Line::new();
    loop {
        let buffered = match input.fill_buf() {
            Ok(buffered) if !buffered.is_empty() => buffered,
            // The end of input, or an input that can no longer be read.
            Ok(_) | Err(_) => break,
        };
        let (part, ended) = match buffered.iter().position(|byte| *byte == b'\n') {
            Some(at) => (buffered.get(..at).unwrap_or_default(), true),
            None => (buffered, false),
        };
        line.push(part);
        let read = part.len() + usize::from(ended);
        input.consume(read);
        if ended && !take(line.finish()) {
            return;
        }
    }
    // A last line with no line break is a line all the same, as it is to a shell.
    if line.begun() && !take(line.finish()) {
        return;
    }
    take(Input::Eof);
}

/// One line as it is read: its first [`MAX_LINE_BYTES`] bytes, and whether there were more.
struct Line {
    kept: Vec<u8>,
    too_long: bool,
}

impl Line {
    fn new() -> Self {
        Self {
            kept: Vec::with_capacity(MAX_LINE_BYTES),
            too_long: false,
        }
    }

    /// Takes the next part of the line. What does not fit under the limit is not kept.
    fn push(&mut self, part: &[u8]) {
        let room = MAX_LINE_BYTES.saturating_sub(self.kept.len());
        match part.get(..room) {
            Some(fits) if part.len() > room => {
                self.kept.extend_from_slice(fits);
                self.too_long = true;
            }
            _ => self.kept.extend_from_slice(part),
        }
    }

    /// Whether anything of a line has been read since the last one ended.
    fn begun(&self) -> bool {
        !self.kept.is_empty() || self.too_long
    }

    /// The line is complete: what it was, leaving this empty for the next. A line that
    /// went beyond the limit is not a command, whatever it began with.
    fn finish(&mut self) -> Input {
        let command = std::str::from_utf8(&self.kept)
            .ok()
            .filter(|_| !self.too_long)
            .and_then(|text| text.parse::<LineCommand>().ok());
        self.kept.clear();
        self.too_long = false;
        command.map_or(Input::UnknownLine, Input::Line)
    }
}

/// What the end of standard input is sent as under `end`; anything else is sent as it is.
fn at_the_end(input: Input, end: AtEnd) -> Input {
    match input {
        // With a key device watched, the end of standard input is a release and no more:
        // the client runs on, driven by the device.
        Input::Eof if end == AtEnd::Release => Input::Line(LineCommand::Up),
        input => input,
    }
}

/// Reads standard input on a thread of its own, for as long as it lasts. Its end is
/// [`Input::Eof`], or with [`AtEnd::Release`] an `up`; either way a wrapper that dies after
/// `down` cannot leave the gate open.
pub fn stdin_lines(inputs: mpsc::UnboundedSender<Input>, at_end: AtEnd) {
    std::thread::spawn(move || {
        read_lines(std::io::stdin(), |input| {
            inputs.send(at_the_end(input, at_end)).is_ok()
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

#[cfg(test)]
mod tests {
    use std::io::Cursor;

    use conch_voice_control::LineCommand::{Deafen, Down, Mute, Quit, Up};

    use super::*;

    fn read(input: impl Read) -> Vec<Input> {
        let mut taken = Vec::new();
        read_lines(input, |input| {
            taken.push(input);
            true
        });
        taken
    }

    #[test]
    fn each_line_is_a_command_or_an_unknown_line_and_the_end_is_the_end() {
        let taken = read(Cursor::new("down\nup\r\n  mute \ndeafen\n\nDOWN\nquit\n"));
        assert_eq!(
            taken,
            [
                Input::Line(Down),
                Input::Line(Up),
                Input::Line(Mute),
                Input::Line(Deafen),
                Input::UnknownLine,
                Input::UnknownLine,
                Input::Line(Quit),
                Input::Eof,
            ]
        );
        assert_eq!(read(Cursor::new("")), [Input::Eof]);
        assert_eq!(read(Cursor::new("\n")), [Input::UnknownLine, Input::Eof]);
    }

    #[test]
    fn a_last_line_with_no_line_break_still_counts() {
        assert_eq!(read(Cursor::new("down")), [Input::Line(Down), Input::Eof]);
        assert_eq!(
            read(Cursor::new("down\nnonsense")),
            [Input::Line(Down), Input::UnknownLine, Input::Eof]
        );
    }

    #[test]
    fn bytes_that_are_not_text_are_an_unknown_line() {
        let taken = read(Cursor::new(b"\xff\xfe\x00down\nup\n".to_vec()));
        assert_eq!(taken, [Input::UnknownLine, Input::Line(Up), Input::Eof]);
    }

    /// An input that never ends and never has a line break: a megabyte at a time, for as
    /// long as it is read. It counts what was read of it.
    struct Endless {
        read: usize,
        until: usize,
        then: &'static [u8],
    }

    impl Read for Endless {
        fn read(&mut self, buffer: &mut [u8]) -> std::io::Result<usize> {
            if self.read >= self.until {
                let n = self.then.len().min(buffer.len());
                buffer[..n].copy_from_slice(&self.then[..n]);
                self.then = &self.then[n..];
                return Ok(n);
            }
            let n = buffer.len().min(self.until - self.read);
            buffer[..n].fill(b'd');
            self.read += n;
            Ok(n)
        }
    }

    #[test]
    fn a_line_longer_than_the_limit_is_an_unknown_line_and_is_not_kept() {
        // One byte over the limit, and exactly at it.
        let over = format!("{}\nup\n", "x".repeat(MAX_LINE_BYTES + 1));
        assert_eq!(
            read(Cursor::new(over)),
            [Input::UnknownLine, Input::Line(Up), Input::Eof]
        );
        let padded = format!("{}down\nup\n", " ".repeat(MAX_LINE_BYTES - 4));
        assert_eq!(
            read(Cursor::new(padded)),
            [Input::Line(Down), Input::Line(Up), Input::Eof]
        );
        // A command at the start of a line that goes on beyond the limit is not a command.
        let disguised = format!("down{}\nup\n", " ".repeat(MAX_LINE_BYTES));
        assert_eq!(
            read(Cursor::new(disguised)),
            [Input::UnknownLine, Input::Line(Up), Input::Eof]
        );

        // Sixty-four megabytes with no line break, then a line break and a command. What
        // came before the break is one unknown line; reading it kept none of it.
        let endless = Endless {
            read: 0,
            until: 64 * 1024 * 1024,
            then: b"\ndown\n",
        };
        assert_eq!(
            read(endless),
            [Input::UnknownLine, Input::Line(Down), Input::Eof]
        );
    }

    #[test]
    fn what_is_kept_of_a_line_never_exceeds_the_limit() {
        let mut line = Line::new();
        let room = line.kept.capacity();
        for part in [&[b'x'; 200][..], &[b'x'; 200][..], &[b'x'; 8192][..]] {
            line.push(part);
        }
        for _ in 0..1000 {
            line.push(&[b'x'; 8192]);
        }
        assert_eq!(line.kept.len(), MAX_LINE_BYTES);
        assert_eq!(
            line.kept.capacity(),
            room,
            "eight megabytes later it has not grown"
        );
        assert!(line.too_long && line.begun());
        assert_eq!(line.finish(), Input::UnknownLine);
        assert!(!line.begun() && line.kept.capacity() == room);
    }

    #[test]
    fn reading_stops_when_nobody_takes_the_lines_any_more() {
        let mut taken = Vec::new();
        read_lines(Cursor::new("down\nup\nquit\n"), |input| {
            taken.push(input);
            taken.len() < 2
        });
        assert_eq!(taken, [Input::Line(Down), Input::Line(Up)]);
    }

    #[test]
    fn the_end_of_input_is_a_release_and_no_more_when_a_key_device_is_watched() {
        // What the session loop gets when standard input ends: with a key device watched,
        // an `up` and nothing more (the client runs on, driven by the device); without
        // one, the end, which is a release and then `quit`.
        assert_eq!(
            at_the_end(Input::Eof, AtEnd::Release),
            Input::Line(LineCommand::Up)
        );
        assert_eq!(at_the_end(Input::Eof, AtEnd::Quit), Input::Eof);
        assert_eq!(
            at_the_end(Input::Line(Down), AtEnd::Release),
            Input::Line(Down)
        );
        assert_eq!(
            at_the_end(Input::UnknownLine, AtEnd::Quit),
            Input::UnknownLine
        );
    }
}
