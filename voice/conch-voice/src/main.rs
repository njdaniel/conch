//! `conch-voice`: the Conch voice client (ADR-006).
//!
//! Input: the command line and the environment. Output: what the library crate
//! `conch_voice` documents, and an exit code: 0 after `quit`, the end of input or a signal
//! (for `managed`, after a final state of stopped); 2 for a mistake on the command line, or
//! a `managed` with no event pipe; 1 when the client stopped for a reason it printed (for
//! `managed`, a final state of failed); 101 for an internal error.
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
    // A managed helper's owner is the process that started it: recorded before anything
    // else, so that an owner that dies at any moment after is noticed (`managed.rs`).
    let parent = std::os::unix::process::parent_id();
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

    let raw_args: Vec<_> = std::env::args_os().collect();
    let args = match cli::parse(raw_args.iter().cloned()) {
        Ok(args) => args,
        // Help and the version: clap prints them to standard output and exits 0.
        Err(error) if !error.use_stderr() => error.exit(),
        // A mistake: clap quotes what was typed, so it is scrubbed before it is printed.
        Err(error) => {
            let text = cli::usage_error_text(&error, &raw_args);
            last_words(text.trim_end_matches('\n').to_owned());
            exit(error.exit_code());
        }
    };
    let result = match &args {
        Invocation::Join(args) => {
            logger.set_level(args.log_level);
            conch_voice::join(args, &Environment::from_process(), Arc::clone(&scrubber))
        }
        Invocation::Managed(args) => {
            logger.set_level(args.log_level);
            conch_voice::managed(
                args,
                &Environment::from_process(),
                Arc::clone(&scrubber),
                parent,
            )
        }
        Invocation::Devices { json } => {
            conch_voice::devices(*json, &Environment::from_process(), &scrubber)
        }
        Invocation::Keys { device } => conch_voice::keys(device),
        Invocation::ConfigSet { key, value } => {
            conch_voice::config_set(&Environment::from_process(), *key, value)
        }
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
