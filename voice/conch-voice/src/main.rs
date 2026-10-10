//! `conch-voice`: the Conch voice client (ADR-006).
//!
//! Input: the command line. Output: for now, its version. The commands (`join`, `devices`,
//! `keys`) arrive with issues #183 to #185; this crate exists so that the native toolchain
//! the LiveKit SDK needs is built, linked and run by `make check` and CI from the first commit.
//!
//! Design: `docs/design/conch-voice.md`.

use std::process::ExitCode;

fn main() -> ExitCode {
    let mut args = std::env::args().skip(1);
    match (args.next().as_deref(), args.next()) {
        (Some("--version" | "-V"), None) => {
            println!("{}", version_line());
            ExitCode::SUCCESS
        }
        _ => {
            eprintln!("usage: conch-voice --version");
            ExitCode::from(2)
        }
    }
}

/// One line: this binary's version and the audio frame it is built for.
fn version_line() -> String {
    format!(
        "conch-voice {} ({} Hz, {} ms frames)",
        conch_voice_api::VERSION,
        conch_voice_audio::SAMPLE_RATE,
        conch_voice_audio::FRAME_MS,
    )
}

#[cfg(test)]
mod tests {
    use super::version_line;

    #[test]
    fn the_version_line_names_the_version() {
        let line = version_line();
        assert!(line.starts_with("conch-voice "), "{line}");
        assert!(line.contains(conch_voice_api::VERSION), "{line}");
    }

    /// Creates the SDK's audio source, which is native code in libwebrtc. Until the binary
    /// itself uses the SDK (issue #183), this test is what proves that the pinned clang
    /// compiled the SDK's C++ and that the verified libwebrtc links and runs.
    #[test]
    fn the_sdk_links_and_its_native_code_runs() {
        use livekit::webrtc::audio_source::{AudioSourceOptions, native::NativeAudioSource};

        let source = NativeAudioSource::new(
            AudioSourceOptions::default(),
            conch_voice_audio::SAMPLE_RATE,
            1,
            100,
        );
        assert_eq!(source.sample_rate(), conch_voice_audio::SAMPLE_RATE);
        assert_eq!(source.num_channels(), 1);
    }
}
