//! Lines on their way to standard output and standard error, written by a thread of their
//! own so that nothing else ever waits for whoever reads them.
//!
//! A write to a pipe nobody reads, or to a terminal under Ctrl-S, does not return. If the
//! session loop made that write itself it would stop there: the release of the talk key,
//! `quit`, the end of input and every signal would wait behind a status line, with the gate
//! open. So nothing in this program writes a line where it is produced. It hands the line
//! to a [`LineQueue`]:
//!
//! - One thread owns the `Write` and is the only thing that can block on it.
//! - The queue between has a bound, and a line is put on it without waiting. When it is
//!   full the line is dropped and counted; a reader who stopped reading has lost lines, not
//!   stopped the client.
//! - When lines were dropped and there is room again, the first thing queued says how many.
//! - A queue may keep a few places beyond its bound for lines that must not be dropped
//!   ([`LineQueue::push_reserved`]). Such a line waits behind the lines already queued, and
//!   is dropped only when those places are taken too, so the queue still has a bound.
//! - A queue also keeps [`LAST_LINES`] places for the lines that end the output (the final
//!   state, the line that says the client left): [`LineQueue::push_last`]. They are taken by
//!   nothing else, so a reader that is slow, and has let the queue fill, still gets the
//!   last word once it reads on (#1144). They are still bounded by the flush limit at exit.
//! - A flush waits for what is queued to be written, but only up to a limit. A flush that
//!   times out is not an error: the process goes on, or exits.
//!
//! [`write_within`] is the same idea for a single last line (the reason the client stopped,
//! a panic): a thread makes the write and the caller gives up on it after a limit.

use std::io::Write;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::mpsc::{Receiver, SyncSender, TrySendError, sync_channel};
use std::sync::{Arc, Mutex, PoisonError};
use std::time::{Duration, Instant};

/// How many lines may wait for the writer before lines are dropped: far more than a client
/// produces in the time a reader that is reading takes to read them.
pub const QUEUE_LINES: usize = 1024;

/// How many places a queue keeps beyond its bound and its reserve, for [`LineQueue::push_last`].
pub const LAST_LINES: usize = 2;

/// How often a flush that found the queue full looks again.
const FLUSH_RETRY: Duration = Duration::from_millis(5);

enum Item {
    Line(String),
    /// Flush the writer, then call this: everything queued before it has been written.
    Flushed(Box<dyn FnOnce() + Send>),
}

/// Says that lines were dropped: given how many, the line to write in their place.
pub type Marker = Box<dyn Fn(u64) -> String + Send + Sync>;

/// A bounded queue of lines in front of a writer thread. Nothing here waits for the
/// writer except [`LineQueue::flush`] and [`LineQueue::flush_blocking`], each up to its
/// limit.
pub struct LineQueue {
    /// Room for `capacity` and `reserve` together. Which line may use which places is
    /// decided by `waiting`, before anything is put on it.
    queue: SyncSender<Item>,
    /// Items put on the queue and not yet taken by the writer: raised before an item is
    /// put there and lowered once the writer has taken it, so never fewer than it holds.
    waiting: Arc<AtomicUsize>,
    /// How many items may wait before a line [`LineQueue::push`] queues is dropped.
    capacity: usize,
    /// How many more may wait for lines [`LineQueue::push_reserved`] queues.
    reserve: usize,
    /// Lines dropped since the last marker was queued. Held only while a line is put on
    /// the queue, which does not wait, so it keeps lines from several threads in order
    /// without making any of them wait for output.
    dropped: Mutex<u64>,
    /// `None` for a queue that drops without saying so (`spawn_unmarked`).
    marker: Option<Marker>,
}

impl LineQueue {
    /// Starts a thread that owns `out` and writes the lines queued for it, each followed by
    /// a line break and flushed. At most `capacity` lines wait; `marker` words the line
    /// that stands in for dropped ones.
    #[must_use]
    pub fn spawn(out: Box<dyn Write + Send>, capacity: usize, marker: Marker) -> Self {
        Self::start(out, capacity, 0, Some(marker))
    }

    /// As [`LineQueue::spawn`], for lines that say for themselves when some are missing: a
    /// line that meets a full queue is dropped and counted, and nothing is written in its
    /// place. The managed helper's events are such lines: each has a number one greater
    /// than the last, and a gap in the numbers is how its owner learns of a drop
    /// (`docs/design/tui-voice.md` §3). `reserve` more may wait, for lines queued with
    /// [`LineQueue::push_reserved`].
    #[must_use]
    pub fn spawn_unmarked(out: Box<dyn Write + Send>, capacity: usize, reserve: usize) -> Self {
        Self::start(out, capacity, reserve, None)
    }

    fn start(
        out: Box<dyn Write + Send>,
        capacity: usize,
        reserve: usize,
        marker: Option<Marker>,
    ) -> Self {
        let (queue, items) =
            sync_channel(capacity.saturating_add(reserve).saturating_add(LAST_LINES));
        let waiting = Arc::new(AtomicUsize::new(0));
        let taken = Arc::clone(&waiting);
        std::thread::spawn(move || write_all(out, &items, &taken));
        Self {
            queue,
            waiting,
            capacity,
            reserve,
            dropped: Mutex::new(0),
            marker,
        }
    }

    /// Queues a line. It never waits: with the queue full the line is dropped and counted,
    /// and the count is written, as the marker, before the next line there is room for.
    pub fn push(&self, line: String) {
        self.put(line, self.capacity);
    }

    /// Queues a line that may also take a reserved place: with the queue full it is queued
    /// all the same, behind the lines waiting, and it is dropped only when the reserve is
    /// taken too. It never waits. The caller is the one who knows why the reserve is enough.
    pub fn push_reserved(&self, line: String) {
        self.put(line, self.capacity.saturating_add(self.reserve));
    }

    /// Queues a line that ends the output: it has a place that no other line may take
    /// ([`LAST_LINES`]), so it is queued behind whatever waits, however full the queue is,
    /// and dropped only when more than [`LAST_LINES`] such lines are queued. It never waits,
    /// and no marker for dropped lines is written first: it is the last word, and the count
    /// stands.
    pub fn push_last(&self, line: String) {
        let mut dropped = self.dropped.lock().unwrap_or_else(PoisonError::into_inner);
        let limit = self
            .capacity
            .saturating_add(self.reserve)
            .saturating_add(LAST_LINES);
        if let Err(TrySendError::Full(_)) = self.offer(Item::Line(line), limit) {
            *dropped = dropped.saturating_add(1);
        }
    }

    /// Queues a line if fewer than `limit` items wait, and otherwise drops and counts it.
    fn put(&self, line: String, limit: usize) {
        let mut dropped = self.dropped.lock().unwrap_or_else(PoisonError::into_inner);
        if self.marker.is_none() {
            if let Err(TrySendError::Full(_)) = self.offer(Item::Line(line), limit) {
                *dropped = dropped.saturating_add(1);
            }
            return;
        }
        if self.owe_nothing(&mut dropped, limit)
            && let Err(TrySendError::Full(_)) = self.offer(Item::Line(line), limit)
        {
            *dropped = 1;
        } else if *dropped > 0 {
            *dropped = dropped.saturating_add(1);
        }
        // A writer that is gone (its thread ended) takes nothing; there is nobody to tell.
    }

    /// Puts `item` on the queue if fewer than `limit` items wait for the writer, and hands
    /// it back as `Full` if not. The place is taken before the item is put there, so two
    /// threads never both take the last one.
    fn offer(&self, item: Item, limit: usize) -> Result<(), TrySendError<Item>> {
        let took = self
            .waiting
            .fetch_update(Ordering::SeqCst, Ordering::SeqCst, |waiting| {
                (waiting < limit).then_some(waiting + 1)
            });
        if took.is_err() {
            return Err(TrySendError::Full(item));
        }
        let sent = self.queue.try_send(item);
        if sent.is_err() {
            self.waiting.fetch_sub(1, Ordering::SeqCst);
        }
        sent
    }

    /// Queues the marker for lines dropped so far, if there are any, if fewer than `limit`
    /// items wait. True if nothing is owed any more; false if the queue is still full, and
    /// then the count stands.
    fn owe_nothing(&self, dropped: &mut u64, limit: usize) -> bool {
        let Some(marker) = &self.marker else {
            // Nothing is ever owed: the lines say for themselves that some are missing.
            return true;
        };
        if *dropped == 0 {
            return true;
        }
        match self.offer(Item::Line(marker(*dropped)), limit) {
            Err(TrySendError::Full(_)) => false,
            Ok(()) | Err(TrySendError::Disconnected(_)) => {
                *dropped = 0;
                true
            }
        }
    }

    /// Lines dropped and not yet said to be; for a queue that never says so, all the lines
    /// it has dropped.
    #[must_use]
    pub fn dropped(&self) -> u64 {
        *self.dropped.lock().unwrap_or_else(PoisonError::into_inner)
    }

    /// Queues the end-of-flush mark, without waiting and without the reserve. The mark is
    /// handed back if the queue is full.
    fn try_mark(&self, mark: Item) -> Result<(), Item> {
        {
            let mut dropped = self.dropped.lock().unwrap_or_else(PoisonError::into_inner);
            if !self.owe_nothing(&mut dropped, self.capacity) {
                return Err(mark);
            }
        }
        match self.offer(mark, self.capacity) {
            Ok(()) => Ok(()),
            Err(TrySendError::Full(mark)) => Err(mark),
            // No writer: nothing is waiting to be written.
            Err(TrySendError::Disconnected(Item::Flushed(done))) => {
                done();
                Ok(())
            }
            Err(TrySendError::Disconnected(Item::Line(_))) => Ok(()),
        }
    }

    /// Waits until everything queued so far has been written, or `limit` has passed. True
    /// if it was written. For code that is not in the async runtime: it blocks the thread.
    pub fn flush_blocking(&self, limit: Duration) -> bool {
        let deadline = Instant::now() + limit;
        let (done, written) = std::sync::mpsc::channel();
        let mut mark = Item::Flushed(Box::new(move || {
            let _ = done.send(());
        }));
        loop {
            match self.try_mark(mark) {
                Ok(()) => break,
                Err(again) => mark = again,
            }
            if Instant::now() >= deadline {
                return false;
            }
            std::thread::sleep(FLUSH_RETRY);
        }
        written
            .recv_timeout(deadline.saturating_duration_since(Instant::now()))
            .is_ok()
    }

    /// Waits until everything queued so far has been written, or `limit` has passed. True
    /// if it was written. It waits without blocking the runtime.
    pub async fn flush(&self, limit: Duration) -> bool {
        let deadline = tokio::time::Instant::now() + limit;
        let (done, written) = tokio::sync::oneshot::channel();
        let mut mark = Item::Flushed(Box::new(move || {
            let _ = done.send(());
        }));
        loop {
            match self.try_mark(mark) {
                Ok(()) => break,
                Err(again) => mark = again,
            }
            if tokio::time::Instant::now() >= deadline {
                return false;
            }
            tokio::time::sleep(FLUSH_RETRY).await;
        }
        matches!(tokio::time::timeout_at(deadline, written).await, Ok(Ok(())))
    }
}

/// The writer thread: the one place that can wait for the reader. It ends when the queue's
/// last sender is gone. A write that fails (the reader went away) loses that line and
/// nothing else: the queue is still emptied, so nobody is ever refused for lack of room.
fn write_all(mut out: Box<dyn Write + Send>, items: &Receiver<Item>, waiting: &AtomicUsize) {
    for item in items {
        // Taken off the queue: its place is free while it is written, as with a channel.
        waiting.fetch_sub(1, Ordering::SeqCst);
        match item {
            Item::Line(mut line) => {
                // The line and its end in one write, so that a reader never sees half.
                line.push('\n');
                let _ = out.write_all(line.as_bytes());
                let _ = out.flush();
            }
            Item::Flushed(done) => {
                let _ = out.flush();
                done();
            }
        }
    }
}

/// Writes one line to `out` on a thread of its own and waits for it up to `limit`. True if
/// the line was written in time. The caller goes on either way: this is for the last line
/// before the process exits, which must not keep the process alive.
pub fn write_within(
    mut out: impl Write + Send + 'static,
    mut line: String,
    limit: Duration,
) -> bool {
    let (done, written) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        line.push('\n');
        let wrote = out
            .write_all(line.as_bytes())
            .and_then(|()| out.flush())
            .is_ok();
        let _ = done.send(wrote);
    });
    written.recv_timeout(limit).unwrap_or(false)
}

/// How long the last thing a process has to say is given to be written before the process
/// exits without it.
pub const LAST_WORDS_LIMIT: Duration = Duration::from_millis(500);

/// Writes one last line to standard error, giving up after [`LAST_WORDS_LIMIT`]. True if it
/// was written. For `main` and the panic hook, just before the process exits.
pub fn last_words(line: String) -> bool {
    write_within(std::io::stderr(), line, LAST_WORDS_LIMIT)
}

/// How long the ordinary exit is given before the process is ended another way.
pub const EXIT_GRACE: Duration = Duration::from_millis(500);

/// Ends the process with `code`, whatever any other thread is doing.
///
/// The ordinary exit is not enough by itself. It flushes C's standard output under that
/// stream's lock, and native code in the SDK writes there: libwebrtc prints a line during a
/// join on a machine with an NVIDIA GPU. If nobody is reading standard output, the thread
/// that prints is stuck in that write with the lock held, and the ordinary exit then waits
/// for the lock for ever (seen with the real binary and a full pipe: every thread of this
/// program had finished, and the process stayed). So a second thread waits [`EXIT_GRACE`]
/// and, if the process is still there, ends it without flushing anything.
pub fn exit(code: i32) -> ! {
    exit_or_replace(code, EXIT_GRACE, |code| std::process::exit(code))
}

/// [`exit`] with its parts given: `ordinary` is tried, and if the process is still there
/// after `grace` it is replaced by a shell that does nothing but exit with `code`. Replacing
/// the process ends every thread where it stands and runs no exit handler; it is the one
/// way to do that from safe code. Without a shell the process is aborted: it ends, though
/// not with its code.
pub fn exit_or_replace(code: i32, grace: Duration, ordinary: impl FnOnce(i32)) -> ! {
    use std::os::unix::process::CommandExt;
    use std::process::{Command, Stdio};

    std::thread::spawn(move || {
        std::thread::sleep(grace);
        // The shell gets none of this process's environment and none of its streams.
        let _no_shell = Command::new("/bin/sh")
            .args(["-c", &format!("exit {code}")])
            .env_clear()
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .exec();
        std::process::abort();
    });
    ordinary(code);
    // Only an `ordinary` that returned without ending the process gets here.
    loop {
        std::thread::park();
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::sync::{Arc, Condvar};

    use super::*;

    /// A writer whose reader can stop reading: while stalled, a write does not return, as
    /// a write to a full pipe does not. The other modules' tests use it too.
    #[derive(Clone, Default)]
    pub(crate) struct Stallable {
        written: Arc<Mutex<Vec<u8>>>,
        stalled: Arc<(Mutex<bool>, Condvar)>,
        waiting: Arc<AtomicBool>,
    }

    impl Stallable {
        pub(crate) fn stalled() -> Self {
            let writer = Self::default();
            writer.stall(true);
            writer
        }

        pub(crate) fn stall(&self, stalled: bool) {
            *self.stalled.0.lock().unwrap() = stalled;
            self.stalled.1.notify_all();
        }

        pub(crate) fn text(&self) -> String {
            String::from_utf8(self.written.lock().unwrap().clone()).unwrap()
        }

        /// Waits until a write is stuck behind the stall.
        pub(crate) fn until_waiting(&self) {
            let deadline = Instant::now() + Duration::from_secs(10);
            while !self.waiting.load(Ordering::SeqCst) {
                assert!(Instant::now() < deadline, "no write is waiting");
                std::thread::sleep(Duration::from_millis(1));
            }
        }

        /// Waits until exactly this has been written.
        pub(crate) fn until_text(&self, expected: &str) {
            let deadline = Instant::now() + Duration::from_secs(10);
            while self.text() != expected {
                assert!(
                    Instant::now() < deadline,
                    "written {:?}, expected {expected:?}",
                    self.text()
                );
                std::thread::sleep(Duration::from_millis(1));
            }
        }

        /// Waits until this many lines have been written.
        pub(crate) fn until_lines(&self, lines: usize) {
            let deadline = Instant::now() + Duration::from_secs(10);
            while self.text().lines().count() < lines {
                assert!(Instant::now() < deadline, "fewer than {lines} lines");
                std::thread::sleep(Duration::from_millis(1));
            }
        }
    }

    impl Write for Stallable {
        fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
            let mut stalled = self.stalled.0.lock().unwrap();
            while *stalled {
                self.waiting.store(true, Ordering::SeqCst);
                stalled = self.stalled.1.wait(stalled).unwrap();
            }
            self.waiting.store(false, Ordering::SeqCst);
            self.written.lock().unwrap().extend_from_slice(bytes);
            Ok(bytes.len())
        }

        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }

    fn queue(writer: &Stallable, capacity: usize) -> LineQueue {
        LineQueue::spawn(
            Box::new(writer.clone()),
            capacity,
            Box::new(|lines| format!("({lines} dropped)")),
        )
    }

    #[test]
    fn lines_are_written_in_the_order_they_were_queued() {
        let writer = Stallable::default();
        let lines = queue(&writer, 8);
        for n in 0..100 {
            lines.push(format!("line {n}"));
            // The writer keeps up, so nothing is dropped however many there are.
            assert!(lines.flush_blocking(Duration::from_secs(10)));
        }
        let expected: String = (0..100).map(|n| format!("line {n}\n")).collect();
        assert_eq!(writer.text(), expected);
        assert_eq!(lines.dropped(), 0);
    }

    #[test]
    fn a_full_queue_drops_and_counts_and_never_waits_for_the_reader() {
        // The reader does not read for the whole of this test.
        let writer = Stallable::stalled();
        let lines = queue(&writer, 4);
        lines.push("in the writer's hands".into());
        writer.until_waiting();

        let began = Instant::now();
        for n in 0..1000 {
            lines.push(format!("line {n}"));
        }
        let took = began.elapsed();
        assert!(
            took < Duration::from_millis(500),
            "queueing waited for the reader: {took:?}"
        );
        // Four fitted in the queue behind the one being written; the rest were dropped.
        assert_eq!(lines.dropped(), 996);
        assert_eq!(writer.text(), "", "and nothing was written");
    }

    #[test]
    fn when_the_reader_reads_again_the_first_thing_written_says_how_many_were_dropped() {
        let writer = Stallable::stalled();
        let lines = queue(&writer, 2);
        lines.push("a".into());
        writer.until_waiting();
        for line in ["b", "c", "lost", "lost", "lost"] {
            lines.push(line.into());
        }
        assert_eq!(lines.dropped(), 3);

        writer.stall(false);
        writer.until_text("a\nb\nc\n");
        // Nothing says so until there is something to say it before.
        lines.push("d".into());
        writer.until_text("a\nb\nc\n(3 dropped)\nd\n");
        assert_eq!(lines.dropped(), 0);

        // A flush alone also brings the count out.
        writer.stall(true);
        lines.push("e".into());
        writer.until_waiting();
        for line in ["f", "g", "lost"] {
            lines.push(line.into());
        }
        writer.stall(false);
        assert!(lines.flush_blocking(Duration::from_secs(10)));
        assert_eq!(
            writer.text(),
            "a\nb\nc\n(3 dropped)\nd\ne\nf\ng\n(1 dropped)\n"
        );
    }

    #[test]
    fn an_unmarked_queue_drops_what_does_not_fit_and_writes_nothing_in_its_place() {
        let writer = Stallable::stalled();
        let lines = LineQueue::spawn_unmarked(Box::new(writer.clone()), 2, 0);
        lines.push("a".into());
        writer.until_waiting();
        for line in ["b", "c", "lost", "lost", "lost"] {
            lines.push(line.into());
        }
        assert_eq!(lines.dropped(), 3);
        writer.stall(false);
        writer.until_text("a\nb\nc\n");
        lines.push("d".into());
        assert!(lines.flush_blocking(Duration::from_secs(10)));
        assert_eq!(writer.text(), "a\nb\nc\nd\n");
        assert_eq!(
            lines.dropped(),
            3,
            "the count stands; nothing was written for it"
        );
    }

    #[test]
    fn a_reserved_line_is_queued_behind_a_full_queue_and_the_reserve_has_a_bound_too() {
        let writer = Stallable::stalled();
        let lines = LineQueue::spawn_unmarked(Box::new(writer.clone()), 2, 2);
        lines.push("a".into());
        writer.until_waiting();
        for line in ["b", "c", "lost"] {
            lines.push(line.into());
        }
        assert_eq!(lines.dropped(), 1);
        // The queue is full for ordinary lines; a reserved one still has a place.
        lines.push_reserved("kept 1".into());
        lines.push("lost".into());
        lines.push_reserved("kept 2".into());
        // The reserve is taken too: a reserved line is dropped like any other, so the
        // queue never holds more than its bound and its reserve.
        lines.push_reserved("lost".into());
        assert_eq!(lines.dropped(), 3);

        writer.stall(false);
        writer.until_text("a\nb\nc\nkept 1\nkept 2\n");
        // Once the writer has taken them, every place is free again.
        lines.push("d".into());
        lines.push_reserved("kept 3".into());
        assert!(lines.flush_blocking(Duration::from_secs(10)));
        assert_eq!(writer.text(), "a\nb\nc\nkept 1\nkept 2\nd\nkept 3\n");
        assert_eq!(lines.dropped(), 3);
    }

    #[test]
    fn a_last_line_is_queued_behind_a_full_queue_and_its_places_are_its_own() {
        let writer = Stallable::stalled();
        let lines = LineQueue::spawn_unmarked(Box::new(writer.clone()), 2, 1);
        lines.push("a".into());
        writer.until_waiting();
        for line in ["b", "c", "lost"] {
            lines.push(line.into());
        }
        lines.push_reserved("answer".into());
        // The queue and its reserve are full; a last line still has a place.
        lines.push_reserved("lost".into());
        lines.push_last("last 1".into());
        lines.push_last("last 2".into());
        lines.push_last("lost".into());
        assert_eq!(lines.dropped(), 3);

        writer.stall(false);
        assert!(lines.flush_blocking(Duration::from_secs(10)));
        assert_eq!(writer.text(), "a\nb\nc\nanswer\nlast 1\nlast 2\n");
    }

    #[test]
    fn a_line_that_meets_a_full_queue_after_the_marker_was_owed_is_counted_too() {
        let writer = Stallable::stalled();
        let lines = queue(&writer, 1);
        lines.push("a".into());
        writer.until_waiting();
        lines.push("b".into());
        lines.push("lost 1".into());
        lines.push("lost 2".into());
        assert_eq!(lines.dropped(), 2);
        writer.stall(false);
        writer.until_text("a\nb\n");
        // Room for the marker, and then none for the line behind it.
        writer.stall(true);
        lines.push("c".into());
        writer.until_waiting();
        lines.push("lost 3".into());
        lines.push("lost 4".into());
        writer.stall(false);
        assert!(lines.flush_blocking(Duration::from_secs(10)));
        let text = writer.text();
        assert!(text.starts_with("a\nb\n(2 dropped)\n"), "{text}");
        assert!(text.ends_with(" dropped)\n"), "{text}");
        assert!(!text.contains("lost"), "{text}");
    }

    #[test]
    fn a_flush_returns_within_its_limit_when_the_reader_is_not_reading() {
        let writer = Stallable::stalled();
        let lines = queue(&writer, 4);
        lines.push("never read".into());
        writer.until_waiting();

        let began = Instant::now();
        assert!(!lines.flush_blocking(Duration::from_millis(100)));
        let took = began.elapsed();
        assert!(
            took >= Duration::from_millis(100) && took < Duration::from_secs(2),
            "{took:?}"
        );

        // And with the queue itself full, so that not even the flush's mark fits.
        for n in 0..10 {
            lines.push(format!("line {n}"));
        }
        let began = Instant::now();
        assert!(!lines.flush_blocking(Duration::from_millis(100)));
        assert!(began.elapsed() < Duration::from_secs(2));
    }

    #[tokio::test]
    async fn an_async_flush_returns_within_its_limit_and_does_not_block_the_runtime() {
        let writer = Stallable::stalled();
        let lines = queue(&writer, 4);
        lines.push("never read".into());
        writer.until_waiting();
        for n in 0..10 {
            lines.push(format!("line {n}"));
        }

        // This runtime has one thread. If the flush blocked it, the timer beside it could
        // not fire.
        let began = Instant::now();
        let (flushed, ()) = tokio::join!(
            lines.flush(Duration::from_millis(100)),
            tokio::time::sleep(Duration::from_millis(20))
        );
        assert!(!flushed);
        assert!(began.elapsed() < Duration::from_secs(2));

        writer.stall(false);
        assert!(lines.flush(Duration::from_secs(10)).await);
        assert!(writer.text().starts_with("never read\nline 0\n"));
    }

    #[test]
    fn a_reader_that_went_away_loses_lines_and_stops_nobody() {
        struct Gone;
        impl Write for Gone {
            fn write(&mut self, _: &[u8]) -> std::io::Result<usize> {
                Err(std::io::ErrorKind::BrokenPipe.into())
            }
            fn flush(&mut self) -> std::io::Result<()> {
                Err(std::io::ErrorKind::BrokenPipe.into())
            }
        }
        let lines = LineQueue::spawn(Box::new(Gone), 2, Box::new(|n| format!("{n}")));
        for n in 0..100 {
            lines.push(format!("line {n}"));
            assert!(lines.flush_blocking(Duration::from_secs(10)));
        }
        assert_eq!(lines.dropped(), 0, "the queue was emptied all the same");
    }

    #[test]
    fn a_last_line_is_written_within_a_limit_or_given_up_on() {
        let writer = Stallable::default();
        assert!(write_within(
            writer.clone(),
            "the reason".into(),
            Duration::from_secs(10)
        ));
        assert_eq!(writer.text(), "the reason\n");

        let stalled = Stallable::stalled();
        let began = Instant::now();
        assert!(!write_within(
            stalled.clone(),
            "never read".into(),
            Duration::from_millis(100)
        ));
        let took = began.elapsed();
        assert!(
            took >= Duration::from_millis(100) && took < Duration::from_secs(2),
            "{took:?}"
        );
        // The thread that was left behind is let go, so the test process can end cleanly.
        stalled.stall(false);
    }

    /// What turns the two tests below into the process they start.
    const EXITING_CHILD: &str = "CONCH_VOICE_EXITING_CHILD";

    /// Not a test of its own: run as a process by the test below, it exits as it is told
    /// to. Without the environment variable it does nothing.
    #[test]
    fn child_process_exits_as_it_is_told() {
        match std::env::var(EXITING_CHILD).as_deref() {
            // An ordinary exit that never returns and never ends the process, as libc's
            // does not when another thread is stuck with a stream's lock.
            Ok("hangs") => exit_or_replace(7, Duration::from_millis(100), |_| {
                loop {
                    std::thread::park();
                }
            }),
            Ok("works") => {
                exit_or_replace(3, Duration::from_secs(600), |code| std::process::exit(code))
            }
            _ => {}
        }
    }

    /// Runs this test binary again as the child above. Returns its exit code and how long
    /// it took to end.
    fn exiting_child(how: &str) -> (Option<i32>, Duration) {
        use std::process::{Command, Stdio};
        let began = Instant::now();
        let mut child = Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "lines::tests::child_process_exits_as_it_is_told",
                "--nocapture",
                "--test-threads=1",
            ])
            .env(EXITING_CHILD, how)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .unwrap();
        loop {
            if let Some(status) = child.try_wait().unwrap() {
                return (status.code(), began.elapsed());
            }
            if began.elapsed() > Duration::from_secs(20) {
                child.kill().unwrap();
                child.wait().unwrap();
                panic!("the child did not end");
            }
            std::thread::sleep(Duration::from_millis(10));
        }
    }

    #[test]
    fn a_process_whose_ordinary_exit_hangs_is_ended_with_its_code_all_the_same() {
        let (code, took) = exiting_child("hangs");
        assert_eq!(code, Some(7), "its own code, not a signal's");
        assert!(took >= Duration::from_millis(100), "{took:?}");
    }

    #[test]
    fn a_process_whose_ordinary_exit_works_is_ended_by_it() {
        let (code, took) = exiting_child("works");
        assert_eq!(code, Some(3));
        assert!(took < Duration::from_secs(10), "{took:?}");
    }
}
