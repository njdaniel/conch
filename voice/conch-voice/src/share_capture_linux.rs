//! The Linux graph of a screen share's sound: PipeWire through `dmn-audio`, compiled only
//! under `cfg(target_os = "linux")` (`docs/design/linux-share-audio.md` §5.1).
//!
//! Input: `dmn-audio`'s node table (`NodeTable::streams_with_metadata`: each application
//! output stream's `object.serial`, its owning client's `pipewire.sec.pid`, and its advisory
//! `application.process.id` and `application.name`, as `dmn-audio` reads them; nothing here
//! reads a property of a node itself). Output: a [`Graph`] the share's engine uses, whose
//! captures are `dmn-audio`'s `StreamCapture` of one stream by its serial at 48 kHz in two
//! channels, and nothing else: no other capture type of `dmn-audio` is named in this file.
//! Owns: the captures it opens, until the engine closes them, and a connection to PipeWire
//! of its own only when the one the helper started with was lost.
//!
//! The connection is the helper's own, the one its microphone and speakers use
//! ([`crate::audio::AudioSystem`]): a share opens no connection per application. When PipeWire
//! goes away that connection is not used again (one that saw its server go may never hear
//! from another), and the engine reaches PipeWire afresh while the share's sound is wanted.

use std::sync::{Arc, Mutex, PoisonError};

use conch_voice_audio::{STEREO_FRAME_LEN, StereoFrame};
use dmn_audio::{CaptureState, PipeWireContext, StreamCapture, StreamReader, StreamStatus};

use crate::share_capture::{
    BACKLOG_FRAMES, Capture, CaptureSpec, Captures, Graph, Health, Look, OpenFailed, OpenGraph,
    ReadTree, Registry, ShareSource, Taken,
};
use crate::share_eligibility::{Proc, StreamFacts, read_tree};

/// The share's sound from PipeWire: the graph on `context` (the helper's own connection, or
/// `None` to connect when first asked), the process tree from `/proc`, and this process's pid.
#[must_use]
pub fn pipewire_source(context: Option<Arc<PipeWireContext>>) -> ShareSource {
    let current = Arc::new(Mutex::new(context));
    let graph: OpenGraph = Arc::new(move |after_loss| {
        let mut current = current.lock().unwrap_or_else(PoisonError::into_inner);
        if after_loss {
            *current = None;
        }
        let context = match current.as_ref() {
            Some(context) => Arc::clone(context),
            None => {
                let context = Arc::new(PipeWireContext::connect().ok()?);
                *current = Some(Arc::clone(&context));
                context
            }
        };
        Some(Box::new(PipeWireGraph { context }) as Box<dyn Graph>)
    });
    let tree: ReadTree = Arc::new(|| {
        read_tree(
            &mut Proc::system(),
            Some(std::process::id()),
            crate::managed::parent_id(),
        )
    });
    ShareSource::Captures(Captures {
        graph,
        tree,
        own_pid: Some(std::process::id()),
    })
}

/// PipeWire's graph, on one connection.
struct PipeWireGraph {
    context: Arc<PipeWireContext>,
}

impl Graph for PipeWireGraph {
    fn look(&mut self, since: Option<u64>) -> Look {
        let table = self.context.nodes();
        if table.disconnected.is_some() {
            return Look::Lost;
        }
        if since == Some(table.generation) {
            return Look::Unchanged;
        }
        let streams = table
            .streams_with_metadata()
            .map(|(_, metadata)| StreamFacts {
                serial: metadata.serial,
                sec_pid: metadata.sec_pid,
                application_process_id: metadata.application_process_id,
                application_name: metadata.application_name,
            })
            .collect();
        Look::Changed(Registry {
            generation: table.generation,
            streams,
        })
    }

    fn open_capture(&mut self, spec: CaptureSpec) -> Result<Box<dyn Capture>, OpenFailed> {
        let CaptureSpec {
            serial,
            rate,
            channels,
            ring_ms,
        } = spec;
        let mut capture =
            StreamCapture::new(Arc::clone(&self.context), serial, rate, channels, ring_ms)
                .map_err(|_| OpenFailed)?;
        let status = capture.status();
        let reader = capture.start().map_err(|_| OpenFailed)?;
        Ok(Box::new(PipeWireCapture {
            capture,
            reader,
            status,
            want: (rate, channels),
        }))
    }
}

/// One capture of one application stream.
struct PipeWireCapture {
    capture: StreamCapture,
    reader: StreamReader,
    status: Arc<StreamStatus>,
    /// The rate and channels asked for: a stream that settles at others is not mixed.
    want: (u32, u32),
}

/// How a capture is doing, from what `dmn-audio` reports of it.
fn health(state: CaptureState, delivered: Option<(u32, u32)>, want: (u32, u32)) -> Health {
    match state {
        CaptureState::Streaming => match delivered {
            Some(format) if format == want => Health::Streaming,
            Some(_) => Health::WrongFormat,
            None => Health::Waiting,
        },
        CaptureState::Idle | CaptureState::Connecting => Health::Waiting,
        CaptureState::Disconnected | CaptureState::Error => Health::Ended,
    }
}

/// What one take does with a capture's ring: nothing unless it is streaming in the share's
/// format (what waits then is thrown away), the oldest frames beyond [`BACKLOG_FRAMES`]
/// dropped and counted, and then one whole frame, oldest first.
fn take_from(
    ring: &mut StreamReader,
    usable: bool,
    frame: &mut StereoFrame,
    scratch: &mut StereoFrame,
) -> Taken {
    let mut taken = Taken::default();
    if !usable {
        while ring.queued() > 0 && ring.pop(scratch) > 0 {}
        return taken;
    }
    while ring.queued() > BACKLOG_FRAMES * STEREO_FRAME_LEN {
        if ring.pop(scratch) == 0 {
            break;
        }
        taken.dropped += 1;
    }
    if ring.queued() >= STEREO_FRAME_LEN && ring.pop(frame) == STEREO_FRAME_LEN {
        taken.frame = true;
    }
    taken
}

impl Capture for PipeWireCapture {
    fn health(&self) -> Health {
        health(
            self.status.state(),
            self.status.delivered_format(),
            self.want,
        )
    }

    fn take(&mut self, frame: &mut StereoFrame) -> Taken {
        let usable = self.health() == Health::Streaming;
        // The frame doubles as the scratch of what is dropped: it is written whole after.
        let mut scratch = [0.0; STEREO_FRAME_LEN];
        take_from(&mut self.reader, usable, frame, &mut scratch)
    }

    fn stop(self: Box<Self>) {
        let mut this = *self;
        // The reason is not logged: it may name the stream.
        let _ = this.capture.stop();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_capture_is_mixed_only_while_it_streams_in_the_shares_format() {
        let want = (48_000, 2);
        let cases = [
            (
                CaptureState::Streaming,
                Some((48_000, 2)),
                Health::Streaming,
            ),
            (
                CaptureState::Streaming,
                Some((44_100, 2)),
                Health::WrongFormat,
            ),
            (
                CaptureState::Streaming,
                Some((48_000, 1)),
                Health::WrongFormat,
            ),
            (CaptureState::Streaming, None, Health::Waiting),
            (CaptureState::Connecting, Some((48_000, 2)), Health::Waiting),
            (CaptureState::Idle, None, Health::Waiting),
            (CaptureState::Disconnected, Some((48_000, 2)), Health::Ended),
            (CaptureState::Error, None, Health::Ended),
        ];
        for (state, delivered, wanted) in cases {
            assert_eq!(
                health(state, delivered, want),
                wanted,
                "{state:?} {delivered:?}"
            );
        }
    }
}
