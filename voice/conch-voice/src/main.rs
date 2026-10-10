//! `conch-voice`: the Conch voice client (ADR-006).
//!
//! Input: the command line and the environment. Output: what the library crate
//! `conch_voice` documents, and an exit code: 0 after `quit`, the end of input or a signal;
//! 2 for a mistake on the command line; 1 when the client stopped for a reason it printed;
//! 101 for an internal error.
//! Owns: the order things start in, and the exit. The logger is installed before anything
//! else runs, so nothing the SDK logs can reach standard error unfiltered. The exit waits
//! for nobody: what is still to be written to standard error is given half a second, and
//! the process then ends whether or not anybody read it.
//!
//! Design: `docs/design/conch-voice.md`.

use std::sync::Arc;

use conch_voice::cli::{self, Environment, Invocation};
use conch_voice::lines::{exit, last_words};
use conch_voice::logger::Logger;
use conch_voice::secrets::Scrubber;
use conch_voice::{LAST_WORDS_LIMIT, error_line, panic_line};
use log::LevelFilter;

fn main() {
    // First, before the command line is even read: from here on every log record in the
    // process goes through this logger, and nothing can install another.
    let scrubber = Scrubber::new();
    let Some(logger) = Logger::install(LevelFilter::Warn, Arc::clone(&scrubber)) else {
        last_words("conch-voice: another logger was installed first; refusing to run".into());
        exit(1);
    };
    // A panic's message can hold whatever the code that panicked was working on, and the
    // SDK is not this program's code. It is scrubbed like a log record, and it ends the
    // process: half a client is not left running with a microphone. The process ends
    // whether or not the line could be written: a standard error nobody reads must not
    // keep a client that has panicked alive.
    let panic_scrubber = Arc::clone(&scrubber);
    std::panic::set_hook(Box::new(move |panic| {
        let message = panic.payload_as_str().unwrap_or("no message");
        let place = panic.location().map(|at| (at.file(), at.line()));
        last_words(panic_line(&panic_scrubber, message, place));
        exit(101);
    }));

    let args = match cli::parse(std::env::args_os()) {
        Ok(args) => args,
        // Help, the version, or a mistake: clap prints it and knows the exit code.
        Err(error) => error.exit(),
    };
    let result = match &args {
        Invocation::Join(args) => {
            logger.set_level(args.log_level);
            conch_voice::join(args, &Environment::from_process(), Arc::clone(&scrubber))
        }
        Invocation::Devices => conch_voice::devices(),
        Invocation::Keys { device } => conch_voice::keys(device),
    };
    // What was logged is given a moment to be written, and then the reason the client
    // stopped for, if it has one. Neither is waited for beyond its limit.
    logger.flush_within(LAST_WORDS_LIMIT);
    let code = match result {
        Ok(()) => 0,
        Err(error) => {
            last_words(error_line(&scrubber, &error));
            i32::from(error.exit_code())
        }
    };
    // Not a return: the SDK's threads and the thread reading standard input would be
    // waited for.
    exit(code);
}
