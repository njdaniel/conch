//! `conch-voice config set` and `config unset`: save the microphone, the speakers, the talk
//! mode, or the key device and bindings in `voice.toml`, keeping the rest of the file as it
//! was written (`docs/design/conch-voice.md` §8).
//!
//! Input: the key, `audio.input`, `audio.output`, `audio.talk_mode`, `keys.device`,
//! `keys.talk`, `keys.mute` or `keys.deafen`; a device name, a talk mode, an event device
//! path or a key the command line has already checked (for `unset`, only the key, one of
//! the four `keys.*`); and the configuration directory, by the rule `join` uses. Output:
//! `voice.toml` with that one value changed or removed, or an error and the file as it was.
//! Nothing on standard output, and no key, device path or file text in any error.
//! Owns: `voice.toml`, for as long as the save takes, and the temporary file beside it.
//! It writes nothing else: never `credentials.json`, and never the directory, which it
//! does not create.
//!
//! The file's `server` decides where the login token is sent, so the save asks of the file
//! what `cli::read_config` asks, and more:
//!
//! - A symbolic link, anything that is not a regular file, and a file that group or others
//!   can write are refused. Each is looked at before it is opened, so a FIFO cannot make the
//!   command wait, and the file opened is checked to be the one looked at.
//! - The text is edited by `set_audio_value`, `set_keys_value` or `unset_keys_value`, which refuse a file the parser refuses and
//!   checks its own result: the same configuration but for the one field, and the same
//!   bytes but for the one value.
//! - The new text goes to a new file in the same directory, created exclusively, written
//!   and synced, with the old file's mode (0600 for a new file); a rename puts it in
//!   `voice.toml`'s place, and the directory is synced. A reader sees the old file or the
//!   new one, never half of either.
//! - Just before the rename, `voice.toml` is read again. If anything changed it, the new
//!   file is removed and the save refused: what the other writer wrote is kept.
//!
//! What remains: a change made between that last read and the rename is lost. Closing that
//! window would need a lock every writer takes, a person's editor included.

#[cfg(unix)]
use std::fs::{self, File, OpenOptions, Permissions};
#[cfg(unix)]
use std::io::{self, Read, Write};
#[cfg(unix)]
use std::os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt};
#[cfg(unix)]
use std::path::{Path, PathBuf};
#[cfg(unix)]
use std::time::{SystemTime, UNIX_EPOCH};

#[cfg(unix)]
use conch_voice_api::default_config_dir;
use conch_voice_control::{AudioKey, KeysKey};
#[cfg(unix)]
use conch_voice_control::{Config, set_audio_value, set_keys_value, unset_keys_value};

use crate::cli::Environment;
use crate::error::Error;

/// The configuration file's name in the configuration directory.
pub const FILE_NAME: &str = "voice.toml";

/// The mode of a `voice.toml` this command creates: the person's own, as `conch login`
/// makes `credentials.json`.
pub const NEW_FILE_MODE: u32 = 0o600;

/// `O_NONBLOCK` on Linux (x86-64 and AArch64, the targets conch-voice is built for). std
/// has no name for it and `libc` is not a dependency here; on a regular file it changes
/// nothing.
#[cfg(target_os = "linux")]
const O_NONBLOCK: i32 = 0o4000;

/// `O_NONBLOCK` on the other Unix systems that share the BSD value (macOS, the BSDs).
#[cfg(all(unix, not(target_os = "linux")))]
const O_NONBLOCK: i32 = 0x0004;

/// How many names are tried for the temporary file before giving up.
#[cfg(unix)]
const TEMPORARY_NAMES: u32 = 16;

/// Sets `key` to `value` in `voice.toml` in the configuration directory `environment`
/// names, and changes nothing else. `value` must be one the command line accepts (a
/// device name fit for the file, or `default`; for the talk mode, `push_to_talk` or
/// `open`): `set_audio_value` refuses any other.
///
/// # Errors
///
/// [`Error::NoConfigDir`] when there is no configuration directory; what
/// [`default_config_dir`] says of `CONCH_CONFIG_DIR`; and what [`save`] says.
pub fn set(environment: &Environment, key: AudioKey, value: &str) -> Result<(), Error> {
    #[cfg(unix)]
    {
        let dir = config_dir(environment)?;
        save(&dir, key, value, &mut || {})
    }
    #[cfg(not(unix))]
    {
        let _ = (environment, key, value);
        Err(unsupported())
    }
}

/// Sets `key` under `[keys]` to `value`, as [`set`] does for `[audio]`.
///
/// # Errors
///
/// As [`set`].
pub fn set_keys(environment: &Environment, key: KeysKey, value: &str) -> Result<(), Error> {
    #[cfg(unix)]
    {
        let dir = config_dir(environment)?;
        save_keys(&dir, key, value, &mut || {})
    }
    #[cfg(not(unix))]
    {
        let _ = (environment, key, value);
        Err(unsupported())
    }
}

/// Removes `key` from `[keys]`; a key that is not there, or no file, is a success that
/// writes nothing.
///
/// # Errors
///
/// As [`set`].
pub fn unset_keys(environment: &Environment, key: KeysKey) -> Result<(), Error> {
    #[cfg(unix)]
    {
        let dir = config_dir(environment)?;
        unset_keys_in(&dir, key, &mut || {})
    }
    #[cfg(not(unix))]
    {
        let _ = (environment, key);
        Err(unsupported())
    }
}

/// What the three commands above answer where the file's checks and its atomic replace
/// have no Windows form yet (the Windows port of the configuration is #904):
/// [`Error::Unsupported`], with `voice.toml` untouched. Compiled on Linux too so that a
/// test holds it.
#[cfg(any(not(unix), test))]
fn unsupported() -> Error {
    Error::Unsupported("changing the configuration file (config set)")
}

#[cfg(unix)]
fn config_dir(environment: &Environment) -> Result<PathBuf, Error> {
    default_config_dir(
        environment.conch_config_dir.as_deref(),
        environment.xdg_config_home.as_deref(),
        environment.home.as_deref(),
    )?
    .ok_or(Error::NoConfigDir)
}

/// Sets `key` to `value` in `voice.toml` in `dir`, as [`set`] does. `before_rename` is
/// called after the new file is written and before `voice.toml` is read again: tests
/// change the file there, as another writer would.
///
/// # Errors
///
/// - [`Error::NoConfigDir`] if `dir` is not a directory.
/// - [`Error::ConfigLink`], [`Error::ConfigNotRegular`] or [`Error::ConfigWritable`] for a
///   `voice.toml` that is a link, is not a regular file, or that others can write.
/// - [`Error::ConfigToFix`] for one the parser refuses; [`Error::Config`] for one whose
///   `[audio]` is in a form that is not edited.
/// - [`Error::ConfigChanged`] if something else changed it during the save.
/// - [`Error::ConfigRead`] and [`Error::ConfigWrite`] for what the operating system refused.
///
/// In every case `voice.toml` is as it was, and no temporary file is left.
#[cfg(unix)]
pub(crate) fn save(
    dir: &Path,
    key: AudioKey,
    value: &str,
    before_rename: &mut dyn FnMut(),
) -> Result<(), Error> {
    save_with(
        dir,
        &|old, name| set_audio_value(old, name, key, value).map_err(Error::from),
        before_rename,
    )
}

/// [`save`] for a `[keys]` value.
#[cfg(unix)]
pub(crate) fn save_keys(
    dir: &Path,
    key: KeysKey,
    value: &str,
    before_rename: &mut dyn FnMut(),
) -> Result<(), Error> {
    save_with(
        dir,
        &|old, name| set_keys_value(old, name, key, value).map_err(Error::from),
        before_rename,
    )
}

/// [`save`] for the removal of a `[keys]` value.
#[cfg(unix)]
pub(crate) fn unset_keys_in(
    dir: &Path,
    key: KeysKey,
    before_rename: &mut dyn FnMut(),
) -> Result<(), Error> {
    save_with(
        dir,
        &|old, name| unset_keys_value(old, name, key).map_err(Error::from),
        before_rename,
    )
}

/// The save every command shares: `edit` turns the file's text (and the name to call it in
/// errors) into the new text.
#[cfg(unix)]
fn save_with(
    dir: &Path,
    edit: &dyn Fn(&str, &str) -> Result<String, Error>,
    before_rename: &mut dyn FnMut(),
) -> Result<(), Error> {
    match fs::metadata(dir) {
        Ok(meta) if meta.is_dir() => {}
        Ok(_) => return Err(Error::NoConfigDir),
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Err(Error::NoConfigDir),
        Err(source) => {
            return Err(Error::ConfigRead {
                path: dir.to_owned(),
                source,
            });
        }
    }
    let path = dir.join(FILE_NAME);
    let before = Snapshot::take(&path)?;
    let old = before.as_ref().map_or("", |before| before.text.as_str());
    // Named as `read_config` names it, so an error reads the same from either.
    let name = format!("{path:?}");
    Config::parse(old, &name).map_err(Error::ConfigToFix)?;
    let new = edit(old, &name)?;
    // Nothing to change: the file is not touched, and a missing one is not created.
    if new == old {
        return Ok(());
    }

    let write = |source| Error::ConfigWrite {
        path: path.clone(),
        source,
    };
    // Never more open than the file it replaces: its mode without group or other write,
    // which the snapshot has already refused, taken off again all the same.
    let mode = before
        .as_ref()
        .map_or(NEW_FILE_MODE, |before| before.mode & !0o022);
    let mut temporary = Temporary::create(dir).map_err(write)?;
    temporary
        .file
        .write_all(new.as_bytes())
        .and_then(|()| temporary.file.set_permissions(Permissions::from_mode(mode)))
        .and_then(|()| temporary.file.sync_all())
        .map_err(write)?;

    before_rename();
    if Snapshot::take(&path)? != before {
        return Err(Error::ConfigChanged { path });
    }
    fs::rename(&temporary.path, &path).map_err(write)?;
    temporary.renamed = true;
    // The rename is in the directory: synced, it is on the disk. The file is saved either
    // way, so a directory that cannot be synced is not reported as a failed save.
    let _ = File::open(dir).and_then(|dir| dir.sync_all());
    Ok(())
}

/// `voice.toml` as it was found: its text, and what would show that it was replaced or
/// changed.
#[cfg(unix)]
#[derive(Debug, PartialEq, Eq)]
struct Snapshot {
    text: String,
    mode: u32,
    device: u64,
    inode: u64,
}

#[cfg(unix)]
impl Snapshot {
    /// Reads the file at `path`, or `None` if there is none. Refuses what `save` documents,
    /// looking before it opens.
    fn take(path: &Path) -> Result<Option<Self>, Error> {
        let read = |source| Error::ConfigRead {
            path: path.to_owned(),
            source,
        };
        let meta = match fs::symlink_metadata(path) {
            Ok(meta) => meta,
            Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(None),
            Err(source) => return Err(read(source)),
        };
        if meta.file_type().is_symlink() {
            return Err(Error::ConfigLink {
                path: path.to_owned(),
            });
        }
        if !meta.is_file() {
            return Err(Error::ConfigNotRegular {
                path: path.to_owned(),
            });
        }
        let mode = meta.mode() & 0o777;
        if mode & 0o022 != 0 {
            return Err(Error::ConfigWritable {
                path: path.to_owned(),
                mode,
            });
        }
        // Opened without waiting: a FIFO put in the file's place since it was looked at
        // would otherwise block the open until something writes to it. The check below
        // then refuses it as a change.
        let mut file = OpenOptions::new()
            .read(true)
            .custom_flags(O_NONBLOCK)
            .open(path)
            .map_err(read)?;
        // What was opened must be what was looked at: a link or another file put in its
        // place between the two is a change.
        let opened = file.metadata().map_err(read)?;
        if (opened.dev(), opened.ino()) != (meta.dev(), meta.ino()) {
            return Err(Error::ConfigChanged {
                path: path.to_owned(),
            });
        }
        let mut text = String::new();
        file.read_to_string(&mut text).map_err(read)?;
        Ok(Some(Self {
            text,
            mode,
            device: meta.dev(),
            inode: meta.ino(),
        }))
    }
}

/// The new file, beside `voice.toml` until it is renamed to it, and removed if it is not.
#[cfg(unix)]
struct Temporary {
    path: PathBuf,
    file: File,
    renamed: bool,
}

#[cfg(unix)]
impl Temporary {
    /// Creates a file no one else has made, in `dir`, readable and writable by its owner
    /// only until its mode is set.
    fn create(dir: &Path) -> io::Result<Self> {
        let process = std::process::id();
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_or(0, |since| since.subsec_nanos());
        for attempt in 0..TEMPORARY_NAMES {
            let path = dir.join(format!(".{FILE_NAME}.{process}-{nanos}-{attempt}.tmp"));
            // O_CREAT | O_EXCL: never a file or a link that is already there.
            match OpenOptions::new()
                .write(true)
                .create_new(true)
                .mode(NEW_FILE_MODE)
                .open(&path)
            {
                Ok(file) => {
                    return Ok(Self {
                        path,
                        file,
                        renamed: false,
                    });
                }
                Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {}
                Err(error) => return Err(error),
            }
        }
        Err(io::Error::new(
            io::ErrorKind::AlreadyExists,
            "no free name for a temporary file",
        ))
    }
}

#[cfg(unix)]
impl Drop for Temporary {
    fn drop(&mut self) {
        if !self.renamed {
            // Nothing more can be done about a file that cannot be removed; the error that
            // brought us here is the one to report.
            let _ = fs::remove_file(&self.path);
        }
    }
}

#[cfg(test)]
mod unsupported_tests {
    use super::*;

    #[test]
    fn the_non_unix_answer_is_typed_and_names_the_command() {
        let error = unsupported();
        assert!(matches!(error, Error::Unsupported(_)));
        assert!(error.to_string().contains("config set"), "{error}");
        assert_eq!(error.exit_code(), 2);
    }
}

#[cfg(all(test, unix))]
mod tests {
    use std::ffi::OsString;
    use std::sync::mpsc;
    use std::time::Duration;

    use conch_voice_control::TalkMode;

    use super::*;

    const FAKE_LOGIN: &str = "conch_FAKE_login_token_do_not_print";
    const CREDENTIALS: &str =
        "{\"http://127.0.0.1:8080\":{\"token\":\"conch_FAKE_login_token_do_not_print\"}}";
    const KEPT: &str = "# mine\nserver = \"http://127.0.0.1:8080\" # here\n\n[keys]\ntalk = \"KEY_F13\"\n\n[audio]\n# the headset\ninput = \"alsa_input.old\" # by hand\nrelease_tail_ms = 150\n";

    fn write(path: &Path, text: &str, mode: u32) {
        fs::write(path, text).unwrap();
        fs::set_permissions(path, Permissions::from_mode(mode)).unwrap();
    }

    fn mode(path: &Path) -> u32 {
        fs::symlink_metadata(path).unwrap().mode() & 0o777
    }

    /// The names in `dir`, sorted: a temporary file left behind would be among them.
    fn names(dir: &Path) -> Vec<OsString> {
        let mut names: Vec<_> = fs::read_dir(dir)
            .unwrap()
            .map(|entry| entry.unwrap().file_name())
            .collect();
        names.sort();
        names
    }

    fn save_now(dir: &Path, key: AudioKey, value: &str) -> Result<(), Error> {
        save(dir, key, value, &mut || {})
    }

    #[test]
    fn the_mode_is_kept_and_only_the_value_changes() {
        for kept in [0o600, 0o644, 0o640, 0o400] {
            let dir = tempfile::tempdir().unwrap();
            let path = dir.path().join(FILE_NAME);
            write(&path, KEPT, kept);
            save_now(dir.path(), AudioKey::Input, "alsa_input.new").unwrap();
            assert_eq!(
                fs::read_to_string(&path).unwrap(),
                KEPT.replace("alsa_input.old", "alsa_input.new"),
                "{kept:o}"
            );
            assert_eq!(mode(&path), kept, "{kept:o}");
            assert_eq!(names(dir.path()), [FILE_NAME], "{kept:o}");
        }
    }

    #[test]
    fn a_missing_file_is_created_0600_with_only_the_table_and_the_key() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(FILE_NAME);
        save_now(dir.path(), AudioKey::Output, "default").unwrap();
        assert_eq!(
            fs::read_to_string(&path).unwrap(),
            "[audio]\noutput = \"default\"\n"
        );
        assert_eq!(mode(&path), NEW_FILE_MODE);
        assert!(fs::symlink_metadata(&path).unwrap().is_file());
    }

    #[test]
    fn what_was_saved_is_what_join_and_managed_read() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(FILE_NAME);
        write(&path, KEPT, 0o600);
        save_now(dir.path(), AudioKey::Input, "alsa_input.new").unwrap();
        save_now(dir.path(), AudioKey::Output, "alsa_output.new").unwrap();
        let config = crate::cli::read_config(&path).unwrap();
        assert_eq!(config.audio.input.as_deref(), Some("alsa_input.new"));
        assert_eq!(config.audio.output.as_deref(), Some("alsa_output.new"));
        assert_eq!(config.audio.release_tail_ms, 150);
        assert_eq!(config.server.as_deref(), Some("http://127.0.0.1:8080"));
    }

    #[test]
    fn the_credentials_file_is_never_written() {
        let dir = tempfile::tempdir().unwrap();
        let credentials = dir.path().join("credentials.json");
        write(&credentials, CREDENTIALS, 0o600);
        let before = fs::symlink_metadata(&credentials).unwrap();
        save_now(dir.path(), AudioKey::Input, "a").unwrap();
        save_now(dir.path(), AudioKey::Input, "b").unwrap();
        let after = fs::symlink_metadata(&credentials).unwrap();
        assert_eq!(fs::read_to_string(&credentials).unwrap(), CREDENTIALS);
        assert_eq!(
            (after.ino(), after.mode(), after.mtime(), after.mtime_nsec()),
            (
                before.ino(),
                before.mode(),
                before.mtime(),
                before.mtime_nsec()
            )
        );
        assert_eq!(names(dir.path()), ["credentials.json", FILE_NAME]);
    }

    #[test]
    fn a_file_others_can_write_is_refused_and_left_as_it_was() {
        for open in [0o664, 0o646, 0o666, 0o620, 0o602] {
            let dir = tempfile::tempdir().unwrap();
            let path = dir.path().join(FILE_NAME);
            write(&path, KEPT, open);
            let error = save_now(dir.path(), AudioKey::Input, "x").unwrap_err();
            assert!(
                matches!(error, Error::ConfigWritable { mode, .. } if mode == open),
                "{open:o}: {error}"
            );
            assert_eq!(fs::read_to_string(&path).unwrap(), KEPT);
            assert_eq!(mode(&path), open);
            assert_eq!(names(dir.path()), [FILE_NAME]);
        }
    }

    #[test]
    fn a_link_is_refused_and_neither_it_nor_what_it_names_is_written() {
        let dir = tempfile::tempdir().unwrap();
        let elsewhere = tempfile::tempdir().unwrap();
        let target = elsewhere.path().join("real.toml");
        write(&target, KEPT, 0o600);
        let path = dir.path().join(FILE_NAME);
        std::os::unix::fs::symlink(&target, &path).unwrap();
        let error = save_now(dir.path(), AudioKey::Input, "x").unwrap_err();
        assert!(matches!(error, Error::ConfigLink { .. }), "{error}");
        assert_eq!(
            error.to_string(),
            format!("{path:?} is a link: change it by hand")
        );
        assert!(
            fs::symlink_metadata(&path)
                .unwrap()
                .file_type()
                .is_symlink()
        );
        assert_eq!(fs::read_to_string(&target).unwrap(), KEPT);
        assert_eq!(names(dir.path()), [FILE_NAME]);

        // A link that names nothing is refused the same way, and nothing is created.
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(FILE_NAME);
        let dangling = elsewhere.path().join("not-there.toml");
        std::os::unix::fs::symlink(&dangling, &path).unwrap();
        let error = save_now(dir.path(), AudioKey::Input, "x").unwrap_err();
        assert!(matches!(error, Error::ConfigLink { .. }), "{error}");
        assert!(!dangling.exists());
    }

    #[test]
    fn a_directory_is_refused() {
        let dir = tempfile::tempdir().unwrap();
        fs::create_dir(dir.path().join(FILE_NAME)).unwrap();
        let error = save_now(dir.path(), AudioKey::Input, "x").unwrap_err();
        assert!(matches!(error, Error::ConfigNotRegular { .. }), "{error}");
        assert_eq!(names(dir.path()), [FILE_NAME]);
    }

    #[test]
    fn a_fifo_is_refused_without_being_opened() {
        let dir = tempfile::tempdir().unwrap();
        let made = std::process::Command::new("mkfifo")
            .arg(dir.path().join(FILE_NAME))
            .status();
        if !matches!(made, Ok(status) if status.success()) {
            eprintln!("skipped: mkfifo is not available");
            return;
        }
        let at = dir.path().to_owned();
        let (tx, rx) = mpsc::channel();
        std::thread::spawn(move || {
            let _ = tx.send(save_now(&at, AudioKey::Input, "x"));
        });
        let result = rx
            .recv_timeout(Duration::from_secs(10))
            .expect("the save waited on the FIFO");
        assert!(
            matches!(result, Err(Error::ConfigNotRegular { .. })),
            "{result:?}"
        );
        assert_eq!(names(dir.path()), [FILE_NAME]);
    }

    #[test]
    fn no_configuration_directory_is_refused_and_none_is_created() {
        let parent = tempfile::tempdir().unwrap();
        let missing = parent.path().join("conch");
        let error = save_now(&missing, AudioKey::Input, "x").unwrap_err();
        assert!(matches!(error, Error::NoConfigDir), "{error}");
        assert_eq!(
            error.to_string(),
            "no configuration directory: run `conch login` first"
        );
        assert!(!missing.exists());

        // A file where the directory should be is no directory either.
        fs::write(&missing, "").unwrap();
        let error = save_now(&missing, AudioKey::Input, "x").unwrap_err();
        assert!(matches!(error, Error::NoConfigDir), "{error}");

        // Nothing names a directory at all.
        let nothing = Environment {
            token: Some(conch_voice_api::Secret::new(FAKE_LOGIN)),
            ..Environment::default()
        };
        let error = set(&nothing, AudioKey::Input, "x").unwrap_err();
        assert!(matches!(error, Error::NoConfigDir), "{error}");

        // The directory the environment names is the one written in.
        let dir = tempfile::tempdir().unwrap();
        let named = Environment {
            conch_config_dir: Some(dir.path().as_os_str().to_owned()),
            ..Environment::default()
        };
        set(&named, AudioKey::Input, "x").unwrap();
        assert!(dir.path().join(FILE_NAME).is_file());
        let relative = Environment {
            conch_config_dir: Some(OsString::from("relative/FAKE_dir")),
            ..Environment::default()
        };
        assert_eq!(
            set(&relative, AudioKey::Input, "x")
                .unwrap_err()
                .to_string(),
            "CONCH_CONFIG_DIR must be an absolute path"
        );
    }

    #[test]
    fn a_file_the_parser_refuses_or_an_audio_table_in_another_form_is_left_as_it_was() {
        for (text, fix) in [
            ("chanel = \"typo\"\n", true),
            ("[audio]\ninput = false\n", true),
            ("audio = { input = \"a\" }\n", false),
            ("audio.input = \"a\"\n", false),
        ] {
            let dir = tempfile::tempdir().unwrap();
            let path = dir.path().join(FILE_NAME);
            write(&path, text, 0o600);
            let error = save_now(dir.path(), AudioKey::Input, "x").unwrap_err();
            if fix {
                assert!(matches!(error, Error::ConfigToFix(_)), "{text:?}: {error}");
                assert!(
                    error.to_string().ends_with("; fix voice.toml first"),
                    "{error}"
                );
            } else {
                assert!(
                    matches!(
                        error,
                        Error::Config(conch_voice_control::Error::AudioFormNotEditable { .. })
                    ),
                    "{text:?}: {error}"
                );
            }
            assert_eq!(fs::read_to_string(&path).unwrap(), text);
            assert_eq!(names(dir.path()), [FILE_NAME]);
        }
    }

    #[test]
    fn a_change_made_during_the_save_is_kept_and_the_save_refused() {
        let other = "# another writer's\n[audio]\ninput = \"theirs\"\n";
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(FILE_NAME);
        write(&path, KEPT, 0o600);
        let at = path.clone();
        let error = save(dir.path(), AudioKey::Input, "mine", &mut || {
            write(&at, other, 0o600);
        })
        .unwrap_err();
        assert!(matches!(error, Error::ConfigChanged { .. }), "{error}");
        assert_eq!(
            error.to_string(),
            format!("{path:?} changed while saving: try again")
        );
        assert_eq!(fs::read_to_string(&path).unwrap(), other);
        assert_eq!(names(dir.path()), [FILE_NAME]);

        // A file another writer replaced with the same text is still another file.
        write(&path, KEPT, 0o600);
        let at = path.clone();
        let error = save(dir.path(), AudioKey::Input, "mine", &mut || {
            let beside = at.with_extension("other");
            write(&beside, KEPT, 0o600);
            fs::rename(&beside, &at).unwrap();
        })
        .unwrap_err();
        assert!(matches!(error, Error::ConfigChanged { .. }), "{error}");
        assert_eq!(fs::read_to_string(&path).unwrap(), KEPT);

        // A file made while a missing one was being created.
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(FILE_NAME);
        let at = path.clone();
        let error = save(dir.path(), AudioKey::Input, "mine", &mut || {
            write(&at, other, 0o600);
        })
        .unwrap_err();
        assert!(matches!(error, Error::ConfigChanged { .. }), "{error}");
        assert_eq!(fs::read_to_string(&path).unwrap(), other);
        assert_eq!(names(dir.path()), [FILE_NAME]);

        // A file made into a link while it was being saved.
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(FILE_NAME);
        write(&path, KEPT, 0o600);
        let at = path.clone();
        let error = save(dir.path(), AudioKey::Input, "mine", &mut || {
            fs::remove_file(&at).unwrap();
            std::os::unix::fs::symlink("/dev/null", &at).unwrap();
        })
        .unwrap_err();
        assert!(matches!(error, Error::ConfigLink { .. }), "{error}");
        assert!(
            fs::symlink_metadata(&path)
                .unwrap()
                .file_type()
                .is_symlink()
        );
        assert_eq!(names(dir.path()), [FILE_NAME]);
    }

    #[test]
    fn saving_the_value_already_there_writes_nothing() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(FILE_NAME);
        write(&path, "[audio]\ninput = \"a\"\n", 0o644);
        let before = fs::symlink_metadata(&path).unwrap().ino();
        save_now(dir.path(), AudioKey::Input, "a").unwrap();
        assert_eq!(fs::symlink_metadata(&path).unwrap().ino(), before);
    }

    #[test]
    fn every_error_is_one_line_even_if_the_directory_name_has_a_newline() {
        let parent = tempfile::tempdir().unwrap();
        let dir = parent.path().join("evil\nname");
        fs::create_dir(&dir).unwrap();
        let path = dir.join(FILE_NAME);
        let cases: [(&str, u32); 3] = [
            ("chanel = \"typo\"\n", 0o600),
            ("audio.input = \"a\"\n", 0o600),
            (KEPT, 0o666),
        ];
        for (text, open) in cases {
            write(&path, text, open);
            let error = save_now(&dir, AudioKey::Input, "x").unwrap_err();
            assert!(!error.to_string().contains('\n'), "{error}");
        }
        fs::remove_file(&path).unwrap();
        std::os::unix::fs::symlink("/dev/null", &path).unwrap();
        let error = save_now(&dir, AudioKey::Input, "x").unwrap_err();
        assert!(!error.to_string().contains('\n'), "{error}");
    }

    /// `config set audio.talk_mode open`, as the TUI saves the toggle: the file keeps its
    /// comments, its mode and every other key, and `join` and `managed` read the mode back.
    #[test]
    fn the_talk_mode_is_saved_keeping_the_rest_of_the_file() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(FILE_NAME);
        write(&path, KEPT, 0o640);
        save_now(dir.path(), AudioKey::TalkMode, "open").unwrap();
        assert_eq!(
            fs::read_to_string(&path).unwrap(),
            KEPT.replace("[audio]\n", "[audio]\ntalk_mode = \"open\"\n")
        );
        assert_eq!(mode(&path), 0o640);
        assert_eq!(names(dir.path()), [FILE_NAME]);
        let config = crate::cli::read_config(&path).unwrap();
        assert_eq!(config.audio.talk_mode, TalkMode::Open);
        assert_eq!(config.audio.input.as_deref(), Some("alsa_input.old"));
        assert_eq!(config.audio.release_tail_ms, 150);

        // And back, changing the value alone.
        save_now(dir.path(), AudioKey::TalkMode, "push_to_talk").unwrap();
        assert_eq!(
            fs::read_to_string(&path).unwrap(),
            KEPT.replace("[audio]\n", "[audio]\ntalk_mode = \"push_to_talk\"\n")
        );

        // A mode that does not exist is refused, and the file is as it was.
        let before = fs::read_to_string(&path).unwrap();
        assert!(save_now(dir.path(), AudioKey::TalkMode, "always").is_err());
        assert_eq!(fs::read_to_string(&path).unwrap(), before);
        assert_eq!(names(dir.path()), [FILE_NAME]);

        // A missing file is created with only the table and the key.
        let empty = tempfile::tempdir().unwrap();
        save_now(empty.path(), AudioKey::TalkMode, "open").unwrap();
        assert_eq!(
            fs::read_to_string(empty.path().join(FILE_NAME)).unwrap(),
            "[audio]\ntalk_mode = \"open\"\n"
        );
        assert_eq!(mode(&empty.path().join(FILE_NAME)), NEW_FILE_MODE);
    }

    const KEYS_FILE: &str = "# mine\nserver = \"http://127.0.0.1:8080\"\n\n[keys]\n# the keyboard\ndevice = \"/dev/input/by-id/old-event-kbd\" # by hand\ntalk = \"KEY_F13\"\nmute = 276\n\n[audio]\ninput = \"x\"\n";
    const MOUSE: &str = "/dev/input/by-id/usb-Example_Mouse-event-mouse";

    fn save_key(dir: &Path, key: KeysKey, value: &str) -> Result<(), Error> {
        save_keys(dir, key, value, &mut || {})
    }

    fn unset_key(dir: &Path, key: KeysKey) -> Result<(), Error> {
        unset_keys_in(dir, key, &mut || {})
    }

    fn read(dir: &Path) -> Config {
        crate::cli::read_config(&dir.join(FILE_NAME)).unwrap()
    }

    #[test]
    fn keys_are_set_changing_only_their_value_and_read_back() {
        // (key, value, expected file text)
        let cases: [(KeysKey, &str, String); 5] = [
            (
                KeysKey::Device,
                MOUSE,
                KEYS_FILE.replace("/dev/input/by-id/old-event-kbd", MOUSE),
            ),
            (
                KeysKey::Talk,
                "275",
                KEYS_FILE.replace("\"KEY_F13\"", "\"275\""),
            ),
            (
                KeysKey::Talk,
                "767",
                KEYS_FILE.replace("\"KEY_F13\"", "\"767\""),
            ),
            (
                KeysKey::Deafen,
                "KEY_F14",
                KEYS_FILE.replace("[keys]\n", "[keys]\ndeafen = \"KEY_F14\"\n"),
            ),
            (
                KeysKey::Mute,
                "KEY_F15",
                KEYS_FILE.replace("mute = 276", "mute = \"KEY_F15\""),
            ),
        ];
        for (key, value, want) in cases {
            let dir = tempfile::tempdir().unwrap();
            let path = dir.path().join(FILE_NAME);
            write(&path, KEYS_FILE, 0o640);
            save_key(dir.path(), key, value).unwrap_or_else(|e| panic!("{key:?} {value}: {e}"));
            let text = fs::read_to_string(&path).unwrap();
            // The deafen line is inserted directly after the table header.
            assert_eq!(text, want, "{key:?}");
            assert_eq!(mode(&path), 0o640);
            assert_eq!(names(dir.path()), [FILE_NAME]);
            let config = read(dir.path());
            match key {
                KeysKey::Device => assert_eq!(config.keys.device.as_deref(), Some(value)),
                KeysKey::Talk => {
                    assert_eq!(config.keys.talk.map(|c| c.code()), value.parse().ok());
                }
                KeysKey::Mute => assert!(config.keys.mute.is_some()),
                KeysKey::Deafen => assert!(config.keys.deafen.is_some()),
            }
            assert_eq!(config.audio.input.as_deref(), Some("x"));
        }
    }

    #[test]
    fn a_missing_file_is_created_private_with_only_the_keys_table() {
        let dir = tempfile::tempdir().unwrap();
        save_key(dir.path(), KeysKey::Talk, "275").unwrap();
        let path = dir.path().join(FILE_NAME);
        assert_eq!(
            fs::read_to_string(&path).unwrap(),
            "[keys]\ntalk = \"275\"\n"
        );
        assert_eq!(mode(&path), NEW_FILE_MODE);
    }

    #[test]
    fn a_value_that_is_not_a_device_or_a_key_is_refused_and_the_file_untouched() {
        let bad: [(KeysKey, &str); 15] = [
            (KeysKey::Device, "/tmp/event3"),
            // An empty part names a directory, which join would refuse.
            (KeysKey::Device, "/dev/input//event3"),
            (KeysKey::Device, "/dev/input/event3/"),
            (KeysKey::Device, "/dev/input/by-id/"),
            (KeysKey::Device, "/dev/input"),
            (KeysKey::Device, "/dev/input/"),
            (KeysKey::Device, "/dev/input/../null"),
            (KeysKey::Device, "/dev/input/by-id/a\"b"),
            (KeysKey::Device, "/dev/input/by-id/a\nb"),
            (KeysKey::Device, "/dev/input/by-id/a\\b"),
            (KeysKey::Talk, "0"),
            (KeysKey::Talk, "768"),
            (KeysKey::Mute, "KEY_NOT_A_KEY_FAKE"),
            (KeysKey::Deafen, ""),
            (KeysKey::Talk, "99999999999999999999999"),
        ];
        for (key, value) in bad {
            let dir = tempfile::tempdir().unwrap();
            let path = dir.path().join(FILE_NAME);
            write(&path, KEYS_FILE, 0o600);
            let error = save_key(dir.path(), key, value).unwrap_err();
            assert!(matches!(error, Error::Config(_)), "{value:?}: {error}");
            assert!(!error.to_string().contains('\n'), "{error:?}");
            assert_eq!(fs::read_to_string(&path).unwrap(), KEYS_FILE);
            assert_eq!(names(dir.path()), [FILE_NAME]);
        }
        // Codes 275 and 767 are accepted, 0 and 768 refused (above).
        let dir = tempfile::tempdir().unwrap();
        for code in ["275", "767"] {
            save_key(dir.path(), KeysKey::Talk, code).unwrap();
        }
    }

    #[test]
    fn a_binding_another_key_has_is_refused() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(FILE_NAME);
        write(&path, KEYS_FILE, 0o600);
        let error = save_key(dir.path(), KeysKey::Deafen, "KEY_F13").unwrap_err();
        assert!(matches!(error, Error::Config(_)), "{error}");
        assert_eq!(fs::read_to_string(&path).unwrap(), KEYS_FILE);
    }

    #[test]
    fn unset_removes_that_line_and_nothing_else() {
        for (key, line) in [
            (
                KeysKey::Device,
                "device = \"/dev/input/by-id/old-event-kbd\" # by hand\n",
            ),
            (KeysKey::Talk, "talk = \"KEY_F13\"\n"),
            (KeysKey::Mute, "mute = 276\n"),
        ] {
            let dir = tempfile::tempdir().unwrap();
            let path = dir.path().join(FILE_NAME);
            write(&path, KEYS_FILE, 0o640);
            unset_key(dir.path(), key).unwrap();
            assert_eq!(
                fs::read_to_string(&path).unwrap(),
                KEYS_FILE.replace(line, "")
            );
            assert_eq!(mode(&path), 0o640);
            assert_eq!(names(dir.path()), [FILE_NAME]);
            let config = read(dir.path());
            assert_eq!(config.audio.input.as_deref(), Some("x"));
            assert_eq!(config.server.as_deref(), Some("http://127.0.0.1:8080"));
        }
        // Set, then unset, leaves the file as it was but for the empty table header.
        let dir = tempfile::tempdir().unwrap();
        save_key(dir.path(), KeysKey::Talk, "275").unwrap();
        unset_key(dir.path(), KeysKey::Talk).unwrap();
        assert_eq!(
            fs::read_to_string(dir.path().join(FILE_NAME)).unwrap(),
            "[keys]\n"
        );
        assert_eq!(read(dir.path()).keys.talk, None);
    }

    #[test]
    fn unsetting_an_absent_key_or_file_succeeds_and_writes_nothing() {
        let dir = tempfile::tempdir().unwrap();
        unset_key(dir.path(), KeysKey::Device).unwrap();
        assert_eq!(names(dir.path()), Vec::<OsString>::new());

        for text in ["[audio]\ninput = \"x\"\n", "[keys]\ntalk = 97\n"] {
            let path = dir.path().join(FILE_NAME);
            write(&path, text, 0o644);
            let ino = fs::symlink_metadata(&path).unwrap().ino();
            unset_key(dir.path(), KeysKey::Device).unwrap();
            assert_eq!(fs::read_to_string(&path).unwrap(), text);
            assert_eq!(fs::symlink_metadata(&path).unwrap().ino(), ino);
            assert_eq!(names(dir.path()), [FILE_NAME]);
        }
    }

    #[test]
    fn the_safety_rules_of_config_set_apply_to_keys_and_unset() {
        type Op = fn(&Path) -> Result<(), Error>;
        let ops: [Op; 2] = [
            |dir| save_key(dir, KeysKey::Talk, "275"),
            |dir| unset_key(dir, KeysKey::Talk),
        ];
        for op in ops {
            // Others-writable.
            let dir = tempfile::tempdir().unwrap();
            let path = dir.path().join(FILE_NAME);
            write(&path, KEYS_FILE, 0o666);
            assert!(matches!(op(dir.path()), Err(Error::ConfigWritable { .. })));
            assert_eq!(fs::read_to_string(&path).unwrap(), KEYS_FILE);

            // A link, to a file that must stay as it is.
            let dir = tempfile::tempdir().unwrap();
            let elsewhere = tempfile::tempdir().unwrap();
            let target = elsewhere.path().join("real.toml");
            write(&target, KEYS_FILE, 0o600);
            std::os::unix::fs::symlink(&target, dir.path().join(FILE_NAME)).unwrap();
            assert!(matches!(op(dir.path()), Err(Error::ConfigLink { .. })));
            assert_eq!(fs::read_to_string(&target).unwrap(), KEYS_FILE);

            // A directory.
            let dir = tempfile::tempdir().unwrap();
            fs::create_dir(dir.path().join(FILE_NAME)).unwrap();
            assert!(matches!(
                op(dir.path()),
                Err(Error::ConfigNotRegular { .. })
            ));

            // A file refused as it stands.
            let dir = tempfile::tempdir().unwrap();
            let path = dir.path().join(FILE_NAME);
            write(&path, "chanel = \"typo\"\n", 0o600);
            assert!(matches!(op(dir.path()), Err(Error::ConfigToFix(_))));
            assert_eq!(fs::read_to_string(&path).unwrap(), "chanel = \"typo\"\n");

            // Inline and dotted [keys].
            for text in ["keys = { talk = 97 }\n", "keys.talk = 97\n"] {
                let dir = tempfile::tempdir().unwrap();
                let path = dir.path().join(FILE_NAME);
                write(&path, text, 0o600);
                let error = op(dir.path()).unwrap_err();
                assert!(
                    matches!(
                        error,
                        Error::Config(conch_voice_control::Error::KeysFormNotEditable { .. })
                    ),
                    "{text:?}: {error}"
                );
                assert_eq!(fs::read_to_string(&path).unwrap(), text);
                assert_eq!(names(dir.path()), [FILE_NAME]);
            }

            // Nothing missing: no directory is made.
            let parent = tempfile::tempdir().unwrap();
            assert!(matches!(
                op(&parent.path().join("conch")),
                Err(Error::NoConfigDir)
            ));
        }
    }

    #[test]
    fn a_change_during_a_keys_save_is_kept_and_nothing_secret_is_shown() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(FILE_NAME);
        write(&path, KEYS_FILE, 0o600);
        let at = path.clone();
        let error = save_keys(dir.path(), KeysKey::Device, MOUSE, &mut || {
            write(&at, "[keys]\ntalk = 97\n", 0o600);
        })
        .unwrap_err();
        assert!(matches!(error, Error::ConfigChanged { .. }), "{error}");
        assert!(!error.to_string().contains("Mouse"));
        assert_eq!(fs::read_to_string(&path).unwrap(), "[keys]\ntalk = 97\n");
        assert_eq!(names(dir.path()), [FILE_NAME]);
    }
}
