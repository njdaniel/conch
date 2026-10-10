//! A microphone for tests that loops a WAV file: 48 kHz, mono, 16-bit PCM, and nothing else.

use std::fmt;
use std::fs::File;
use std::io::Read;
use std::path::Path;

use crate::{AudioError, Frame, MicRead, MicSource, SAMPLE_RATE};

/// A microphone for tests: the samples of one WAV file, over and over.
///
/// Guarantees:
///
/// - Only a RIFF/WAVE file that is already 48 kHz, mono, 16-bit integer PCM is accepted.
///   Nothing is resampled, downmixed or converted: any other file is refused, with an error
///   that names the file and what was wrong with it.
/// - A file that is cut short, has a length field pointing past its end, or is not a WAV
///   file at all is refused in the same way. No input makes it panic.
/// - It loops without a seam: the sample after the file's last is its first, with nothing
///   put in between and nothing left out, so a file holding a whole number of cycles of a
///   tone plays as an unbroken tone.
/// - The file is read once, when the source is made, and never written.
pub struct WavSource {
    samples: Box<[f32]>,
    /// The sample the next read starts at. Always less than `samples.len()`.
    next: usize,
}

impl WavSource {
    /// The largest file loaded, in bytes: about eleven minutes of audio.
    pub const MAX_FILE_BYTES: u64 = 64 * 1024 * 1024;

    /// Reads a WAV file.
    ///
    /// # Errors
    ///
    /// An [`AudioError`] naming the file if it cannot be read, is larger than
    /// [`MAX_FILE_BYTES`](Self::MAX_FILE_BYTES), or is anything other than a well-formed
    /// 48 kHz mono 16-bit PCM WAV file with at least one sample.
    pub fn open(path: impl AsRef<Path>) -> Result<Self, AudioError> {
        let path = path.as_ref();
        let unreadable = |source| AudioError::WavRead {
            file: path.to_path_buf(),
            source,
        };
        let mut bytes = Vec::new();
        File::open(path)
            .map_err(unreadable)?
            // One byte past the limit is enough to know the file is over it.
            .take(Self::MAX_FILE_BYTES + 1)
            .read_to_end(&mut bytes)
            .map_err(unreadable)?;
        if bytes.len() as u64 > Self::MAX_FILE_BYTES {
            return Err(AudioError::WavTooLarge {
                file: path.to_path_buf(),
                limit: Self::MAX_FILE_BYTES,
            });
        }
        Self::from_bytes(path, &bytes)
    }

    /// Reads a WAV file that is already in memory. `name` is only used to name the file in
    /// errors.
    ///
    /// # Errors
    ///
    /// As [`open`](Self::open), except that nothing is read from disk.
    pub fn from_bytes(name: impl AsRef<Path>, bytes: &[u8]) -> Result<Self, AudioError> {
        let file = name.as_ref();
        let data = pcm_data(file, bytes)?;
        let samples: Box<[f32]> = data
            .chunks_exact(2)
            .map(|pair| {
                let mut le = [0_u8; 2];
                le.copy_from_slice(pair);
                f32::from(i16::from_le_bytes(le)) / 32_768.0
            })
            .collect();
        if samples.is_empty() {
            return Err(malformed(file, "the data chunk holds no samples"));
        }
        Ok(Self { samples, next: 0 })
    }

    /// How many samples the file holds: the length of one loop.
    #[must_use]
    pub fn len_samples(&self) -> usize {
        self.samples.len()
    }
}

impl MicSource for WavSource {
    fn read(&mut self, frame: &mut Frame) -> MicRead {
        for sample in frame.iter_mut() {
            *sample = self.samples.get(self.next).copied().unwrap_or(0.0);
            self.next += 1;
            if self.next >= self.samples.len() {
                self.next = 0;
            }
        }
        MicRead::Ready
    }
}

/// Prints no audio.
impl fmt::Debug for WavSource {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("WavSource")
            .field("len_samples", &self.samples.len())
            .field("next", &self.next)
            .finish_non_exhaustive()
    }
}

fn malformed(file: &Path, what: &'static str) -> AudioError {
    AudioError::WavMalformed {
        file: file.to_path_buf(),
        what,
    }
}

fn u16_at(bytes: &[u8], at: usize) -> Option<u16> {
    let field = bytes.get(at..at.checked_add(2)?)?;
    Some(u16::from_le_bytes(field.try_into().ok()?))
}

fn u32_at(bytes: &[u8], at: usize) -> Option<u32> {
    let field = bytes.get(at..at.checked_add(4)?)?;
    Some(u32::from_le_bytes(field.try_into().ok()?))
}

/// A chunk's four-byte name, made safe to print.
fn chunk_name(id: &[u8]) -> String {
    id.iter()
        .map(|&byte| {
            if byte.is_ascii_graphic() || byte == b' ' {
                char::from(byte)
            } else {
                '?'
            }
        })
        .collect()
}

fn too_long(file: &Path, id: &[u8], declared: u32, available: usize) -> AudioError {
    AudioError::WavChunkLength {
        file: file.to_path_buf(),
        chunk: chunk_name(id),
        declared: u64::from(declared),
        available: available as u64,
    }
}

/// Finds the sample bytes of a 48 kHz mono 16-bit PCM WAV file, or says why it is not one.
fn pcm_data<'a>(file: &Path, bytes: &'a [u8]) -> Result<&'a [u8], AudioError> {
    const HEADER: usize = 12;
    let too_short = || malformed(file, "it is shorter than a RIFF header");
    let riff_len = u32_at(bytes, 4).ok_or_else(too_short)?;
    let form = bytes.get(8..HEADER).ok_or_else(too_short)?;
    if !bytes.starts_with(b"RIFF") || form != b"WAVE" {
        return Err(malformed(file, "it does not start with a RIFF/WAVE header"));
    }
    // The RIFF length counts everything after itself: "WAVE" and the chunks. Bytes after
    // that are not part of the file's content and are ignored.
    let content = bytes
        .get(8..)
        .and_then(|rest| rest.get(..riff_len as usize))
        .ok_or_else(|| too_long(file, b"RIFF", riff_len, bytes.len().saturating_sub(8)))?;
    let mut rest = content
        .get(4..)
        .ok_or_else(|| malformed(file, "its RIFF length leaves no room for 'WAVE'"))?;

    let mut format_seen = false;
    let mut data = None;
    // A chunk is a four-byte name, a four-byte length, and that many bytes, padded to even.
    while let (Some(id), Some(len)) = (rest.get(..4), u32_at(rest, 4)) {
        let body = rest.get(8..).unwrap_or_default();
        let chunk = body
            .get(..len as usize)
            .ok_or_else(|| too_long(file, id, len, body.len()))?;
        if id == b"fmt " && !format_seen {
            check_format(file, chunk)?;
            format_seen = true;
        } else if id == b"data" && data.is_none() {
            data = Some(chunk);
        }
        if format_seen && data.is_some() {
            break;
        }
        let padded = (len as usize).saturating_add(len as usize & 1);
        rest = body.get(padded..).unwrap_or_default();
    }

    if !format_seen {
        return Err(AudioError::WavMissingChunk {
            file: file.to_path_buf(),
            chunk: "fmt ",
        });
    }
    let data = data.ok_or_else(|| AudioError::WavMissingChunk {
        file: file.to_path_buf(),
        chunk: "data",
    })?;
    if !data.len().is_multiple_of(2) {
        return Err(malformed(
            file,
            "the data chunk is not a whole number of 16-bit samples",
        ));
    }
    Ok(data)
}

/// Checks a `fmt ` chunk: integer PCM, mono, 48 kHz, 16 bits.
fn check_format(file: &Path, format: &[u8]) -> Result<(), AudioError> {
    let fields = (
        u16_at(format, 0),
        u16_at(format, 2),
        u32_at(format, 4),
        u16_at(format, 12),
        u16_at(format, 14),
    );
    let (Some(tag), Some(channels), Some(rate), Some(block_align), Some(bits)) = fields else {
        return Err(malformed(file, "the fmt chunk is shorter than 16 bytes"));
    };
    let file_path = || file.to_path_buf();
    if tag != 1 {
        let encoding = match tag {
            3 => "floating point",
            6 => "A-law",
            7 => "mu-law",
            0xFFFE => "in the extensible format",
            _ => "in an encoding other than integer PCM",
        };
        return Err(AudioError::WavEncoding {
            file: file_path(),
            tag,
            encoding,
        });
    }
    if channels != 1 {
        return Err(AudioError::WavChannels {
            file: file_path(),
            found: channels,
        });
    }
    if rate != SAMPLE_RATE {
        return Err(AudioError::WavSampleRate {
            file: file_path(),
            found: rate,
        });
    }
    if bits != 16 {
        return Err(AudioError::WavBitDepth {
            file: file_path(),
            found: bits,
        });
    }
    if block_align != 2 {
        return Err(malformed(
            file,
            "its block alignment is not the 2 bytes of 16-bit mono",
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::Lcg;
    use crate::{FRAME_LEN, SILENCE};
    use std::path::PathBuf;

    const NAME: &str = "fixture.wav";

    /// The fields of a `fmt ` chunk.
    #[derive(Clone, Copy)]
    struct Format {
        tag: u16,
        channels: u16,
        rate: u32,
        bits: u16,
    }

    const GOOD: Format = Format {
        tag: 1,
        channels: 1,
        rate: 48_000,
        bits: 16,
    };

    fn chunk(id: &[u8; 4], body: &[u8]) -> Vec<u8> {
        let mut out = id.to_vec();
        out.extend((body.len() as u32).to_le_bytes());
        out.extend(body);
        if body.len() % 2 == 1 {
            out.push(0);
        }
        out
    }

    fn format_chunk(format: Format) -> Vec<u8> {
        let block_align = format.channels * (format.bits / 8);
        let mut body = Vec::new();
        body.extend(format.tag.to_le_bytes());
        body.extend(format.channels.to_le_bytes());
        body.extend(format.rate.to_le_bytes());
        body.extend((format.rate * u32::from(block_align)).to_le_bytes());
        body.extend(block_align.to_le_bytes());
        body.extend(format.bits.to_le_bytes());
        chunk(b"fmt ", &body)
    }

    fn riff(chunks: &[Vec<u8>]) -> Vec<u8> {
        let body: Vec<u8> = chunks.concat();
        let mut out = b"RIFF".to_vec();
        out.extend((body.len() as u32 + 4).to_le_bytes());
        out.extend(b"WAVE");
        out.extend(body);
        out
    }

    fn pcm_bytes(samples: &[i16]) -> Vec<u8> {
        samples.iter().flat_map(|s| s.to_le_bytes()).collect()
    }

    /// A whole, canonical WAV file: 44 bytes of header and the samples.
    fn wav(format: Format, samples: &[i16]) -> Vec<u8> {
        riff(&[format_chunk(format), chunk(b"data", &pcm_bytes(samples))])
    }

    /// Samples that are all different, so any slip in position shows.
    fn counting(n: i16) -> Vec<i16> {
        (1..=n).collect()
    }

    fn read_samples(source: &mut WavSource, frames: usize) -> Vec<f32> {
        let mut out = Vec::new();
        let mut frame = SILENCE;
        for _ in 0..frames {
            assert_eq!(source.read(&mut frame), MicRead::Ready);
            out.extend(frame);
        }
        out
    }

    fn refused(bytes: &[u8]) -> AudioError {
        let err = WavSource::from_bytes(NAME, bytes).unwrap_err();
        assert!(
            err.to_string().starts_with("fixture.wav: "),
            "the error does not name the file: {err}"
        );
        err
    }

    #[test]
    fn it_loops_without_a_gap_or_a_repeat_at_the_seam() {
        // Lengths that are shorter than a frame, longer, and not a divisor of one.
        for len in [1_i16, 7, 479, 480, 481, 1000] {
            let samples = counting(len);
            let mut source = WavSource::from_bytes(NAME, &wav(GOOD, &samples)).unwrap();
            assert_eq!(source.len_samples(), samples.len());
            let heard = read_samples(&mut source, 25);
            assert_eq!(heard.len(), 25 * FRAME_LEN);
            for (n, sample) in heard.iter().enumerate() {
                let want = f32::from(samples[n % samples.len()]) / 32_768.0;
                assert_eq!(*sample, want, "length {len}, sample {n}");
            }
        }
    }

    #[test]
    fn a_looped_tone_has_no_click_at_the_seam() {
        // 30 ms of 400 Hz: twelve whole cycles in 1440 samples, so the end meets the start.
        let step = std::f64::consts::TAU * 400.0 / 48_000.0;
        let samples: Vec<i16> = (0..1440)
            .map(|n| (16_000.0 * (step * f64::from(n)).sin()).round() as i16)
            .collect();
        let mut source = WavSource::from_bytes(NAME, &wav(GOOD, &samples)).unwrap();
        let heard = read_samples(&mut source, 50);

        // The largest step a 400 Hz tone of this level takes between two samples; a gap or a
        // dropped sample at a seam would be a larger one.
        let largest_step = (16_000.0 / 32_768.0 * step) as f32 + 1e-3;
        for (n, pair) in heard.windows(2).enumerate() {
            assert!(
                (pair[1] - pair[0]).abs() <= largest_step,
                "a click between samples {n} and {}",
                n + 1
            );
        }
        // And it is the same tone throughout, not silence between loops.
        let mut meter = crate::SignalMeter::new(&[400.0, 800.0]).unwrap();
        for frame in heard.chunks_exact(FRAME_LEN) {
            meter.push(frame.try_into().unwrap());
        }
        assert_eq!(meter.dominant(), Some(400.0));
        assert!((meter.rms() - 16_000.0 / 32_768.0 / 2.0_f32.sqrt()).abs() < 1e-3);
    }

    #[test]
    fn samples_are_scaled_to_the_unit_range() {
        let mut source =
            WavSource::from_bytes(NAME, &wav(GOOD, &[0, i16::MAX, i16::MIN, 16_384])).unwrap();
        let heard = read_samples(&mut source, 1);
        assert_eq!(heard[..4], [0.0, 32_767.0 / 32_768.0, -1.0, 0.5]);
    }

    #[test]
    fn chunks_it_does_not_need_are_skipped() {
        let bytes = riff(&[
            chunk(b"LIST", b"odd"),
            format_chunk(GOOD),
            chunk(b"fact", &[0; 4]),
            chunk(b"data", &pcm_bytes(&counting(10))),
            chunk(b"junk", &[0xFF; 9]),
        ]);
        let mut source = WavSource::from_bytes(NAME, &bytes).unwrap();
        assert_eq!(source.len_samples(), 10);
        assert_eq!(read_samples(&mut source, 1)[9], 10.0 / 32_768.0);

        // Bytes after the end the RIFF header declares are ignored too.
        let mut trailing = wav(GOOD, &counting(10));
        trailing.extend([0xAB; 33]);
        assert_eq!(
            WavSource::from_bytes(NAME, &trailing)
                .unwrap()
                .len_samples(),
            10
        );
    }

    #[test]
    fn anything_but_48_khz_mono_16_bit_pcm_is_refused_saying_what_was_wrong() {
        struct Case {
            name: &'static str,
            format: Format,
            says: &'static str,
        }
        let cases = [
            Case {
                name: "44.1 kHz",
                format: Format {
                    rate: 44_100,
                    ..GOOD
                },
                says: "sample rate is 44100 Hz",
            },
            Case {
                name: "16 kHz",
                format: Format {
                    rate: 16_000,
                    ..GOOD
                },
                says: "sample rate is 16000 Hz",
            },
            Case {
                name: "stereo",
                format: Format {
                    channels: 2,
                    ..GOOD
                },
                says: "has 2 channels",
            },
            Case {
                name: "no channels",
                format: Format {
                    channels: 0,
                    ..GOOD
                },
                says: "has 0 channels",
            },
            Case {
                name: "32-bit float",
                format: Format {
                    tag: 3,
                    bits: 32,
                    ..GOOD
                },
                says: "floating point",
            },
            Case {
                name: "extensible",
                format: Format {
                    tag: 0xFFFE,
                    ..GOOD
                },
                says: "extensible",
            },
            Case {
                name: "mu-law",
                format: Format {
                    tag: 7,
                    bits: 8,
                    ..GOOD
                },
                says: "mu-law",
            },
            Case {
                name: "8-bit",
                format: Format { bits: 8, ..GOOD },
                says: "8 bits wide",
            },
            Case {
                name: "24-bit",
                format: Format { bits: 24, ..GOOD },
                says: "24 bits wide",
            },
        ];
        for case in cases {
            let err = refused(&wav(case.format, &counting(100)));
            assert!(err.to_string().contains(case.says), "{}: {err}", case.name);
        }
    }

    #[test]
    fn each_refusal_is_its_own_kind_of_error() {
        let rate = refused(&wav(
            Format {
                rate: 44_100,
                ..GOOD
            },
            &[1],
        ));
        assert!(matches!(
            rate,
            AudioError::WavSampleRate { found: 44_100, .. }
        ));
        let stereo = refused(&wav(
            Format {
                channels: 2,
                ..GOOD
            },
            &[1, 2],
        ));
        assert!(matches!(stereo, AudioError::WavChannels { found: 2, .. }));
        let float = refused(&wav(
            Format {
                tag: 3,
                bits: 32,
                ..GOOD
            },
            &[1, 2],
        ));
        assert!(matches!(float, AudioError::WavEncoding { tag: 3, .. }));
        let wide = refused(&wav(Format { bits: 24, ..GOOD }, &[1, 2, 3]));
        assert!(matches!(wide, AudioError::WavBitDepth { found: 24, .. }));
    }

    #[test]
    fn every_truncation_of_a_good_file_is_refused() {
        let whole = wav(GOOD, &counting(50));
        assert!(WavSource::from_bytes(NAME, &whole).is_ok());
        for len in 0..whole.len() {
            let err = refused(&whole[..len]);
            assert!(
                matches!(
                    err,
                    AudioError::WavMalformed { .. } | AudioError::WavChunkLength { .. }
                ),
                "cut to {len} bytes: {err}"
            );
        }
    }

    #[test]
    fn a_header_cut_short_is_refused_even_when_the_riff_length_agrees() {
        // The RIFF length is made to match the cut file, so only the chunks give it away.
        let whole = wav(GOOD, &counting(50));
        for len in 12..whole.len() {
            let mut cut = whole[..len].to_vec();
            cut[4..8].copy_from_slice(&(len as u32 - 8).to_le_bytes());
            let err = refused(&cut);
            assert!(
                matches!(
                    err,
                    AudioError::WavMalformed { .. }
                        | AudioError::WavChunkLength { .. }
                        | AudioError::WavMissingChunk { .. }
                ),
                "cut to {len} bytes: {err}"
            );
        }

        // A fmt chunk that is whole but too short to hold the fields.
        let short = riff(&[
            chunk(b"fmt ", &[1, 0, 1, 0, 0x80, 0xBB, 0, 0]),
            chunk(b"data", &pcm_bytes(&counting(4))),
        ]);
        let err = refused(&short);
        assert!(err.to_string().contains("fmt chunk is shorter"), "{err}");
    }

    #[test]
    fn an_absurd_length_field_is_refused() {
        let good = wav(GOOD, &counting(50));
        assert_eq!(good.len(), 144);
        // The length fields of a canonical file (RIFF at 4, fmt at 16, data at 40) and the
        // bytes that really follow each.
        for (at, chunk, follows) in [(4, "RIFF", 136_u32), (16, "fmt ", 124), (40, "data", 100)] {
            // From hopeless down to one byte too many.
            for absurd in [u32::MAX, 0x7FFF_FFFF, 1_000_000, follows + 1] {
                let mut bytes = good.clone();
                bytes[at..at + 4].copy_from_slice(&absurd.to_le_bytes());
                let err = refused(&bytes);
                let AudioError::WavChunkLength {
                    chunk: named,
                    declared,
                    available,
                    ..
                } = &err
                else {
                    panic!("{chunk} length {absurd}: {err}");
                };
                assert_eq!(named, chunk);
                assert_eq!(*declared, u64::from(absurd));
                assert_eq!(*available, u64::from(follows));
                assert!(err.to_string().contains(&absurd.to_string()), "{err}");
            }
        }
    }

    #[test]
    fn files_that_are_not_wav_or_hold_no_samples_are_refused() {
        struct Case {
            name: &'static str,
            bytes: Vec<u8>,
            says: &'static str,
        }
        let mut rifx = wav(GOOD, &counting(4));
        rifx[..4].copy_from_slice(b"RIFX");
        let mut avi = wav(GOOD, &counting(4));
        avi[8..12].copy_from_slice(b"AVI ");
        let mut misaligned = wav(GOOD, &counting(4));
        misaligned[32] = 4;
        let cases = [
            Case {
                name: "empty",
                bytes: Vec::new(),
                says: "shorter than a RIFF header",
            },
            Case {
                name: "text",
                bytes: b"this is not audio at all".to_vec(),
                says: "RIFF/WAVE header",
            },
            Case {
                name: "big-endian RIFX",
                bytes: rifx,
                says: "RIFF/WAVE header",
            },
            Case {
                name: "another RIFF form",
                bytes: avi,
                says: "RIFF/WAVE header",
            },
            Case {
                name: "RIFF length of zero",
                bytes: b"RIFF\0\0\0\0WAVE".to_vec(),
                says: "no room for 'WAVE'",
            },
            Case {
                name: "no chunks",
                bytes: riff(&[]),
                says: "no 'fmt ' chunk",
            },
            Case {
                name: "no fmt chunk",
                bytes: riff(&[chunk(b"data", &pcm_bytes(&counting(4)))]),
                says: "no 'fmt ' chunk",
            },
            Case {
                name: "no data chunk",
                bytes: riff(&[format_chunk(GOOD)]),
                says: "no 'data' chunk",
            },
            Case {
                name: "no samples",
                bytes: wav(GOOD, &[]),
                says: "holds no samples",
            },
            Case {
                name: "half a sample",
                bytes: riff(&[format_chunk(GOOD), chunk(b"data", &[1, 2, 3])]),
                says: "whole number of 16-bit samples",
            },
            Case {
                name: "block alignment that contradicts the rest",
                bytes: misaligned,
                says: "block alignment",
            },
        ];
        for case in cases {
            let err = refused(&case.bytes);
            assert!(err.to_string().contains(case.says), "{}: {err}", case.name);
        }
    }

    #[test]
    fn a_chunk_name_that_is_not_text_is_printed_safely() {
        let bytes = riff(&[format_chunk(GOOD), {
            let mut bad = vec![0x1B, b'[', 0xFF, b'm'];
            bad.extend(5000_u32.to_le_bytes());
            bad
        }]);
        let message = refused(&bytes).to_string();
        assert!(message.contains("'?[?m'"), "{message}");
        assert!(message.is_ascii() && !message.contains('\u{1b}'));
    }

    #[test]
    fn random_bytes_never_panic() {
        let mut rng = Lcg::new(181);
        for _ in 0..500 {
            let len = rng.below(300) as usize;
            let bytes: Vec<u8> = (0..len).map(|_| rng.byte()).collect();
            // Random bytes are never a WAV file: all that matters is that it says so.
            assert!(WavSource::from_bytes(NAME, &bytes).is_err());
        }
    }

    #[test]
    fn random_damage_to_a_good_file_never_panics() {
        let good = wav(GOOD, &counting(200));
        let mut rng = Lcg::new(2024);
        let (mut accepted, mut turned_away) = (0, 0);
        for _ in 0..2000 {
            let mut bytes = good.clone();
            // Mostly the header, where every byte means something.
            for _ in 0..=rng.below(4) {
                let within = if rng.below(4) == 0 {
                    bytes.len() as u32
                } else {
                    44
                };
                bytes[rng.below(within) as usize] = rng.byte();
            }
            if rng.below(3) == 0 {
                bytes.truncate(rng.below(bytes.len() as u32 + 1) as usize);
            }
            // Either answer is fine; reading whatever it accepted must be fine too.
            match WavSource::from_bytes(NAME, &bytes) {
                Ok(mut source) => {
                    assert!(source.len_samples() > 0);
                    read_samples(&mut source, 3);
                    accepted += 1;
                }
                Err(err) => {
                    assert!(err.to_string().starts_with("fixture.wav: "), "{err}");
                    turned_away += 1;
                }
            }
        }
        assert!(
            accepted > 0 && turned_away > 0,
            "{accepted} accepted, {turned_away} refused"
        );
    }

    /// A file in the system's temporary directory, removed when the test ends, pass or fail.
    struct TempWav(PathBuf);

    impl TempWav {
        fn new(name: &str, bytes: &[u8]) -> Self {
            let path = std::env::temp_dir().join(format!(
                "conch-voice-audio-{}-{name}.wav",
                std::process::id()
            ));
            std::fs::write(&path, bytes).unwrap();
            Self(path)
        }
    }

    impl Drop for TempWav {
        fn drop(&mut self) {
            let _ = std::fs::remove_file(&self.0);
        }
    }

    #[test]
    fn a_file_on_disk_is_read_and_looped() {
        let samples = counting(700);
        let file = TempWav::new("good", &wav(GOOD, &samples));
        let mut source = WavSource::open(&file.0).unwrap();
        assert_eq!(source.len_samples(), 700);
        let heard = read_samples(&mut source, 3);
        for (n, sample) in heard.iter().enumerate() {
            assert_eq!(
                *sample,
                f32::from(samples[n % 700]) / 32_768.0,
                "sample {n}"
            );
        }
        assert!(!format!("{source:?}").contains("0.0"));
    }

    #[test]
    fn a_refused_file_on_disk_is_named_in_the_error() {
        let file = TempWav::new(
            "stereo",
            &wav(
                Format {
                    channels: 2,
                    ..GOOD
                },
                &counting(8),
            ),
        );
        let err = WavSource::open(&file.0).unwrap_err();
        let message = err.to_string();
        assert!(message.contains(file.0.to_str().unwrap()), "{message}");
        assert!(message.contains("has 2 channels"), "{message}");
    }

    #[test]
    fn a_file_that_is_not_there_is_named_in_the_error() {
        let path = std::env::temp_dir().join(format!(
            "conch-voice-audio-{}-does-not-exist.wav",
            std::process::id()
        ));
        let err = WavSource::open(&path).unwrap_err();
        assert!(matches!(err, AudioError::WavRead { .. }));
        assert!(err.to_string().contains(path.to_str().unwrap()), "{err}");
    }
}
