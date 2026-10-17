//! The stored login: the token `conch login` wrote, read as `conch` reads it.
//!
//! The reference is `internal/cli/credentials.go`. The file is
//! `<configuration directory>/credentials.json`, where the directory is `CONCH_CONFIG_DIR`
//! as given or else `<user configuration directory>/conch` ([`default_config_dir`]; the
//! shared vectors in `internal/cli/testdata/config-dir-vectors.json` hold the two programs
//! to one rule), a JSON object from a server's key
//! ([`ServerAddress::key`]) to `{"token": "..."}`; on unix a file that group or others can
//! touch is refused; and `CONCH_TOKEN`, when it is set to more than white space, wins over
//! the file.
//!
//! This module never writes that file and never reads the environment: the caller hands
//! in the value of `CONCH_TOKEN` and the directory (and `default_config_dir` the values of
//! the variables that name it), so a test can never reach the real
//! user's files. The functions are synchronous; they read one small file and are meant to
//! be called before the runtime starts.

use std::collections::HashMap;
use std::ffi::OsStr;
use std::io::Read;
#[cfg(unix)]
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};

use crate::error::Error;
use crate::server::ServerAddress;
use crate::types::{Secret, wire_object};

/// The environment variable whose value the caller passes to [`resolve_token`] as
/// `explicit`. It is how scripts and CI sign in.
pub const TOKEN_ENV: &str = "CONCH_TOKEN";

/// The directory under the user's configuration directory, when `CONCH_CONFIG_DIR` is not set.
const CONFIG_DIR_NAME: &str = "conch";
const CREDENTIALS_FILE: &str = "credentials.json";

/// More than any credentials file holds; a larger one is not read into memory.
const MAX_CREDENTIALS_BYTES: u64 = 1 << 20;

/// The directory that holds `credentials.json` (and `conch-voice`'s `voice.toml`), from the
/// values of `CONCH_CONFIG_DIR`, `XDG_CONFIG_HOME` and `HOME`, by the rule of `configDir` in
/// `internal/cli/credentials.go`:
///
/// 1. `CONCH_CONFIG_DIR`, if it holds more than white space, is the directory exactly as
///    given (not trimmed, no `conch` appended). It must be absolute: a relative one is
///    [`Error::ConfigDirNotAbsolute`], not resolved against the working directory.
/// 2. Otherwise the user's configuration directory plus `conch`, by the rule of Go's
///    `os.UserConfigDir` on Linux: `XDG_CONFIG_HOME` if it is set and absolute, otherwise
///    `$HOME/.config`.
///
/// `Ok(None)` means there is none (a relative `XDG_CONFIG_HOME`, or neither variable set),
/// and then there is no stored login.
///
/// # Errors
///
/// [`Error::ConfigDirNotAbsolute`] for a relative `CONCH_CONFIG_DIR`.
pub fn default_config_dir(
    conch_config_dir: Option<&OsStr>,
    xdg_config_home: Option<&OsStr>,
    home: Option<&OsStr>,
) -> Result<Option<PathBuf>, Error> {
    // A value that is not UTF-8 is not white space.
    let given = conch_config_dir.filter(|dir| dir.to_str().is_none_or(|s| !s.trim().is_empty()));
    if let Some(dir) = given {
        return if Path::new(dir).is_absolute() {
            Ok(Some(PathBuf::from(dir)))
        } else {
            Err(Error::ConfigDirNotAbsolute)
        };
    }
    let base = match xdg_config_home.filter(|dir| !dir.is_empty()) {
        Some(dir) => Path::new(dir).is_absolute().then(|| PathBuf::from(dir)),
        None => home
            .filter(|dir| !dir.is_empty())
            .map(|dir| Path::new(dir).join(".config")),
    };
    Ok(base.map(|base| base.join(CONFIG_DIR_NAME)))
}

/// Where the credentials file is, in the configuration directory [`default_config_dir`]
/// returns.
pub fn credentials_path(config_dir: &Path) -> PathBuf {
    config_dir.join(CREDENTIALS_FILE)
}

/// The bearer token for `server`: `explicit` (the value of `CONCH_TOKEN`) when it holds
/// more than white space, otherwise the stored login under `config_dir`.
///
/// No token anywhere is [`Error::NotSignedIn`]: a missing file, a file with no entry for
/// this server, an entry with an empty token, or no configuration directory at all. A file
/// that is there and cannot be used is a different error, and names the file.
pub fn resolve_token(
    explicit: Option<&str>,
    server: &ServerAddress,
    config_dir: Option<&Path>,
) -> Result<Secret, Error> {
    if let Some(token) = explicit.map(str::trim).filter(|token| !token.is_empty()) {
        return Ok(Secret::new(token));
    }
    let stored = match config_dir {
        Some(dir) => stored_token(server, &credentials_path(dir))?,
        None => None,
    };
    stored.ok_or_else(|| Error::NotSignedIn {
        server: server.key().to_owned(),
    })
}

/// One entry of the credentials file (`storedCredential` in Go).
struct StoredCredential {
    token: Option<Secret>,
}

// A JSON object and nothing else, as Go reads it. serde's derived decoder would also take
// the array ["<token>"] for an entry, and then this program would find a login in a file
// that `conch` refuses.
wire_object!(StoredCredential, "a stored credential object", unknown = ignored, {
    token: optional,
});

/// The servers a login is stored for under `config_dir`, sorted by key: the rule of
/// `storedServers` in `internal/cli/credentials.go`, which `conch` picks its server by when
/// nothing names one.
///
/// A key that is not exactly a server's key as [`ServerAddress::parse`] makes it was not
/// written by `conch login` and names no server to reach, and a key whose host `conch`
/// refuses names one other than its own; both are left out. A file that cannot be used
/// gives none: this only helps choose a server, and reading that server's token then
/// reports what is wrong with the file. Like [`resolve_token`], it never writes anything.
pub fn stored_servers(config_dir: Option<&Path>) -> Vec<ServerAddress> {
    let Some(Ok(Some(logins))) = config_dir.map(|dir| read_logins(&credentials_path(dir))) else {
        return Vec::new();
    };
    let mut servers: Vec<ServerAddress> = logins
        .into_keys()
        .filter_map(|key| {
            ServerAddress::parse(&key)
                .ok()
                .filter(|server| server.key() == key && !server.has_refused_host())
        })
        .collect();
    servers.sort_by(|a, b| a.key().cmp(b.key()));
    servers
}

/// The token stored for `server` in the credentials file at `path`, if there is one.
fn stored_token(server: &ServerAddress, path: &Path) -> Result<Option<Secret>, Error> {
    let Some(logins) = read_logins(path)? else {
        return Ok(None);
    };
    Ok(logins
        .into_iter()
        .find(|(key, _)| key == server.key())
        .and_then(|(_, credential)| credential?.token)
        .filter(|token| !token.is_empty()))
}

/// Every login in the credentials file at `path`, by key; `None` when there is no file.
fn read_logins(path: &Path) -> Result<Option<HashMap<String, Option<StoredCredential>>>, Error> {
    let unreadable = |source| Error::CredentialsUnreadable {
        path: path.to_owned(),
        source,
    };
    // Looked at before it is opened: opening a FIFO waits for a writer. A link is refused
    // too, as `conch` refuses one, so both programs read the same file or none.
    match std::fs::symlink_metadata(path) {
        Ok(meta) if meta.file_type().is_file() => {}
        Ok(_) => {
            return Err(Error::CredentialsNotRegular {
                path: path.to_owned(),
            });
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(unreadable(e)),
    }
    let file = match std::fs::File::open(path) {
        Ok(file) => file,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(unreadable(e)),
    };
    // The open file is what is checked, so the check cannot be about another file.
    // The mode bits exist on unix only; elsewhere there is no group/other rule to apply.
    #[cfg(unix)]
    {
        let mode = file.metadata().map_err(unreadable)?.permissions().mode() & 0o777;
        if mode & 0o077 != 0 {
            return Err(Error::CredentialsExposed {
                path: path.to_owned(),
                mode,
            });
        }
    }

    let mut data = Vec::new();
    file.take(MAX_CREDENTIALS_BYTES + 1)
        .read_to_end(&mut data)
        .map_err(unreadable)?;
    let invalid = |detail: String| Error::CredentialsInvalid {
        path: path.to_owned(),
        detail,
    };
    if data.len() as u64 > MAX_CREDENTIALS_BYTES {
        return Err(invalid("it is larger than 1 MiB".into()));
    }

    // Go decodes the first JSON value in the file and stops, and reads null as no logins
    // and a null entry as an entry with no token. The file is an object of objects: any
    // other shape, anywhere, is the whole file refused, as in Go.
    // A decoding error's own text can quote what it found, and what it found may be a
    // token, so only its position is kept.
    let mut values = serde_json::Deserializer::from_slice(&data)
        .into_iter::<Option<HashMap<String, Option<StoredCredential>>>>();
    match values.next() {
        Some(Ok(logins)) => Ok(Some(logins.unwrap_or_default())),
        Some(Err(e)) => Err(invalid(format!(
            "not the JSON 'conch login' writes, at line {} column {}",
            e.line(),
            e.column()
        ))),
        None => Err(invalid("it is empty".into())),
    }
}

#[cfg(all(test, unix))]
mod tests {
    use std::fs;

    use super::*;

    const FAKE_A: &str = "conch_FAKE_token_for_server_a";
    const FAKE_B: &str = "conch_FAKE_token_for_server_b";
    const FAKE_ENV: &str = "conch_FAKE_token_from_env";

    fn server(raw: &str) -> ServerAddress {
        ServerAddress::parse(raw).unwrap()
    }

    /// A private configuration directory holding a credentials file with these bytes and
    /// this mode.
    fn config_with(contents: &str, mode: u32) -> tempfile::TempDir {
        let dir = tempfile::tempdir().unwrap();
        let path = credentials_path(dir.path());
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(&path, contents).unwrap();
        fs::set_permissions(&path, fs::Permissions::from_mode(mode)).unwrap();
        dir
    }

    /// Byte for byte what `writeCredentials` in `internal/cli/credentials.go` writes for
    /// two logins: `json.MarshalIndent` with two spaces, keys sorted, and a newline.
    fn as_go_writes_it() -> String {
        format!(
            "{{\n  \"http://a:1\": {{\n    \"token\": \"{FAKE_A}\"\n  }},\n  \"https://b.example/conch\": {{\n    \"token\": \"{FAKE_B}\"\n  }}\n}}\n"
        )
    }

    fn resolve(
        explicit: Option<&str>,
        raw: &str,
        dir: &tempfile::TempDir,
    ) -> Result<Secret, Error> {
        resolve_token(explicit, &server(raw), Some(dir.path()))
    }

    #[test]
    fn the_stored_token_is_found_under_the_normalised_server() {
        let dir = config_with(&as_go_writes_it(), 0o600);
        let table = [
            ("http://a:1", FAKE_A),
            ("http://A:1/", FAKE_A),
            ("HTTP://a:1//", FAKE_A),
            ("http://user@a:1", FAKE_A),
            ("https://b.example/conch", FAKE_B),
            ("https://B.EXAMPLE/conch/", FAKE_B),
        ];
        for (raw, want) in table {
            assert_eq!(resolve(None, raw, &dir).unwrap().expose(), want, "{raw}");
        }
    }

    #[test]
    fn no_entry_for_the_server_is_not_signed_in() {
        let dir = config_with(&as_go_writes_it(), 0o600);
        for raw in [
            "http://c:3",
            "https://a:1",
            "http://a:2",
            "http://a",
            "https://b.example",
            "https://b.example/Conch",
            "https://b.example/conch/more",
        ] {
            match resolve(None, raw, &dir) {
                Err(Error::NotSignedIn { server: key }) => assert_eq!(key, server(raw).key()),
                other => panic!("{raw}: expected NotSignedIn, got {other:?}"),
            }
        }
    }

    #[test]
    fn a_missing_file_or_directory_is_not_signed_in_and_names_no_path() {
        let empty = tempfile::tempdir().unwrap();
        let no_file = tempfile::tempdir().unwrap();
        let gone = empty.path().join("does-not-exist");

        let results = [
            resolve_token(None, &server("http://a:1"), Some(empty.path())),
            resolve_token(None, &server("http://a:1"), Some(no_file.path())),
            resolve_token(None, &server("http://a:1"), Some(&gone)),
            resolve_token(None, &server("http://a:1"), None),
            resolve_token(Some(""), &server("http://a:1"), None),
        ];
        for result in results {
            let error = result.unwrap_err();
            assert!(matches!(error, Error::NotSignedIn { .. }), "{error:?}");
            assert_eq!(
                error.to_string(),
                "not signed in to http://a:1: run 'conch login'"
            );
            assert!(!format!("{error:?}").contains(&*empty.path().to_string_lossy()));
        }
    }

    #[test]
    fn an_entry_with_no_token_is_not_signed_in() {
        for contents in [
            r#"{"http://a:1": {"token": ""}}"#,
            r#"{"http://a:1": {}}"#,
            r#"{"http://a:1": {"token": null}}"#,
            r#"{"http://a:1": null}"#,
            "{}",
            "null",
        ] {
            let dir = config_with(contents, 0o600);
            let result = resolve(None, "http://a:1", &dir);
            assert!(
                matches!(result, Err(Error::NotSignedIn { .. })),
                "{contents}: {result:?}"
            );
        }
    }

    #[test]
    fn the_explicit_token_wins_when_it_is_more_than_white_space() {
        let dir = config_with(&as_go_writes_it(), 0o600);
        // (the value of CONCH_TOKEN, the token used)
        let table = [
            (None, FAKE_A),
            (Some(""), FAKE_A),
            (Some("   "), FAKE_A),
            (Some("\t\n"), FAKE_A),
            (Some(FAKE_ENV), FAKE_ENV),
            (Some(" conch_FAKE_token_from_env\n"), FAKE_ENV),
        ];
        for (explicit, want) in table {
            let got = resolve(explicit, "http://a:1", &dir).unwrap();
            assert_eq!(got.expose(), want, "{explicit:?}");
        }
        // It applies to every server, including one with nothing stored and no directory.
        assert_eq!(
            resolve(Some(FAKE_ENV), "http://other:2", &dir)
                .unwrap()
                .expose(),
            FAKE_ENV
        );
        let token = resolve_token(Some(FAKE_ENV), &server("http://other:2"), None).unwrap();
        assert_eq!(token.expose(), FAKE_ENV);
    }

    #[test]
    fn the_explicit_token_is_used_without_looking_at_the_file() {
        // As in Go: CONCH_TOKEN is returned before the file is opened, so a file that
        // would be refused does not stand in the way of a script's login.
        let exposed = config_with(&as_go_writes_it(), 0o644);
        assert_eq!(
            resolve(Some(FAKE_ENV), "http://a:1", &exposed)
                .unwrap()
                .expose(),
            FAKE_ENV
        );
        let broken = config_with("not json", 0o600);
        assert_eq!(
            resolve(Some(FAKE_ENV), "http://a:1", &broken)
                .unwrap()
                .expose(),
            FAKE_ENV
        );
    }

    #[test]
    fn a_file_others_can_touch_is_refused_naming_the_file_and_the_fix() {
        // (mode, refused)
        let table = [
            (0o600, false),
            (0o400, false),
            (0o700, false),
            (0o640, true),
            (0o620, true),
            (0o610, true),
            (0o604, true),
            (0o602, true),
            (0o601, true),
            (0o644, true),
            (0o660, true),
            (0o666, true),
            (0o777, true),
        ];
        for (mode, refused) in table {
            let dir = config_with(&as_go_writes_it(), mode);
            let path = credentials_path(dir.path());
            let result = resolve(None, "http://a:1", &dir);
            if !refused {
                assert_eq!(result.unwrap().expose(), FAKE_A, "mode {mode:o}");
                continue;
            }
            let error = result.unwrap_err();
            match &error {
                Error::CredentialsExposed { path: p, mode: m } => {
                    assert_eq!((p, *m), (&path, mode));
                }
                other => panic!("mode {mode:o}: expected CredentialsExposed, got {other:?}"),
            }
            let shown = error.to_string();
            assert_eq!(
                shown,
                format!(
                    "credentials file {0:?} is accessible by other users (mode {mode:04o}); run 'chmod 600 {0:?}'",
                    path
                )
            );
            assert!(!shown.contains("FAKE"), "{shown}");
        }
    }

    #[test]
    fn a_file_that_is_not_the_expected_json_is_refused_without_quoting_it() {
        for contents in [
            "",
            "not json",
            "[]",
            r#""conch_FAKE_token_for_server_a""#,
            r#"{"http://a:1": "conch_FAKE_token_for_server_a"}"#,
            r#"{"http://a:1": {"token": 12345}}"#,
            r#"{"http://a:1": {"token": ["conch_FAKE_token_for_server_a"]}}"#,
            // An entry, or the file, written as an array: Go refuses the file, and so
            // must this, whichever server is asked for.
            r#"{"http://a:1": ["conch_FAKE_token_for_server_a"]}"#,
            r#"{"http://other:9": ["conch_FAKE_token_for_server_a"], "http://a:1": {"token": "conch_FAKE_token_for_server_a"}}"#,
            r#"{"http://a:1": [{"token": "conch_FAKE_token_for_server_a"}]}"#,
            r#"[{"http://a:1": {"token": "conch_FAKE_token_for_server_a"}}]"#,
            r#"[["http://a:1", {"token": "conch_FAKE_token_for_server_a"}]]"#,
            r#"{"http://a:1": "conch_FAKE_token_for_server_a", "http://b:2": {}}"#,
            r#"{"http://a:1": {"token": "conch_FAKE_token_for_server_a", "token": "conch_FAKE_again"}}"#,
            r#"{"http://a:1": {"token": "conch_FAKE_token_for_server_a"}"#,
            r#"{"http://a:1": {"token": "conch_FAKE_token_for_server_a}}"#,
        ] {
            let dir = config_with(contents, 0o600);
            let error = resolve(None, "http://a:1", &dir).unwrap_err();
            assert!(
                matches!(error, Error::CredentialsInvalid { .. }),
                "{contents}: {error:?}"
            );
            let shown = format!("{error} {error:?}");
            assert!(shown.contains("credentials.json"), "{shown}");
            assert!(
                !shown.contains("FAKE") && !shown.contains("12345"),
                "{contents}: {shown}"
            );
        }
    }

    #[test]
    fn what_follows_the_first_json_value_is_ignored_as_go_ignores_it() {
        let dir = config_with(&format!("{}\ntrailing text", as_go_writes_it()), 0o600);
        assert_eq!(resolve(None, "http://a:1", &dir).unwrap().expose(), FAKE_A);
        let dir = config_with(
            &format!(r#"{{"http://a:1": {{"token": "{FAKE_A}", "added_later": true}}}}"#),
            0o600,
        );
        assert_eq!(resolve(None, "http://a:1", &dir).unwrap().expose(), FAKE_A);
    }

    #[test]
    fn a_file_too_large_to_be_credentials_is_refused() {
        let padding = " ".repeat(usize::try_from(MAX_CREDENTIALS_BYTES).unwrap());
        let dir = config_with(&format!("{padding}{}", as_go_writes_it()), 0o600);
        let error = resolve(None, "http://a:1", &dir).unwrap_err();
        // Refused for its size, and said to be: not read to the bound and then called
        // malformed where the read happened to stop.
        match &error {
            Error::CredentialsInvalid { detail, .. } => {
                assert_eq!(detail, "it is larger than 1 MiB");
            }
            other => panic!("{other:?}"),
        }
        // One byte less is read, and is the login it holds.
        let padding =
            " ".repeat(usize::try_from(MAX_CREDENTIALS_BYTES).unwrap() - as_go_writes_it().len());
        let dir = config_with(&format!("{padding}{}", as_go_writes_it()), 0o600);
        assert_eq!(resolve(None, "http://a:1", &dir).unwrap().expose(), FAKE_A);
    }

    #[test]
    fn something_that_is_not_a_file_is_an_error_that_names_it() {
        // credentials.json is a directory: refused before it is opened (this case used to
        // open and fail on the read as CredentialsUnreadable).
        let dir = tempfile::tempdir().unwrap();
        let path = credentials_path(dir.path());
        fs::create_dir_all(&path).unwrap();
        fs::set_permissions(&path, fs::Permissions::from_mode(0o700)).unwrap();
        let error = resolve(None, "http://a:1", &dir).unwrap_err();
        match &error {
            Error::CredentialsNotRegular { path: p } => assert_eq!(p, &path),
            other => panic!("expected CredentialsNotRegular, got {other:?}"),
        }
        assert!(error.to_string().contains("credentials.json"));
    }

    #[test]
    fn a_file_this_user_may_not_open_is_an_error_that_names_it_and_not_a_missing_login() {
        let dir = config_with(&as_go_writes_it(), 0o000);
        let path = credentials_path(dir.path());
        if fs::File::open(&path).is_ok() {
            // Root, or a process that may read anything: the mode stops nothing, so there
            // is nothing here to test.
            eprintln!("skipped: this process can open a file of mode 000");
            return;
        }
        let error = resolve(None, "http://a:1", &dir).unwrap_err();
        match &error {
            Error::CredentialsUnreadable { path: p, source } => {
                assert_eq!(p, &path);
                assert_eq!(source.kind(), std::io::ErrorKind::PermissionDenied);
            }
            other => panic!("expected CredentialsUnreadable, got {other:?}"),
        }
        assert!(error.to_string().contains("credentials.json"), "{error}");
        assert!(!format!("{error} {error:?}").contains("FAKE"));

        // The same for a directory on the way to the file that may not be entered.
        let dir = config_with(&as_go_writes_it(), 0o600);
        // The configuration directory itself, which now holds the file directly.
        let sub = tempfile::tempdir_in(dir.path()).unwrap();
        let path = sub.path().join("config");
        fs::create_dir(&path).unwrap();
        fs::write(credentials_path(&path), as_go_writes_it()).unwrap();
        fs::set_permissions(credentials_path(&path), fs::Permissions::from_mode(0o600)).unwrap();
        fs::set_permissions(&path, fs::Permissions::from_mode(0o000)).unwrap();
        let result = resolve_token(None, &server("http://a:1"), Some(&path));
        fs::set_permissions(&path, fs::Permissions::from_mode(0o700)).unwrap();
        assert!(
            matches!(result, Err(Error::CredentialsUnreadable { .. })),
            "{result:?}"
        );
    }

    #[test]
    fn reading_the_login_never_changes_the_file_or_its_directory() {
        let dir = config_with(&as_go_writes_it(), 0o600);
        let path = credentials_path(dir.path());
        let before = (
            fs::read(&path).unwrap(),
            fs::metadata(&path).unwrap().modified().unwrap(),
        );
        for raw in ["http://a:1", "http://c:3"] {
            let _ = resolve(None, raw, &dir);
            let _ = resolve(Some(FAKE_ENV), raw, &dir);
        }
        let after = (
            fs::read(&path).unwrap(),
            fs::metadata(&path).unwrap().modified().unwrap(),
        );
        assert_eq!(before, after);
        assert_eq!(
            fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o600
        );
        let entries: Vec<_> = fs::read_dir(path.parent().unwrap())
            .unwrap()
            .map(|e| e.unwrap().file_name())
            .collect();
        assert_eq!(entries, [OsStr::new("credentials.json")]);
    }

    #[derive(serde::Deserialize)]
    struct ConfigDirVector {
        note: String,
        conch_config_dir: String,
        xdg_config_home: String,
        home: String,
        want: String,
        want_error: bool,
    }

    #[test]
    fn the_directory_matches_go_for_every_shared_vector() {
        let path = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../../internal/cli/testdata/config-dir-vectors.json");
        let text =
            fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {}: {e}", path.display()));
        let vectors: Vec<ConfigDirVector> = serde_json::from_str(&text).unwrap();
        assert!(vectors.len() >= 10, "only {} vectors", vectors.len());
        // "" in a vector means the variable is unset.
        fn var(value: &str) -> Option<&OsStr> {
            (!value.is_empty()).then(|| OsStr::new(value))
        }
        for v in &vectors {
            let got = default_config_dir(
                var(&v.conch_config_dir),
                var(&v.xdg_config_home),
                var(&v.home),
            );
            if v.want_error {
                let error = got.unwrap_err();
                assert!(matches!(error, Error::ConfigDirNotAbsolute), "{}", v.note);
                continue;
            }
            let want = (!v.want.is_empty()).then(|| PathBuf::from(&v.want));
            assert_eq!(got.unwrap(), want, "{}", v.note);
        }
    }

    #[test]
    fn a_relative_config_dir_is_one_line_naming_the_variable_and_not_the_value() {
        let error = default_config_dir(
            Some(OsStr::new("relative/FAKE_secret_dir")),
            Some(OsStr::new("/made/up/xdg")),
            None,
        )
        .unwrap_err();
        let shown = format!("{error} {error:?}");
        assert_eq!(
            error.to_string(),
            "CONCH_CONFIG_DIR must be an absolute path"
        );
        assert!(!shown.contains("FAKE"), "{shown}");
    }

    #[test]
    fn the_credentials_file_sits_directly_in_the_directory() {
        assert_eq!(
            credentials_path(Path::new("/made/up/alice")),
            PathBuf::from("/made/up/alice/credentials.json")
        );
    }

    /// A FIFO at `dir/credentials.json`, or None (with a message) without `mkfifo`.
    fn fifo_in(dir: &Path) -> Option<PathBuf> {
        let path = credentials_path(dir);
        match std::process::Command::new("mkfifo").arg(&path).status() {
            Ok(status) if status.success() => Some(path),
            _ => {
                eprintln!("skipped: mkfifo is not available");
                None
            }
        }
    }

    #[test]
    fn a_fifo_is_refused_at_once_and_does_not_hang() {
        let dir = tempfile::tempdir().unwrap();
        if fifo_in(dir.path()).is_none() {
            return;
        }
        let path = dir.path().to_owned();
        let (tx, rx) = std::sync::mpsc::channel();
        std::thread::spawn(move || {
            let _ = tx.send(resolve_token(None, &server("http://a:1"), Some(&path)));
        });
        let result = rx
            .recv_timeout(std::time::Duration::from_secs(10))
            .expect("resolve_token waited on the FIFO");
        match result {
            Err(error @ Error::CredentialsNotRegular { .. }) => {
                assert!(error.to_string().contains("not a regular file"), "{error}");
            }
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn a_symlink_at_the_leaf_is_refused_even_to_a_good_file() {
        let real = config_with(&as_go_writes_it(), 0o600);
        let dir = tempfile::tempdir().unwrap();
        std::os::unix::fs::symlink(credentials_path(real.path()), credentials_path(dir.path()))
            .unwrap();
        let result = resolve_token(None, &server("http://a:1"), Some(dir.path()));
        assert!(
            matches!(result, Err(Error::CredentialsNotRegular { .. })),
            "{result:?}"
        );
        // A directory in its place is refused the same way.
        let dir = tempfile::tempdir().unwrap();
        fs::create_dir(credentials_path(dir.path())).unwrap();
        let result = resolve_token(None, &server("http://a:1"), Some(dir.path()));
        assert!(matches!(result, Err(Error::CredentialsNotRegular { .. })));
    }

    #[test]
    fn an_error_is_one_line_even_if_the_directory_name_has_a_newline() {
        let parent = tempfile::tempdir().unwrap();
        let dir = parent.path().join("evil\nname");
        fs::create_dir(&dir).unwrap();
        fs::write(credentials_path(&dir), as_go_writes_it()).unwrap();
        fs::set_permissions(credentials_path(&dir), fs::Permissions::from_mode(0o644)).unwrap();
        let error = resolve_token(None, &server("http://a:1"), Some(&dir)).unwrap_err();
        assert!(matches!(error, Error::CredentialsExposed { .. }));
        assert!(!error.to_string().contains('\n'), "{error}");
    }

    fn keys(servers: &[ServerAddress]) -> Vec<&str> {
        servers.iter().map(ServerAddress::key).collect()
    }

    #[test]
    fn the_stored_servers_are_the_keys_conch_login_writes_sorted() {
        // (the file, the servers listed)
        let table: [(String, &[&str]); 8] = [
            (
                as_go_writes_it(),
                &["http://a:1", "https://b.example/conch"],
            ),
            ("{}".into(), &[]),
            ("null".into(), &[]),
            // An entry without a token is listed as conch lists it: the key is there.
            (
                r#"{"http://b:2": null, "http://a:1": {}}"#.into(),
                &["http://a:1", "http://b:2"],
            ),
            // Keys conch login would not write: not a URL, not normalised, a scheme it
            // refuses, and hosts with a letter it refuses (which are never their own key).
            (
                format!(
                    r#"{{"http://a:1": {{"token": "{FAKE_A}"}}, "not a url": {{"token": "{FAKE_B}"}},
                        "HTTP://Other:2/": {{"token": "{FAKE_B}"}}, "http://other:2/": {{"token": "{FAKE_B}"}},
                        "ftp://c:3": {{"token": "{FAKE_B}"}}, "http://\u0130.example": {{"token": "{FAKE_B}"}},
                        "http://[fe80::1%c4%b0]": {{"token": "{FAKE_B}"}}}}"#
                ),
                &["http://a:1"],
            ),
            // A file that cannot be used lists nothing; reading a token says why.
            ("not json".into(), &[]),
            (format!(r#"{{"http://a:1": "{FAKE_A}"}}"#), &[]),
            ("".into(), &[]),
        ];
        for (contents, want) in table {
            let dir = config_with(&contents, 0o600);
            assert_eq!(keys(&stored_servers(Some(dir.path()))), want, "{contents}");
        }
    }

    #[test]
    fn no_file_or_one_that_cannot_be_used_lists_no_server() {
        assert!(stored_servers(None).is_empty());
        let empty = tempfile::tempdir().unwrap();
        assert!(stored_servers(Some(empty.path())).is_empty());
        assert!(stored_servers(Some(&empty.path().join("gone"))).is_empty());
        // Others may read it: conch would not use it, and neither does this.
        let exposed = config_with(&as_go_writes_it(), 0o644);
        assert!(stored_servers(Some(exposed.path())).is_empty());
        // A link to a good file, a directory in its place, a FIFO that would never answer.
        let real = config_with(&as_go_writes_it(), 0o600);
        let linked = tempfile::tempdir().unwrap();
        std::os::unix::fs::symlink(
            credentials_path(real.path()),
            credentials_path(linked.path()),
        )
        .unwrap();
        assert!(stored_servers(Some(linked.path())).is_empty());
        let directory = tempfile::tempdir().unwrap();
        fs::create_dir(credentials_path(directory.path())).unwrap();
        assert!(stored_servers(Some(directory.path())).is_empty());
        let fifo = tempfile::tempdir().unwrap();
        if fifo_in(fifo.path()).is_some() {
            let path = fifo.path().to_owned();
            let (tx, rx) = std::sync::mpsc::channel();
            std::thread::spawn(move || {
                let _ = tx.send(stored_servers(Some(&path)).len());
            });
            let listed = rx
                .recv_timeout(std::time::Duration::from_secs(10))
                .expect("stored_servers waited on the FIFO");
            assert_eq!(listed, 0);
        }
    }

    #[test]
    fn listing_the_servers_shows_no_token_and_changes_nothing() {
        let dir = config_with(&as_go_writes_it(), 0o600);
        let path = credentials_path(dir.path());
        let before = fs::read(&path).unwrap();
        let servers = stored_servers(Some(dir.path()));
        let shown = format!("{servers:?} {servers:#?}");
        assert!(!shown.contains("FAKE"), "{shown}");
        assert_eq!(fs::read(&path).unwrap(), before);
        // A server listed is one whose token is found under it.
        for server in &servers {
            assert!(
                resolve_token(None, server, Some(dir.path())).is_ok(),
                "{server:?}"
            );
        }
    }

    #[test]
    fn two_directories_never_see_each_others_login() {
        let a = config_with(&as_go_writes_it(), 0o600);
        let b = tempfile::tempdir().unwrap();
        let from_a = resolve_token(None, &server("http://a:1"), Some(a.path())).unwrap();
        assert_eq!(from_a.expose(), FAKE_A);
        let from_b = resolve_token(None, &server("http://a:1"), Some(b.path()));
        assert!(
            matches!(from_b, Err(Error::NotSignedIn { .. })),
            "{from_b:?}"
        );
    }
}
