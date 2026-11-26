//! Which applications' playback a screen share's sound may capture: "everything but the
//! call" (`docs/design/linux-share-audio.md` §5.2).
//!
//! Input: the application output streams the audio system lists, as plain values
//! ([`StreamFacts`]: an `object.serial`, the owning client's `pipewire.sec.pid`, the
//! advisory `application.process.id` and `application.name`, and the display-only
//! `media.name`), and the process table
//! (`/proc/<pid>/stat`, read by [`Proc`], or a fake in the tests). Output: the serials that
//! may be captured, or [`NoTree`] when what to leave out cannot be worked out. Owns: nothing
//! but the rule. It opens nothing and connects to nothing; the captures are
//! `share_capture.rs`'s.
//!
//! The rule: a stream is captured unless
//!
//! 1. its owning client's `pipewire.sec.pid` is this helper's own pid (the helper's playback
//!    of the call: set by PipeWire's protocol, not by the application);
//! 2. its `pipewire.sec.pid` or its `application.process.id` is in the Conch process tree:
//!    the helper, its parent (the desktop's main process) and every descendant of that
//!    parent, read from `/proc` when the rule is evaluated (a `pipewire-pulse` client is the
//!    proxy's pid, so a Chromium stream through it is caught by its application pid);
//! 3. its `application.name` is the one the owner's command named.
//!
//! And two kinds of stream are **deferred**, neither captured nor counted, until the next
//! evaluation: one whose owning client's pid is not known yet (the client's properties can
//! reach the registry after the node's, and the helper's own playback must never be caught
//! in that moment), and one with no `object.serial` to capture it by.
//!
//! **Fail closed.** No tree, no capture: `/proc` that cannot be read, a parent that is pid 1
//! or 0 (the owner is gone), an own pid that is not known, or a process table in which the
//! helper is not its parent's child gives [`NoTree`], and [`eligible`] then names no stream
//! at all. It never answers "I could not tell what to leave out" by capturing everything.
//!
//! Nothing here logs, and [`StreamFacts`]'s `Debug` leaves out both display labels.

use std::collections::{BTreeMap, BTreeSet};
use std::fmt;
use std::path::PathBuf;

/// One application output stream (`Stream/Output/Audio`), as the rule sees it.
#[derive(Clone, PartialEq, Eq, Default)]
pub struct StreamFacts {
    /// `object.serial`: what a capture targets. `None` until the audio system says it.
    pub serial: Option<u64>,
    /// `pipewire.sec.pid` of the client that owns the node: trusted, set by the protocol.
    /// `None` until the client's properties are known.
    pub sec_pid: Option<u32>,
    /// `application.process.id`: advisory, whatever the application or its proxy wrote.
    pub application_process_id: Option<u32>,
    /// `application.name`: advisory.
    pub application_name: Option<String>,
    /// `media.name`: display metadata, never used to decide eligibility.
    pub media_name: Option<String>,
}

/// Shows which facts are known, and no application's name.
impl fmt::Debug for StreamFacts {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("StreamFacts")
            .field("serial", &self.serial)
            .field("sec_pid", &self.sec_pid)
            .field("application_process_id", &self.application_process_id)
            .field(
                "application_name",
                &self.application_name.as_ref().map(|_| ".."),
            )
            .finish()
    }
}

/// Why the Conch process tree could not be worked out. Each is a reason to capture nothing.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum NoTree {
    /// `/proc` could not be listed.
    Unreadable,
    /// The parent is pid 0 or 1: the owner is gone, and its tree with it.
    ParentGone,
    /// This process's own pid is not known.
    OwnPidUnknown,
    /// The process table does not show this process as its parent's child: it was not
    /// read whole, or the parent has changed.
    NotParentsChild,
}

impl NoTree {
    /// The reason, in words of this program's own, for the log.
    #[must_use]
    pub fn reason(self) -> &'static str {
        match self {
            NoTree::Unreadable => "the process table cannot be read",
            NoTree::ParentGone => "the owner process is gone",
            NoTree::OwnPidUnknown => "this process's own id is not known",
            NoTree::NotParentsChild => {
                "the process table does not show this process under its owner"
            }
        }
    }
}

/// The parent pid in one `/proc/<pid>/stat` line: the second field after the **last** `)`,
/// so that a process name holding spaces or parentheses (`comm` is whatever the process
/// chose, up to 15 bytes) cannot shift it. `None` for a line that is not a stat line.
#[must_use]
pub fn stat_parent(line: &str) -> Option<u32> {
    stat_parent_bytes(line.as_bytes())
}

/// [`stat_parent`] on the bytes as read. `comm` is never decoded: whatever bytes a process
/// gave itself as a name (they need not be UTF-8) cannot fail the read or shift the field.
fn stat_parent_bytes(line: &[u8]) -> Option<u32> {
    let close = line.iter().rposition(|byte| *byte == b')')?;
    let rest = line.get(close + 1..)?;
    let mut fields = rest
        .split(u8::is_ascii_whitespace)
        .filter(|field| !field.is_empty());
    let _state = fields.next()?;
    number(fields.next()?)
}

/// The pid at the head of a `/proc/<pid>/stat` line, which is the process's own: the digits
/// before the first space, read before `comm` begins.
fn stat_pid(line: &[u8]) -> Option<u32> {
    let space = line.iter().position(|byte| *byte == b' ')?;
    number(line.get(..space)?)
}

/// A decimal number of ASCII digits, and nothing else.
fn number(digits: &[u8]) -> Option<u32> {
    std::str::from_utf8(digits).ok()?.parse().ok()
}

/// The helper, its parent, and every descendant of that parent.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ProcessTree {
    members: BTreeSet<u32>,
}

impl ProcessTree {
    /// Whether `pid` is one of Conch's own processes.
    #[must_use]
    pub fn contains(&self, pid: u32) -> bool {
        self.members.contains(&pid)
    }

    /// How many processes the tree holds.
    #[must_use]
    pub fn len(&self) -> usize {
        self.members.len()
    }

    /// Whether the tree is empty (it never is when it was built).
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.members.is_empty()
    }

    /// The tree of `own`'s parent `parent`, from every `(pid, parent pid)` of the process
    /// table. Each process is visited once, so a table that loops cannot keep this going.
    ///
    /// # Errors
    ///
    /// [`NoTree`] for an own pid that is not known, a parent of 0 or 1, or a table that does
    /// not show `own` as `parent`'s child.
    pub fn build(own: Option<u32>, parent: u32, table: &[(u32, u32)]) -> Result<Self, NoTree> {
        let own = own.filter(|pid| *pid > 1).ok_or(NoTree::OwnPidUnknown)?;
        if parent <= 1 {
            return Err(NoTree::ParentGone);
        }
        if !table
            .iter()
            .any(|&(pid, ppid)| pid == own && ppid == parent)
        {
            return Err(NoTree::NotParentsChild);
        }
        let mut children: BTreeMap<u32, Vec<u32>> = BTreeMap::new();
        for &(pid, ppid) in table {
            children.entry(ppid).or_default().push(pid);
        }
        let mut members = BTreeSet::from([own, parent]);
        let mut next = vec![parent];
        while let Some(pid) = next.pop() {
            for &child in children.get(&pid).into_iter().flatten() {
                if members.insert(child) {
                    next.push(child);
                }
            }
        }
        Ok(Self { members })
    }
}

/// The process table, as the tree is built from it: `(pid, parent pid)` of every process.
pub trait ProcessTable: Send {
    /// Every process that could be read.
    ///
    /// # Errors
    ///
    /// [`NoTree::Unreadable`] when the table cannot be listed at all.
    fn read(&mut self) -> Result<Vec<(u32, u32)>, NoTree>;
}

/// The real process table: `/proc`, read with the standard library and nothing else.
#[derive(Debug, Clone)]
pub struct Proc {
    root: PathBuf,
    /// This process's user, as `/proc/self` says: whose entries a refusal can never be.
    uid: Option<u32>,
}

impl Proc {
    /// The system's `/proc`.
    #[must_use]
    pub fn system() -> Self {
        Self::at(PathBuf::from("/proc"))
    }

    /// A process table laid out as `/proc` is, under `root`: for the tests.
    #[must_use]
    pub fn at(root: PathBuf) -> Self {
        Self {
            root,
            uid: owner(std::path::Path::new("/proc/self")),
        }
    }
}

impl ProcessTable for Proc {
    fn read(&mut self) -> Result<Vec<(u32, u32)>, NoTree> {
        let entries = std::fs::read_dir(&self.root).map_err(|_| NoTree::Unreadable)?;
        let mut table = Vec::new();
        for entry in entries {
            let Ok(entry) = entry else {
                return Err(NoTree::Unreadable);
            };
            let name = entry.file_name();
            let Some(pid) = name.to_str().and_then(|name| name.parse::<u32>().ok()) else {
                continue;
            };
            // A process that ended between the listing and the read is not in the table:
            // its entry is gone (not found), or its stat answers "no such process". Any
            // other error is a table that could not be read whole, so nothing is captured
            // (skipping it would drop that process's descendants from the tree).
            // Bytes, not text: a process's name need not be UTF-8.
            let line = match std::fs::read(entry.path().join("stat")) {
                Ok(line) => line,
                Err(error) if ended(&error) => continue,
                Err(error) if hidden(&error, owner(&entry.path()), self.uid) => continue,
                Err(_) => return Err(NoTree::Unreadable),
            };
            match (stat_pid(&line), stat_parent_bytes(&line)) {
                (Some(said), Some(ppid)) if said == pid => table.push((pid, ppid)),
                _ => continue,
            }
        }
        Ok(table)
    }
}

/// `ESRCH`: the process is gone. Read from a stat whose process ended while it was read.
const NO_SUCH_PROCESS: i32 = 3;

/// Whether a read of `/proc/<pid>/stat` failed because the process has ended.
fn ended(error: &std::io::Error) -> bool {
    error.kind() == std::io::ErrorKind::NotFound || error.raw_os_error() == Some(NO_SUCH_PROCESS)
}

/// Whether a read of `/proc/<pid>/stat` was refused because that process is another
/// user's, as `/proc` mounted with `hidepid=1` refuses it. Conch's own processes all run as
/// this user (the desktop's package has no setuid sandbox), so such a process is not in
/// Conch's tree and is left out. A refusal of one of this user's own processes, or one whose
/// owner is not known, is not this, and the table is not read.
fn hidden(error: &std::io::Error, entry_owner: Option<u32>, own: Option<u32>) -> bool {
    error.kind() == std::io::ErrorKind::PermissionDenied
        && matches!((entry_owner, own), (Some(entry), Some(own)) if entry != own)
}

/// The user that owns this path, where the system says so.
#[cfg(unix)]
fn owner(path: &std::path::Path) -> Option<u32> {
    use std::os::unix::fs::MetadataExt;
    std::fs::metadata(path).ok().map(|meta| meta.uid())
}

/// The user that owns this path, where the system says so.
#[cfg(not(unix))]
fn owner(_path: &std::path::Path) -> Option<u32> {
    None
}

/// Reads the Conch process tree for this helper: its own pid, its parent now, and the table.
///
/// # Errors
///
/// As [`ProcessTree::build`], and [`NoTree::Unreadable`] when the table cannot be read.
pub fn read_tree(
    table: &mut dyn ProcessTable,
    own: Option<u32>,
    parent: u32,
) -> Result<ProcessTree, NoTree> {
    // The cheap refusals first: nothing is read for an owner that is gone.
    let own_known = own.filter(|pid| *pid > 1).ok_or(NoTree::OwnPidUnknown)?;
    if parent <= 1 {
        return Err(NoTree::ParentGone);
    }
    let rows = table.read()?;
    ProcessTree::build(Some(own_known), parent, &rows)
}

/// Why a stream is left out.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Exclusion {
    /// Its owning client is this helper: the call's own playback.
    OwnPlayback,
    /// Its owning client is a process of the Conch tree.
    ClientInTree,
    /// Its advisory application pid is a process of the Conch tree.
    ApplicationInTree,
    /// Its application name is the one the owner named.
    ApplicationName,
}

/// Why a stream is not decided on yet.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Deferral {
    /// Its owning client's pid is not known yet.
    NoClientPid,
    /// It has no `object.serial` to capture it by.
    NoSerial,
}

/// What the rule says of one stream.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Verdict {
    /// Captured, by this serial.
    Eligible(u64),
    /// Never captured.
    Excluded(Exclusion),
    /// Not captured now; looked at again at the next evaluation.
    Deferred(Deferral),
}

/// What the rule needs besides the stream: this helper's pid, the Conch tree, and the
/// application name the owner's command carried.
#[derive(Clone, Copy)]
pub struct Rule<'a> {
    /// This process's pid.
    pub own_pid: u32,
    /// The Conch process tree.
    pub tree: &'a ProcessTree,
    /// The owner's own `application.name`.
    pub application_name: &'a str,
}

/// The rule applied to one stream. Exclusions are looked at first: a stream that matches
/// one is never captured, whatever else is not known of it.
#[must_use]
pub fn judge(stream: &StreamFacts, rule: Rule<'_>) -> Verdict {
    if let Some(pid) = stream.sec_pid {
        if pid == rule.own_pid {
            return Verdict::Excluded(Exclusion::OwnPlayback);
        }
        if rule.tree.contains(pid) {
            return Verdict::Excluded(Exclusion::ClientInTree);
        }
    }
    if stream
        .application_process_id
        .is_some_and(|pid| pid == rule.own_pid || rule.tree.contains(pid))
    {
        return Verdict::Excluded(Exclusion::ApplicationInTree);
    }
    if stream.application_name.as_deref() == Some(rule.application_name) {
        return Verdict::Excluded(Exclusion::ApplicationName);
    }
    if stream.sec_pid.is_none() {
        return Verdict::Deferred(Deferral::NoClientPid);
    }
    match stream.serial {
        Some(serial) => Verdict::Eligible(serial),
        None => Verdict::Deferred(Deferral::NoSerial),
    }
}

/// The serials to capture now, each once, in order: every eligible stream's, or none at all
/// when the tree could not be worked out.
///
/// # Errors
///
/// The [`NoTree`] given, and [`NoTree::OwnPidUnknown`] for an own pid that is not known:
/// with either, nothing is captured.
pub fn eligible(
    streams: &[StreamFacts],
    tree: Result<&ProcessTree, NoTree>,
    own_pid: Option<u32>,
    application_name: &str,
) -> Result<Vec<u64>, NoTree> {
    let tree = tree?;
    let own_pid = own_pid.ok_or(NoTree::OwnPidUnknown)?;
    let rule = Rule {
        own_pid,
        tree,
        application_name,
    };
    let serials: BTreeSet<u64> = streams
        .iter()
        .filter_map(|stream| match judge(stream, rule) {
            Verdict::Eligible(serial) => Some(serial),
            Verdict::Excluded(_) | Verdict::Deferred(_) => None,
        })
        .collect();
    Ok(serials.into_iter().collect())
}

#[cfg(test)]
mod tests {
    use super::*;

    const OWN: u32 = 4100;
    const PARENT: u32 = 4000;
    const RENDERER: u32 = 4001;
    const GPU: u32 = 4002;
    /// A grandchild of the parent: Chromium's audio service under its utility process.
    const AUDIO_SERVICE: u32 = 4020;
    const PULSE: u32 = 900;
    const BROWSER: u32 = 7000;
    const PLAYER: u32 = 7100;
    const NAME: &str = "Conch";

    /// A machine: init, the session's PipeWire and its Pulse proxy, Conch's tree, and two
    /// applications of others.
    fn table() -> Vec<(u32, u32)> {
        vec![
            (1, 0),
            (800, 1),
            (PULSE, 800),
            (PARENT, 800),
            (RENDERER, PARENT),
            (GPU, PARENT),
            (4010, PARENT),
            (AUDIO_SERVICE, 4010),
            (OWN, PARENT),
            (BROWSER, 800),
            (PLAYER, 800),
        ]
    }

    fn tree() -> ProcessTree {
        ProcessTree::build(Some(OWN), PARENT, &table()).unwrap()
    }

    fn stream(serial: u64, sec_pid: u32) -> StreamFacts {
        StreamFacts {
            serial: Some(serial),
            sec_pid: Some(sec_pid),
            application_process_id: Some(sec_pid),
            application_name: Some(format!("app-{serial}")),
            media_name: None,
        }
    }

    #[test]
    fn the_tree_is_the_helper_its_parent_and_every_descendant_of_the_parent() {
        let tree = tree();
        for pid in [OWN, PARENT, RENDERER, GPU, 4010, AUDIO_SERVICE] {
            assert!(tree.contains(pid), "{pid}");
        }
        for pid in [1, 800, PULSE, BROWSER, PLAYER] {
            assert!(!tree.contains(pid), "{pid}");
        }
        assert_eq!(tree.len(), 6);
        assert!(!tree.is_empty());
    }

    /// The rule, table-driven over streams and the fake tree above.
    #[test]
    fn each_stream_is_captured_excluded_or_deferred_by_the_rule() {
        let tree = tree();
        let rule = Rule {
            own_pid: OWN,
            tree: &tree,
            application_name: NAME,
        };
        let cases: Vec<(&str, StreamFacts, Verdict)> = vec![
            (
                "the helper's own playback, by its client's pid",
                StreamFacts {
                    application_process_id: None,
                    application_name: Some("conch-voice".into()),
                    ..stream(10, OWN)
                },
                Verdict::Excluded(Exclusion::OwnPlayback),
            ),
            (
                "the helper's own playback claiming another application's pid and name",
                StreamFacts {
                    application_process_id: Some(BROWSER),
                    application_name: Some("Firefox".into()),
                    ..stream(11, OWN)
                },
                Verdict::Excluded(Exclusion::OwnPlayback),
            ),
            (
                "a renderer, a child of the parent",
                stream(12, RENDERER),
                Verdict::Excluded(Exclusion::ClientInTree),
            ),
            (
                "the parent itself",
                stream(13, PARENT),
                Verdict::Excluded(Exclusion::ClientInTree),
            ),
            (
                "a grandchild of the parent",
                stream(14, AUDIO_SERVICE),
                Verdict::Excluded(Exclusion::ClientInTree),
            ),
            (
                "Chromium's audio through pipewire-pulse: the proxy's client pid, the tree's application pid",
                StreamFacts {
                    application_process_id: Some(AUDIO_SERVICE),
                    application_name: Some("Chromium".into()),
                    ..stream(15, PULSE)
                },
                Verdict::Excluded(Exclusion::ApplicationInTree),
            ),
            (
                "the owner's application name, from a process the tree missed",
                StreamFacts {
                    application_process_id: Some(9999),
                    application_name: Some(NAME.into()),
                    ..stream(16, PULSE)
                },
                Verdict::Excluded(Exclusion::ApplicationName),
            ),
            (
                "an unrelated application",
                stream(17, BROWSER),
                Verdict::Eligible(17),
            ),
            (
                "an unrelated application through pipewire-pulse",
                StreamFacts {
                    application_process_id: Some(PLAYER),
                    application_name: Some("mpv".into()),
                    ..stream(18, PULSE)
                },
                Verdict::Eligible(18),
            ),
            (
                "an application that names no pid of its own",
                StreamFacts {
                    application_process_id: None,
                    application_name: None,
                    ..stream(19, PLAYER)
                },
                Verdict::Eligible(19),
            ),
            (
                "a name only like the owner's",
                StreamFacts {
                    application_name: Some("conch".into()),
                    ..stream(20, PLAYER)
                },
                Verdict::Eligible(20),
            ),
            (
                "the owning client's pid not known yet",
                StreamFacts {
                    sec_pid: None,
                    application_process_id: Some(PLAYER),
                    ..stream(21, PLAYER)
                },
                Verdict::Deferred(Deferral::NoClientPid),
            ),
            (
                "the helper's own playback before its client's pid is known: deferred, never captured",
                StreamFacts {
                    sec_pid: None,
                    application_process_id: None,
                    application_name: Some("conch-voice".into()),
                    ..stream(22, OWN)
                },
                Verdict::Deferred(Deferral::NoClientPid),
            ),
            (
                "no serial to capture it by",
                StreamFacts {
                    serial: None,
                    ..stream(23, PLAYER)
                },
                Verdict::Deferred(Deferral::NoSerial),
            ),
            (
                "no serial and the helper's own: excluded",
                StreamFacts {
                    serial: None,
                    ..stream(24, OWN)
                },
                Verdict::Excluded(Exclusion::OwnPlayback),
            ),
        ];
        for (what, facts, want) in cases {
            assert_eq!(judge(&facts, rule), want, "{what}");
        }
    }

    #[test]
    fn eligible_names_each_capturable_serial_once_and_nothing_else() {
        let tree = tree();
        let streams = vec![
            stream(30, OWN),
            stream(31, BROWSER),
            stream(32, RENDERER),
            stream(33, PLAYER),
            stream(31, BROWSER),
            StreamFacts {
                sec_pid: None,
                ..stream(34, PLAYER)
            },
        ];
        assert_eq!(
            eligible(&streams, Ok(&tree), Some(OWN), NAME),
            Ok(vec![31, 33])
        );
        assert_eq!(eligible(&[], Ok(&tree), Some(OWN), NAME), Ok(vec![]));
    }

    /// Fail closed: with no tree, not even an unrelated stream is captured.
    #[test]
    fn without_a_tree_nothing_at_all_is_captured() {
        let streams = vec![stream(40, BROWSER), stream(41, PLAYER)];
        for no_tree in [
            NoTree::Unreadable,
            NoTree::ParentGone,
            NoTree::OwnPidUnknown,
            NoTree::NotParentsChild,
        ] {
            assert_eq!(
                eligible(&streams, Err(no_tree), Some(OWN), NAME),
                Err(no_tree)
            );
            assert!(!no_tree.reason().is_empty());
        }
        let tree = tree();
        assert_eq!(
            eligible(&streams, Ok(&tree), None, NAME),
            Err(NoTree::OwnPidUnknown)
        );
    }

    #[test]
    fn a_parent_of_one_or_zero_an_unknown_own_pid_or_a_stray_table_is_no_tree() {
        assert_eq!(
            ProcessTree::build(Some(OWN), 1, &table()),
            Err(NoTree::ParentGone)
        );
        assert_eq!(
            ProcessTree::build(Some(OWN), 0, &table()),
            Err(NoTree::ParentGone)
        );
        assert_eq!(
            ProcessTree::build(None, PARENT, &table()),
            Err(NoTree::OwnPidUnknown)
        );
        // The helper is not in the table, or is under another parent: not read whole.
        let mut missing = table();
        missing.retain(|&(pid, _)| pid != OWN);
        assert_eq!(
            ProcessTree::build(Some(OWN), PARENT, &missing),
            Err(NoTree::NotParentsChild)
        );
        assert_eq!(
            ProcessTree::build(Some(OWN), RENDERER, &table()),
            Err(NoTree::NotParentsChild)
        );
    }

    #[test]
    fn a_table_that_loops_is_walked_once() {
        let mut looped = table();
        // A cycle that no real table has: each a child of the other.
        looped.push((5000, 5001));
        looped.push((5001, 5000));
        looped.push((PARENT, RENDERER));
        let tree = ProcessTree::build(Some(OWN), PARENT, &looped).unwrap();
        assert!(tree.contains(AUDIO_SERVICE));
        assert!(!tree.contains(5000));
    }

    /// `comm` is the process's own choice, up to 15 bytes: spaces and parentheses in it must
    /// not move the parent pid.
    #[test]
    fn the_parent_pid_is_read_after_the_last_parenthesis() {
        let cases = [
            (
                "4100 (conch-voice) S 4000 4100 4000 0 -1 4194560",
                Some(4000),
            ),
            ("17 (a) (b) S 1234 17 17 0", Some(1234)),
            ("17 (x) ( S 99 1 ) R 555 17 17 0", Some(555)),
            ("17 (with space) S 42 17", Some(42)),
            ("17 ()) S 43 17", Some(43)),
            ("17 (:)) S 7 1 ) S 8 1", Some(8)),
            ("17 (a b c d e f) Z 0 0", Some(0)),
            ("17 (noparen S 1 1", None),
            ("17 (x) S", None),
            ("17 (x) S notapid", None),
            ("", None),
        ];
        for (line, want) in cases {
            assert_eq!(stat_parent(line), want, "{line:?}");
        }
        assert_eq!(stat_pid(b"4100 (conch-voice) S 4000"), Some(4100));
        assert_eq!(stat_pid(b"x (y) S 1"), None);
    }

    /// A fake `/proc` on disk: the real reader, over files laid out as the kernel lays them.
    #[test]
    fn the_process_table_is_read_from_proc_and_a_name_with_parentheses_does_not_fool_it() {
        let root = tempfile::tempdir().unwrap();
        let write = |pid: u32, line: &str| {
            let dir = root.path().join(pid.to_string());
            std::fs::create_dir(&dir).unwrap();
            std::fs::write(dir.join("stat"), line).unwrap();
        };
        write(PARENT, "4000 (conch) S 800 4000 4000 0");
        write(OWN, "4100 (conch-voice) S 4000 4100 4000 0");
        write(RENDERER, "4001 (conch) (renderer) S 4000 4000 4000 0");
        write(BROWSER, "7000 (evil) S 4000 ) S 800 7000 7000 0");
        // Not a process: ignored.
        std::fs::create_dir(root.path().join("self")).unwrap();
        std::fs::write(root.path().join("uptime"), "1 1").unwrap();
        // A process whose stat says another pid: not trusted.
        write(7500, "7501 (x) S 4000 1 1 0");
        let mut proc = Proc::at(root.path().to_path_buf());
        let mut rows = proc.read().unwrap();
        rows.sort_unstable();
        assert_eq!(
            rows,
            vec![
                (PARENT, 800),
                (RENDERER, PARENT),
                (OWN, PARENT),
                (BROWSER, 800)
            ]
        );
        let tree = read_tree(&mut proc, Some(OWN), PARENT).unwrap();
        assert!(tree.contains(RENDERER));
        assert!(
            !tree.contains(BROWSER),
            "a name cannot shift the parent field"
        );

        // A table that cannot be listed.
        let mut gone = Proc::at(root.path().join("absent"));
        assert_eq!(gone.read(), Err(NoTree::Unreadable));
        assert_eq!(
            read_tree(&mut gone, Some(OWN), PARENT),
            Err(NoTree::Unreadable)
        );
        // An owner that is gone is refused before anything is read.
        assert_eq!(read_tree(&mut gone, Some(OWN), 1), Err(NoTree::ParentGone));
        assert_eq!(
            read_tree(&mut gone, None, PARENT),
            Err(NoTree::OwnPidUnknown)
        );
    }

    /// A process that ended while the table was read is left out (its entry is gone); any
    /// other failure to read an entry fails the whole table, closed, since skipping it would
    /// drop that process and its descendants from the tree.
    #[test]
    fn an_entry_that_ended_is_skipped_and_one_that_cannot_be_read_fails_the_table() {
        let root = tempfile::tempdir().unwrap();
        let write = |pid: u32, line: &str| {
            let dir = root.path().join(pid.to_string());
            std::fs::create_dir(&dir).unwrap();
            std::fs::write(dir.join("stat"), line).unwrap();
        };
        write(PARENT, "4000 (conch) S 800 4000 4000 0");
        write(OWN, "4100 (conch-voice) S 4000 4100 4000 0");
        // Ended between the listing and the read: its stat is not there.
        std::fs::create_dir(root.path().join("4300")).unwrap();
        let mut proc = Proc::at(root.path().to_path_buf());
        let mut rows = proc.read().unwrap();
        rows.sort_unstable();
        assert_eq!(rows, vec![(PARENT, 800), (OWN, PARENT)]);

        // A stat that is there and cannot be read (here a directory, which no read of a
        // file gives): not "ended", so the table is not read and nothing is captured.
        std::fs::create_dir_all(root.path().join(RENDERER.to_string()).join("stat")).unwrap();
        assert_eq!(proc.read(), Err(NoTree::Unreadable));
        assert_eq!(
            read_tree(&mut proc, Some(OWN), PARENT),
            Err(NoTree::Unreadable)
        );
    }

    /// A process name need not be UTF-8: the table is read as bytes, the pid before the
    /// name and the parent after its last `)`, so such a name neither fails the read nor
    /// shifts the field.
    #[test]
    fn a_name_that_is_not_utf8_is_read_and_cannot_shift_the_parent() {
        let root = tempfile::tempdir().unwrap();
        let write = |pid: u32, line: &[u8]| {
            let dir = root.path().join(pid.to_string());
            std::fs::create_dir(&dir).unwrap();
            std::fs::write(dir.join("stat"), line).unwrap();
        };
        write(PARENT, b"4000 (conch) S 800 4000 4000 0");
        write(OWN, b"4100 (conch-voice) S 4000 4100 4000 0");
        write(RENDERER, b"4001 (\xff\xfe) S) S 4000 4000 4000 0");
        let mut proc = Proc::at(root.path().to_path_buf());
        let mut rows = proc.read().unwrap();
        rows.sort_unstable();
        assert_eq!(rows, vec![(PARENT, 800), (RENDERER, PARENT), (OWN, PARENT)]);
        assert!(
            read_tree(&mut proc, Some(OWN), PARENT)
                .unwrap()
                .contains(RENDERER)
        );
    }

    /// A refusal is left out only for another user's process (`hidepid=1`), which cannot be
    /// Conch's; any other refusal, or one whose owner is not known, fails the table.
    #[test]
    fn only_another_users_refused_entry_is_left_out() {
        use std::io::{Error, ErrorKind};
        let denied = Error::from(ErrorKind::PermissionDenied);
        let other = Error::from(ErrorKind::InvalidData);
        for (error, entry, own, left_out) in [
            (&denied, Some(1001), Some(1000), true),
            (&denied, Some(1000), Some(1000), false),
            (&denied, None, Some(1000), false),
            (&denied, Some(1001), None, false),
            (&other, Some(1001), Some(1000), false),
        ] {
            assert_eq!(
                hidden(error, entry, own),
                left_out,
                "{error:?} {entry:?} {own:?}"
            );
        }
    }

    /// One of this user's own entries that cannot be read fails the table, closed.
    #[cfg(unix)]
    #[test]
    fn a_refused_entry_of_this_users_own_fails_the_table() {
        use std::os::unix::fs::PermissionsExt;
        let root = tempfile::tempdir().unwrap();
        let dir = root.path().join(OWN.to_string());
        std::fs::create_dir(&dir).unwrap();
        let stat = dir.join("stat");
        std::fs::write(&stat, "4100 (conch-voice) S 4000 4100 4000 0").unwrap();
        std::fs::set_permissions(&stat, std::fs::Permissions::from_mode(0o000)).unwrap();
        if std::fs::read(&stat).is_ok() {
            // Run as root: a mode refuses nothing, so there is no refusal to test.
            eprintln!("a mode does not refuse this user: not tested");
            return;
        }
        let mut proc = Proc::at(root.path().to_path_buf());
        assert_eq!(proc.read(), Err(NoTree::Unreadable));
    }

    /// This process's own tree, from the real `/proc`: it is its parent's child there.
    #[cfg(target_os = "linux")]
    #[test]
    fn this_process_is_in_its_own_tree_in_the_real_proc() {
        let own = std::process::id();
        let parent = std::os::unix::process::parent_id();
        let tree = read_tree(&mut Proc::system(), Some(own), parent).unwrap();
        assert!(tree.contains(own));
        assert!(tree.contains(parent));
    }

    #[test]
    fn a_streams_debug_never_shows_its_application_name() {
        let shown = format!(
            "{:?}",
            StreamFacts {
                application_name: Some("Secret Player".into()),
                ..stream(1, BROWSER)
            }
        );
        assert!(!shown.contains("Secret"), "{shown}");
        assert!(shown.contains(".."), "{shown}");
    }
}
