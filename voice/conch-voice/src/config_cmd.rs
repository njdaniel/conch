//! `conch-voice config set`: saves the microphone or the speakers in `voice.toml`, keeping
//! the rest of the file as it was written (`docs/design/conch-voice.md` §8).
//!
//! Input: the key, `audio.input` or `audio.output`; a device name the command line has
//! already checked; and the configuration directory, by the rule `join` uses. Output:
//! `voice.toml` with that one value changed, or an error and the file as it was. Nothing
//! on standard output.
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
//! - The text is edited by `set_audio_value`, which refuses a file the parser refuses and
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

use std::fs::{self, File, OpenOptions, Permissions};
use std::io::{self, Read, Write};
use std::os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

use conch_voice_api::default_config_dir;
use conch_voice_control::{AudioKey, Config, set_audio_value};

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
const O_NONBLOCK: i32 = 0o4000;

/// How many names are tried for the temporary file before giving up.
const TEMPORARY_NAMES: u32 = 16;

/// Sets `key` to `value` in `voice.toml` in the configuration directory `environment`
/// names, and changes nothing else. `value` must be one the command line accepts (a
/// device name fit for the file, or `default`): `set_audio_value` refuses any other.
///
/// # Errors
///
/// [`Error::NoConfigDir`] when there is no configuration directory; what
/// [`default_config_dir`] says of `CONCH_CONFIG_DIR`; and what [`save`] says.
pub fn set(environment: &Environment, key: AudioKey, value: &str) -> Result<(), Error> {
    let dir = default_config_dir(
        environment.conch_config_dir.as_deref(),
        environment.xdg_config_home.as_deref(),
        environment.home.as_deref(),
    )?
    .ok_or(Error::NoConfigDir)?;
    save(&dir, key, value, &mut || {})
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
pub(crate) fn save(
    dir: &Path,
    key: AudioKey,
    value: &str,
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
    let new = set_audio_value(old, &name, key, value)?;
    if before.is_some() && new == old {
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
#[derive(Debug, PartialEq, Eq)]
struct Snapshot {
    text: String,
    mode: u32,
    device: u64,
    inode: u64,
}

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
struct Temporary {
    path: PathBuf,
    file: File,
    renamed: bool,
}

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
mod tests {
    use std::ffi::OsString;
    use std::sync::mpsc;
    use std::time::Duration;

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
}
