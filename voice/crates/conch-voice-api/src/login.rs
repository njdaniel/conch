//! The stored login: the token `conch login` wrote, read as `conch` reads it.
//!
//! The reference is `internal/cli/credentials.go`. The file is
//! `<configuration directory>/conch/credentials.json`, a JSON object from a server's key
//! ([`ServerAddress::key`]) to `{"token": "..."}`; a file that group or others can touch is
//! refused; and `CONCH_TOKEN`, when it is set to more than white space, wins over the file.
//!
//! This module never writes that file and never reads the environment: the caller hands
//! in the value of `CONCH_TOKEN` and the directory, so a test can never reach the real
//! user's files. The functions are synchronous; they read one small file and are meant to
//! be called before the runtime starts.

use std::collections::HashMap;
use std::ffi::OsStr;
use std::io::Read;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};

use crate::error::Error;
use crate::server::ServerAddress;
use crate::types::{Secret, wire_object};

/// The environment variable whose value the caller passes to [`resolve_token`] as
/// `explicit`. It is how scripts and CI sign in.
pub const TOKEN_ENV: &str = "CONCH_TOKEN";

const CREDENTIALS_DIR: &str = "conch";
const CREDENTIALS_FILE: &str = "credentials.json";

/// More than any credentials file holds; a larger one is not read into memory.
const MAX_CREDENTIALS_BYTES: u64 = 1 << 20;

/// The user's configuration directory, from the values of `XDG_CONFIG_HOME` and `HOME`,
/// by the rule of Go's `os.UserConfigDir` on Linux: `XDG_CONFIG_HOME` if it is set and
/// absolute, otherwise `$HOME/.config`. `None` means there is none (a relative
/// `XDG_CONFIG_HOME`, or neither variable set), and then there is no stored login.
pub fn default_config_dir(
    xdg_config_home: Option<&OsStr>,
    home: Option<&OsStr>,
) -> Option<PathBuf> {
    match xdg_config_home.filter(|dir| !dir.is_empty()) {
        Some(dir) => Path::new(dir).is_absolute().then(|| PathBuf::from(dir)),
        None => home
            .filter(|dir| !dir.is_empty())
            .map(|dir| Path::new(dir).join(".config")),
    }
}

/// Where the credentials file is, under a configuration directory.
pub fn credentials_path(config_dir: &Path) -> PathBuf {
    config_dir.join(CREDENTIALS_DIR).join(CREDENTIALS_FILE)
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

/// The token stored for `server` in the credentials file at `path`, if there is one.
fn stored_token(server: &ServerAddress, path: &Path) -> Result<Option<Secret>, Error> {
    let unreadable = |source| Error::CredentialsUnreadable {
        path: path.to_owned(),
        source,
    };
    let file = match std::fs::File::open(path) {
        Ok(file) => file,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(unreadable(e)),
    };
    // The open file is what is checked, so the check cannot be about another file.
    let mode = file.metadata().map_err(unreadable)?.permissions().mode() & 0o777;
    if mode & 0o077 != 0 {
        return Err(Error::CredentialsExposed {
            path: path.to_owned(),
            mode,
        });
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
    let logins = match values.next() {
        Some(Ok(logins)) => logins.unwrap_or_default(),
        Some(Err(e)) => {
            return Err(invalid(format!(
                "not the JSON 'conch login' writes, at line {} column {}",
                e.line(),
                e.column()
            )));
        }
        None => return Err(invalid("it is empty".into())),
    };
    Ok(logins
        .into_iter()
        .find(|(key, _)| key == server.key())
        .and_then(|(_, credential)| credential?.token)
        .filter(|token| !token.is_empty()))
}

#[cfg(test)]
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
        fs::create_dir(no_file.path().join("conch")).unwrap();
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
                    "credentials file {0} is accessible by other users (mode {mode:04o}); run 'chmod 600 {0}'",
                    path.display()
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
    fn a_file_that_cannot_be_read_is_an_error_that_names_it() {
        // credentials.json is a directory: it opens, and reading it fails.
        let dir = tempfile::tempdir().unwrap();
        let path = credentials_path(dir.path());
        fs::create_dir_all(&path).unwrap();
        fs::set_permissions(&path, fs::Permissions::from_mode(0o700)).unwrap();
        let error = resolve(None, "http://a:1", &dir).unwrap_err();
        match &error {
            Error::CredentialsUnreadable { path: p, .. } => assert_eq!(p, &path),
            other => panic!("expected CredentialsUnreadable, got {other:?}"),
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
        let conch = dir.path().join("conch");
        fs::set_permissions(&conch, fs::Permissions::from_mode(0o000)).unwrap();
        let result = resolve(None, "http://a:1", &dir);
        fs::set_permissions(&conch, fs::Permissions::from_mode(0o700)).unwrap();
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

    #[test]
    fn the_default_directory_follows_gos_user_config_dir() {
        let os = |s: &'static str| Some(OsStr::new(s));
        // (XDG_CONFIG_HOME, HOME, the directory). Made-up values: the environment of the
        // process running this test is never read.
        let table = [
            (
                os("/made/up/xdg"),
                os("/made/up/home"),
                Some("/made/up/xdg"),
            ),
            (os("/made/up/xdg"), None, Some("/made/up/xdg")),
            (None, os("/made/up/home"), Some("/made/up/home/.config")),
            (os(""), os("/made/up/home"), Some("/made/up/home/.config")),
            (os("relative/xdg"), os("/made/up/home"), None),
            (None, None, None),
            (None, os(""), None),
            (os(""), os(""), None),
        ];
        for (xdg, home, want) in table {
            assert_eq!(
                default_config_dir(xdg, home),
                want.map(PathBuf::from),
                "XDG_CONFIG_HOME={xdg:?} HOME={home:?}"
            );
        }
        assert_eq!(
            credentials_path(Path::new("/made/up/xdg")),
            PathBuf::from("/made/up/xdg/conch/credentials.json")
        );
    }
}
