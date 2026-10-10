//! `conch-voice`: the Conch voice client (ADR-006).
//!
//! Input: the command line and the environment. Output: what the library crate
//! `conch_voice` documents, and an exit code: 0 after `quit`, the end of input or a signal;
//! 2 for a mistake on the command line; 1 when the client stopped for a reason it printed.
//! Owns: the order things start in. The logger is installed before anything else runs, so
//! nothing the SDK logs can reach standard error unfiltered.
//!
//! Design: `docs/design/conch-voice.md`.

use std::sync::Arc;

use conch_voice::cli::{self, Environment};
use conch_voice::logger::Logger;
use conch_voice::secrets::Scrubber;
use log::LevelFilter;

fn main() {
    // First, before the command line is even read: from here on every log record in the
    // process goes through this logger, and nothing can install another.
    let scrubber = Scrubber::new();
    let Some(logger) = Logger::install(LevelFilter::Warn, Arc::clone(&scrubber)) else {
        eprintln!("conch-voice: another logger was installed first; refusing to run");
        std::process::exit(1);
    };
    // A panic's message can hold whatever the code that panicked was working on, and the
    // SDK is not this program's code. It is scrubbed like a log record, and it ends the
    // process: half a client is not left running with a microphone.
    let panic_scrubber = Arc::clone(&scrubber);
    std::panic::set_hook(Box::new(move |panic| {
        let message = panic.payload_as_str().unwrap_or("no message");
        let place = panic
            .location()
            .map(|at| format!(" at {}:{}", at.file(), at.line()))
            .unwrap_or_default();
        eprintln!(
            "conch-voice: internal error{place}: {}",
            panic_scrubber.scrub(message)
        );
        std::process::exit(101);
    }));

    let args = match cli::parse(std::env::args_os()) {
        Ok(args) => args,
        // Help, the version, or a mistake: clap prints it and knows the exit code.
        Err(error) => error.exit(),
    };
    logger.set_level(args.log_level);

    let code = match conch_voice::join(&args, &Environment::from_process(), Arc::clone(&scrubber)) {
        Ok(()) => 0,
        Err(error) => {
            // No error holds a secret or a control character by construction; this is the
            // last line the program writes, and it is made sure of all the same.
            eprintln!("conch-voice: {}", scrubber.scrub_line(&error.to_string()));
            i32::from(error.exit_code())
        }
    };
    // Not a return: the SDK's threads and the thread reading standard input would be
    // waited for.
    std::process::exit(code);
}
