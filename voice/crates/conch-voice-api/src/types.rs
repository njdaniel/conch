//! Rust copies of the wire types the voice client uses.
//!
//! `pkg/schema` is the truth and these are copies of it (`voice.go`, `message_v2.go` for
//! [`Audience`], `timestamp.go`, `message.go` for [`ErrorBody`]). The test at the end of
//! this file reads every golden fixture for these shapes from `pkg/schema/testdata`,
//! decodes it, encodes it again and compares the JSON; it also fails when a `voice-*`
//! fixture exists that no type here claims.
//!
//! How strict each type is follows the schema, type by type:
//!
//! - Presence, the session response and the error document tolerate fields they do not
//!   know, as the Go client's decoding does (`TestVoicePresenceUnknownFieldsTolerated`):
//!   they are read by a client that may be older than its server.
//! - The transmit report rejects them at every depth, as `DecodeVoiceTransmitReportV1`
//!   does. This client only ever writes a report; the strictness is here so the copy is
//!   faithful.
//! - A field the schema always writes must be present. `conchd` never leaves one out, and
//!   reading a missing `can_publish` as false would be guessing.
//!
//! The `validate` methods make the checks the Go `Validate` methods make, in the same
//! order.

use std::fmt;

use serde::{Deserialize, Deserializer, Serialize, Serializer};

use crate::error::Error;

/// The value of `schema` in a v1 voice presence document.
pub const VOICE_PRESENCE_SCHEMA_V1: &str = "conch.voice_presence.v1";

/// The most principal ids a principals audience may list (`schema.MaxAudiencePrincipals`).
pub const MAX_AUDIENCE_PRINCIPALS: usize = 64;

/// A value that must never be printed: the login token, a join token, a room name.
///
/// `Debug` writes a fixed placeholder, and there is no `Display`, so a secret cannot reach
/// output through formatting. The only way to its text is [`Secret::expose`], which is
/// easy to find in review. It can be decoded from JSON; it can be encoded only in this
/// crate's own tests, so nothing built on this crate can serialise a session by accident.
#[derive(Clone, PartialEq, Eq, Deserialize)]
#[cfg_attr(test, derive(Serialize))]
#[serde(transparent)]
pub struct Secret(String);

impl Secret {
    /// Wraps a secret value.
    pub fn new(value: impl Into<String>) -> Self {
        Self(value.into())
    }

    /// The secret itself. Pass it to the one thing that needs it; never format it.
    pub fn expose(&self) -> &str {
        &self.0
    }

    /// Whether there is nothing in it.
    pub fn is_empty(&self) -> bool {
        self.0.is_empty()
    }
}

impl fmt::Debug for Secret {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("<redacted>")
    }
}

/// A wire timestamp: an instant in UTC with millisecond precision (`schema.Timestamp`).
///
/// It encodes as `2026-10-09T12:00:45.000Z`: always UTC, always three fractional digits,
/// always `Z`. Decoding accepts any RFC 3339 timestamp and brings it to that form, as the
/// Go type does, so decoding and encoding again is stable. There is no clock here: this
/// type does not know what time it is.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct Timestamp {
    unix_millis: i64,
}

/// Go's zero time, 0001-01-01T00:00:00Z, in milliseconds since the Unix epoch.
const ZERO_UNIX_MILLIS: i64 = -62_135_596_800_000;
const MILLIS_PER_DAY: i64 = 86_400_000;

impl Timestamp {
    /// Milliseconds since 1970-01-01T00:00:00Z.
    pub fn unix_millis(self) -> i64 {
        self.unix_millis
    }

    /// Whether this is Go's zero time, which the schema reads as "not set".
    pub fn is_zero(self) -> bool {
        self.unix_millis == ZERO_UNIX_MILLIS
    }

    /// Parses an RFC 3339 timestamp: `YYYY-MM-DDTHH:MM:SS`, an optional fraction, then `Z`
    /// or a `+HH:MM` or `-HH:MM` offset. Anything finer than a millisecond is dropped.
    pub fn parse(text: &str) -> Option<Self> {
        let b = text.as_bytes();
        if b.len() < 20 {
            return None;
        }
        let two = |at: usize| -> Option<i64> {
            match (b.get(at)?, b.get(at + 1)?) {
                (h, l) if h.is_ascii_digit() && l.is_ascii_digit() => {
                    Some(i64::from(h - b'0') * 10 + i64::from(l - b'0'))
                }
                _ => None,
            }
        };
        if b[4] != b'-' || b[7] != b'-' || b[10] != b'T' || b[13] != b':' || b[16] != b':' {
            return None;
        }
        let year = two(0)? * 100 + two(2)?;
        let (month, day) = (two(5)?, two(8)?);
        let (hour, minute, second) = (two(11)?, two(14)?, two(17)?);
        if !(1..=12).contains(&month)
            || !(1..=days_in_month(year, month)).contains(&day)
            || hour > 23
            || minute > 59
            || second > 59
        {
            return None;
        }

        let mut at = 19;
        let mut millis = 0;
        if b[at] == b'.' {
            at += 1;
            let digits = b[at..].iter().take_while(|c| c.is_ascii_digit()).count();
            if digits == 0 {
                return None;
            }
            for place in 0..3 {
                let digit = if place < digits {
                    b[at + place] - b'0'
                } else {
                    0
                };
                millis = millis * 10 + i64::from(digit);
            }
            at += digits;
        }

        let offset_minutes = match *b.get(at)? {
            b'Z' if at + 1 == b.len() => 0,
            sign @ (b'+' | b'-') if at + 6 == b.len() && b[at + 3] == b':' => {
                let (hours, minutes) = (two(at + 1)?, two(at + 4)?);
                if hours > 23 || minutes > 59 {
                    return None;
                }
                let offset = hours * 60 + minutes;
                if sign == b'-' { -offset } else { offset }
            }
            _ => return None,
        };

        let seconds =
            days_from_civil(year, month, day) * 86_400 + hour * 3600 + minute * 60 + second
                - offset_minutes * 60;
        let stamp = Self {
            unix_millis: seconds * 1000 + millis,
        };
        // An offset can carry the instant out of the years the wire form can write.
        (0..=9999).contains(&stamp.civil().0).then_some(stamp)
    }

    /// (year, month, day, hour, minute, second, millisecond), in UTC.
    fn civil(self) -> (i64, i64, i64, i64, i64, i64, i64) {
        let days = self.unix_millis.div_euclid(MILLIS_PER_DAY);
        let in_day = self.unix_millis.rem_euclid(MILLIS_PER_DAY);
        let (year, month, day) = civil_from_days(days);
        (
            year,
            month,
            day,
            in_day / 3_600_000,
            in_day / 60_000 % 60,
            in_day / 1000 % 60,
            in_day % 1000,
        )
    }
}

fn is_leap(year: i64) -> bool {
    (year % 4 == 0 && year % 100 != 0) || year % 400 == 0
}

fn days_in_month(year: i64, month: i64) -> i64 {
    match month {
        2 if is_leap(year) => 29,
        2 => 28,
        4 | 6 | 9 | 11 => 30,
        _ => 31,
    }
}

/// Days from 1970-01-01 to the given date in the proleptic Gregorian calendar.
fn days_from_civil(year: i64, month: i64, day: i64) -> i64 {
    // Count years from March, so the leap day is the last day of the year.
    let year = if month <= 2 { year - 1 } else { year };
    let era = year.div_euclid(400);
    let year_of_era = year.rem_euclid(400);
    let month_from_march = (month + 9) % 12;
    let day_of_year = (153 * month_from_march + 2) / 5 + day - 1;
    let day_of_era = year_of_era * 365 + year_of_era / 4 - year_of_era / 100 + day_of_year;
    era * 146_097 + day_of_era - 719_468
}

/// The inverse of [`days_from_civil`].
fn civil_from_days(days: i64) -> (i64, i64, i64) {
    let days = days + 719_468;
    let era = days.div_euclid(146_097);
    let day_of_era = days.rem_euclid(146_097);
    let year_of_era =
        (day_of_era - day_of_era / 1460 + day_of_era / 36_524 - day_of_era / 146_096) / 365;
    let day_of_year = day_of_era - (365 * year_of_era + year_of_era / 4 - year_of_era / 100);
    let month_from_march = (5 * day_of_year + 2) / 153;
    let day = day_of_year - (153 * month_from_march + 2) / 5 + 1;
    let month = if month_from_march < 10 {
        month_from_march + 3
    } else {
        month_from_march - 9
    };
    let year = year_of_era + era * 400 + i64::from(month <= 2);
    (year, month, day)
}

impl fmt::Display for Timestamp {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let (year, month, day, hour, minute, second, millis) = self.civil();
        write!(
            f,
            "{year:04}-{month:02}-{day:02}T{hour:02}:{minute:02}:{second:02}.{millis:03}Z"
        )
    }
}

impl fmt::Debug for Timestamp {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "Timestamp({self})")
    }
}

impl Serialize for Timestamp {
    fn serialize<S: Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        serializer.collect_str(self)
    }
}

impl<'de> Deserialize<'de> for Timestamp {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        let text = String::deserialize(deserializer)?;
        Self::parse(&text).ok_or_else(|| serde::de::Error::custom("invalid RFC 3339 timestamp"))
    }
}

/// Which of the two scoped audiences an [`Audience`] is (`schema.AudienceKind`). A kind
/// outside these two fails the whole document, as it does in Go: an audience a reader does
/// not understand must never widen what it shows (ADR-005).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AudienceKind {
    /// The members and monitors of one net.
    Net,
    /// An explicit list of principals.
    Principals,
}

/// The audience a room, a grant or a report is for (`schema.Audience`). Where one of these
/// is optional, absent means the whole channel.
///
/// The fields are the wire's, flat: exactly one of `net_id` and `principal_ids` is set,
/// chosen by `kind`. [`Audience::validate`] checks that.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Audience {
    /// Which shape this is.
    pub kind: AudienceKind,
    /// The net, for kind `net`; otherwise 0 and not written.
    #[serde(default, skip_serializing_if = "is_zero")]
    pub net_id: i64,
    /// The principals, for kind `principals`; otherwise empty and not written.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub principal_ids: Vec<i64>,
}

// serde's skip_serializing_if hands the field by reference.
#[allow(clippy::trivially_copy_pass_by_ref)]
fn is_zero(n: &i64) -> bool {
    *n == 0
}

impl Audience {
    /// The audience of one net.
    pub fn net(net_id: i64) -> Self {
        Self {
            kind: AudienceKind::Net,
            net_id,
            principal_ids: Vec::new(),
        }
    }

    /// An explicit list of principals.
    pub fn principals(principal_ids: Vec<i64>) -> Self {
        Self {
            kind: AudienceKind::Principals,
            net_id: 0,
            principal_ids,
        }
    }

    /// Checks what `schema.Audience.Validate` checks.
    pub fn validate(&self) -> Result<(), Error> {
        self.check().map_err(invalid("audience"))
    }

    fn check(&self) -> Result<(), String> {
        match self.kind {
            AudienceKind::Net => {
                if self.net_id <= 0 {
                    return Err(format!(
                        "audience net_id must be positive, got {}",
                        self.net_id
                    ));
                }
                if !self.principal_ids.is_empty() {
                    return Err("audience of kind net must not list principal_ids".into());
                }
            }
            AudienceKind::Principals => {
                if self.net_id != 0 {
                    return Err("audience of kind principals must not carry net_id".into());
                }
                let ids = &self.principal_ids;
                if ids.is_empty() {
                    return Err(
                        "audience of kind principals must list at least one principal_id".into(),
                    );
                }
                if ids.len() > MAX_AUDIENCE_PRINCIPALS {
                    return Err(format!(
                        "audience lists {} principal_ids, more than the maximum {MAX_AUDIENCE_PRINCIPALS}",
                        ids.len()
                    ));
                }
                let mut seen = std::collections::BTreeSet::new();
                for &id in ids {
                    if id <= 0 {
                        return Err(format!("audience principal_id must be positive, got {id}"));
                    }
                    if !seen.insert(id) {
                        return Err(format!("audience lists principal_id {id} more than once"));
                    }
                }
            }
        }
        Ok(())
    }
}

/// Names an audience for the one-room-per-audience rule (`audienceKey` in Go): the whole
/// channel, one net, or one set of principals whatever their order.
#[derive(PartialEq, Eq, PartialOrd, Ord)]
enum AudienceKey {
    Channel,
    Net(i64),
    Principals(Vec<i64>),
}

fn audience_key(audience: Option<&Audience>) -> AudienceKey {
    match audience {
        None => AudienceKey::Channel,
        Some(a) if a.kind == AudienceKind::Net => AudienceKey::Net(a.net_id),
        Some(a) => {
            let mut ids = a.principal_ids.clone();
            ids.sort_unstable();
            AudienceKey::Principals(ids)
        }
    }
}

fn invalid(what: &'static str) -> impl Fn(String) -> Error {
    move |problem| Error::Invalid { what, problem }
}

/// One room a voice session may join (`schema.VoiceRoomGrant`).
///
/// `room` and `token` are opaque and secret: this type's `Debug` shows neither.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
#[cfg_attr(test, derive(Serialize))]
pub struct VoiceRoomGrant {
    /// The LiveKit room name `conchd` created.
    pub room: Secret,
    /// The signed LiveKit access token for this one room.
    pub token: Secret,
    /// Whether the token lets the caller publish a microphone track.
    pub can_publish: bool,
    /// When the token stops starting connections. Short on purpose: it means "connect
    /// now", not a deadline to schedule against.
    pub expires_at: Timestamp,
    /// The audience the room carries; `None` is the whole channel.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub audience: Option<Audience>,
}

impl VoiceRoomGrant {
    fn check(&self) -> Result<(), String> {
        if self.room.is_empty() {
            return Err("voice room grant room is required".into());
        }
        if self.token.is_empty() {
            return Err("voice room grant token is required".into());
        }
        if self.expires_at.is_zero() {
            return Err("voice room grant expires_at is required".into());
        }
        match &self.audience {
            Some(audience) => audience.check(),
            None => Ok(()),
        }
    }
}

/// The body of `POST /v1/channels/{channel}/voice/session`: everything needed to connect
/// (`schema.VoiceSessionResponseV1`).
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
#[cfg_attr(test, derive(Serialize))]
pub struct VoiceSessionResponseV1 {
    /// The `ws://` or `wss://` address of LiveKit, exactly as the operator configured it.
    pub livekit_url: String,
    /// The caller's LiveKit participant identity, the same in every room.
    pub identity: String,
    /// One grant per room the caller may join; never empty.
    pub rooms: Vec<VoiceRoomGrant>,
}

impl VoiceSessionResponseV1 {
    /// Checks what `schema.VoiceSessionResponseV1.Validate` checks: an address and an
    /// identity, at least one grant, every grant with a room, a token and an expiry, and
    /// no two grants for the same audience.
    pub fn validate(&self) -> Result<(), Error> {
        self.check().map_err(invalid("voice session"))
    }

    fn check(&self) -> Result<(), String> {
        if self.livekit_url.is_empty() {
            return Err("voice session livekit_url is required".into());
        }
        if self.identity.is_empty() {
            return Err("voice session identity is required".into());
        }
        if self.rooms.is_empty() {
            return Err("voice session rooms must list at least one grant".into());
        }
        let mut seen = std::collections::BTreeSet::new();
        for grant in &self.rooms {
            grant.check()?;
            if !seen.insert(audience_key(grant.audience.as_ref())) {
                return Err("voice session lists more than one grant for the same audience".into());
            }
        }
        Ok(())
    }

    /// The grant for the channel-wide room: the one with no audience. A valid session has
    /// at most one.
    pub fn channel_grant(&self) -> Option<&VoiceRoomGrant> {
        self.rooms.iter().find(|grant| grant.audience.is_none())
    }
}

/// One principal connected to a voice room (`schema.VoiceParticipant`).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct VoiceParticipant {
    /// The connected principal.
    pub principal_id: i64,
    /// Whether the participant may transmit in this room.
    pub can_publish: bool,
    /// Whether the participant was talking on `conchd`'s last pass.
    pub transmitting: bool,
    /// When the participant connected.
    pub joined_at: Timestamp,
}

impl VoiceParticipant {
    fn check(&self) -> Result<(), String> {
        if self.principal_id <= 0 {
            return Err(format!(
                "voice participant principal_id must be positive, got {}",
                self.principal_id
            ));
        }
        if self.transmitting && !self.can_publish {
            return Err(format!(
                "voice participant {} cannot be transmitting without can_publish",
                self.principal_id
            ));
        }
        if self.joined_at.is_zero() {
            return Err("voice participant joined_at is required".into());
        }
        Ok(())
    }
}

/// One voice room as presence shows it: its audience and who is in it
/// (`schema.VoicePresenceRoom`). It has no room name and no token.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct VoicePresenceRoom {
    /// The audience the room carries; `None` is the whole channel.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub audience: Option<Audience>,
    /// Every principal connected to the room, ordered by id.
    pub participants: Vec<VoiceParticipant>,
}

impl VoicePresenceRoom {
    fn check(&self) -> Result<(), String> {
        if let Some(audience) = &self.audience {
            audience.check()?;
        }
        let mut seen = std::collections::BTreeSet::new();
        for participant in &self.participants {
            participant.check()?;
            if !seen.insert(participant.principal_id) {
                return Err(format!(
                    "voice room lists principal {} more than once",
                    participant.principal_id
                ));
            }
        }
        Ok(())
    }
}

/// The whole voice state of one channel (`schema.VoicePresenceV1`): the body of
/// `GET /v1/channels/{channel}/voice` and every frame of the presence socket. A snapshot
/// is the whole state, not a change to it.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct VoicePresenceV1 {
    /// Always [`VOICE_PRESENCE_SCHEMA_V1`].
    pub schema: String,
    /// The channel the snapshot describes.
    pub channel_id: i64,
    /// Whether this `conchd` has LiveKit configured at all.
    pub configured: bool,
    /// Whether LiveKit answered on the last pass. Never true when `configured` is false.
    pub available: bool,
    /// The rooms the caller may see. Empty unless `available`.
    pub rooms: Vec<VoicePresenceRoom>,
}

impl VoicePresenceV1 {
    /// Checks what `schema.VoicePresenceV1.Validate` checks: the schema name, a positive
    /// channel, `available` only when `configured`, no rooms unless `available`, every
    /// room well-formed, and no two rooms for the same audience.
    pub fn validate(&self) -> Result<(), Error> {
        self.check().map_err(invalid("voice presence"))
    }

    fn check(&self) -> Result<(), String> {
        if self.schema != VOICE_PRESENCE_SCHEMA_V1 {
            // The name it carries instead is the server's text and is not repeated.
            return Err(format!(
                "voice presence schema must be \"{VOICE_PRESENCE_SCHEMA_V1}\""
            ));
        }
        if self.channel_id <= 0 {
            return Err(format!(
                "voice presence channel_id must be positive, got {}",
                self.channel_id
            ));
        }
        if !self.configured && self.available {
            return Err("voice presence cannot be available when not configured".into());
        }
        if !self.available && !self.rooms.is_empty() {
            return Err("voice presence must list no rooms when not available".into());
        }
        let mut seen = std::collections::BTreeSet::new();
        for room in &self.rooms {
            room.check()?;
            if !seen.insert(audience_key(room.audience.as_ref())) {
                return Err("voice presence lists more than one room for the same audience".into());
            }
        }
        Ok(())
    }

    /// The channel-wide room: the one with no audience. A valid snapshot has at most one.
    pub fn channel_room(&self) -> Option<&VoicePresenceRoom> {
        self.rooms.iter().find(|room| room.audience.is_none())
    }
}

/// What a transmit report says: that the caller's transmission began or ended
/// (`schema.VoiceTransmitState`).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum VoiceTransmitState {
    /// The push-to-talk gate opened.
    Started,
    /// The gate shut.
    Stopped,
}

/// The body of `POST /v1/channels/{channel}/voice/transmit`
/// (`schema.VoiceTransmitReportV1`). It carries no time, no sequence number, and no
/// principal, room or token: `conchd` knows all of those itself.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct VoiceTransmitReportV1 {
    /// Started or stopped.
    pub state: VoiceTransmitState,
    /// The audience transmitted to; `None` is the whole channel.
    #[serde(
        default,
        skip_serializing_if = "Option::is_none",
        deserialize_with = "strict_audience"
    )]
    pub audience: Option<Audience>,
}

impl VoiceTransmitReportV1 {
    /// Checks what `schema.VoiceTransmitReportV1.Validate` checks beyond the state, which
    /// the type already limits to the two values: a well-formed audience, if there is one.
    pub fn validate(&self) -> Result<(), Error> {
        match &self.audience {
            Some(audience) => audience.check().map_err(invalid("voice transmit report")),
            None => Ok(()),
        }
    }
}

/// [`Audience`] as a transmit report must decode it: a field it does not declare is an
/// error, where everywhere else it is ignored.
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct StrictAudience {
    kind: AudienceKind,
    #[serde(default)]
    net_id: i64,
    #[serde(default)]
    principal_ids: Vec<i64>,
}

fn strict_audience<'de, D: Deserializer<'de>>(
    deserializer: D,
) -> Result<Option<Audience>, D::Error> {
    let strict = Option::<StrictAudience>::deserialize(deserializer)?;
    Ok(strict.map(|a| Audience {
        kind: a.kind,
        net_id: a.net_id,
        principal_ids: a.principal_ids,
    }))
}

/// The error document every REST endpoint answers a refusal with (`schema.Error`).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ErrorBody {
    /// A short machine-readable code, such as `voice_unavailable`.
    pub code: String,
    /// A sentence for a person. Server-supplied text: escape it before printing.
    pub message: String,
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;
    use std::path::{Path, PathBuf};

    use serde::de::DeserializeOwned;
    use serde_json::{Value, json};

    use super::*;

    /// `pkg/schema/testdata`, read in place: the fixtures are never copied into this crate.
    fn fixtures() -> PathBuf {
        Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../pkg/schema/testdata")
    }

    fn fixture(name: &str) -> Value {
        let path = fixtures().join(name);
        let text = std::fs::read_to_string(&path)
            .unwrap_or_else(|e| panic!("read {}: {e}", path.display()));
        serde_json::from_str(&text).unwrap_or_else(|e| panic!("parse {name}: {e}"))
    }

    /// Decodes a fixture into `T`, checks it with `validate`, encodes it again, and
    /// requires the JSON to equal the fixture.
    fn round_trip<T>(name: &str, validate: impl Fn(&T) -> Result<(), Error>)
    where
        T: DeserializeOwned + Serialize,
    {
        let want = fixture(name);
        let typed: T = serde_json::from_value(want.clone())
            .unwrap_or_else(|e| panic!("{name} does not decode: {e}"));
        validate(&typed).unwrap_or_else(|e| panic!("{name} does not validate: {e}"));
        let got = serde_json::to_value(&typed).unwrap();
        assert_eq!(got, want, "{name} did not survive decoding and encoding");
    }

    /// A fixture's file name and the check of the type that claims it.
    type Claim = (&'static str, fn(&str));

    /// Every fixture this crate claims, with the type that claims it.
    fn claimed() -> Vec<Claim> {
        fn session(name: &str) {
            round_trip::<VoiceSessionResponseV1>(name, VoiceSessionResponseV1::validate);
        }
        fn presence(name: &str) {
            round_trip::<VoicePresenceV1>(name, VoicePresenceV1::validate);
        }
        fn report(name: &str) {
            round_trip::<VoiceTransmitReportV1>(name, VoiceTransmitReportV1::validate);
        }
        fn error(name: &str) {
            round_trip::<ErrorBody>(name, |_| Ok(()));
        }
        vec![
            ("voice-session-response-v1.json", session),
            ("voice-session-response-v1-net.json", session),
            ("voice-presence-v1.json", presence),
            ("voice-presence-v1-not-configured.json", presence),
            ("voice-presence-v1-unavailable.json", presence),
            ("voice-transmit-report-v1-started.json", report),
            ("voice-transmit-report-v1-stopped.json", report),
            ("voice-transmit-report-v1-net.json", report),
            ("error.json", error),
        ]
    }

    #[test]
    fn every_claimed_fixture_round_trips_to_equal_json() {
        for (name, check) in claimed() {
            check(name);
        }
    }

    #[test]
    fn every_voice_fixture_in_pkg_schema_is_claimed_by_a_type() {
        let claimed: BTreeSet<String> = claimed().iter().map(|(n, _)| (*n).to_owned()).collect();
        let mut on_disk = BTreeSet::new();
        for entry in std::fs::read_dir(fixtures()).unwrap() {
            let name = entry.unwrap().file_name().into_string().unwrap();
            if name.starts_with("voice-") && name.ends_with(".json") {
                on_disk.insert(name);
            }
        }
        assert!(
            on_disk.len() >= 8,
            "expected the voice fixtures in {}, found {on_disk:?}",
            fixtures().display()
        );
        let unclaimed: Vec<_> = on_disk.difference(&claimed).collect();
        assert!(
            unclaimed.is_empty(),
            "pkg/schema/testdata has voice fixtures no Rust type claims: {unclaimed:?}. \
             Add the type (or the fixture to an existing one) in src/types.rs."
        );
    }

    #[test]
    fn the_fixtures_keep_the_details_the_schema_promises() {
        // can_publish: false is written out, never left for a reader to assume.
        let net: VoiceSessionResponseV1 =
            serde_json::from_value(fixture("voice-session-response-v1-net.json")).unwrap();
        let encoded = serde_json::to_value(&net).unwrap();
        assert_eq!(encoded["rooms"][1]["can_publish"], json!(false));
        assert_eq!(
            encoded["rooms"][1]["audience"],
            json!({"kind": "net", "net_id": 3})
        );
        // An absent audience stays absent: not null, not an empty object.
        assert!(encoded["rooms"][0].get("audience").is_none());
        assert!(net.rooms[0].audience.is_none());
        // The channel grant is the one with no audience.
        assert_eq!(
            net.channel_grant().unwrap().room.expose(),
            "conch-k3m7q2x9v4t1b8n6w5z0r2c4e6"
        );
        // Timestamps come back as the same string.
        assert_eq!(
            encoded["rooms"][0]["expires_at"],
            json!("2026-10-09T12:00:45.000Z")
        );

        // Empty lists are arrays, never null.
        let idle: VoicePresenceV1 =
            serde_json::from_value(fixture("voice-presence-v1-unavailable.json")).unwrap();
        assert_eq!(serde_json::to_value(&idle).unwrap()["rooms"], json!([]));
        let stopped: VoiceTransmitReportV1 =
            serde_json::from_value(fixture("voice-transmit-report-v1-stopped.json")).unwrap();
        assert_eq!(
            serde_json::to_string(&stopped).unwrap(),
            r#"{"state":"stopped"}"#
        );
    }

    #[test]
    fn a_secret_shows_a_placeholder_and_nothing_else() {
        let secret = Secret::new("conch_FAKE_do_not_print");
        assert_eq!(format!("{secret:?}"), "<redacted>");
        assert_eq!(format!("{secret:#?}"), "<redacted>");
        assert_eq!(secret.expose(), "conch_FAKE_do_not_print");
        assert!(!secret.is_empty() && Secret::new("").is_empty());
    }

    #[test]
    fn timestamps_decode_to_the_canonical_form() {
        // (input, the form it is written back in)
        let table = [
            ("2026-10-09T12:00:45.000Z", "2026-10-09T12:00:45.000Z"),
            ("2026-10-09T11:58:02.250Z", "2026-10-09T11:58:02.250Z"),
            ("2026-10-09T12:00:45Z", "2026-10-09T12:00:45.000Z"),
            ("2026-10-09T12:00:45.5Z", "2026-10-09T12:00:45.500Z"),
            ("2026-10-09T12:00:45.123456789Z", "2026-10-09T12:00:45.123Z"),
            ("2026-10-09T12:00:45.9999Z", "2026-10-09T12:00:45.999Z"),
            ("2026-10-09T14:00:45+02:00", "2026-10-09T12:00:45.000Z"),
            ("2026-10-09T06:30:45.25-05:30", "2026-10-09T12:00:45.250Z"),
            ("2026-12-31T23:59:59.999-00:01", "2027-01-01T00:00:59.999Z"),
            ("2024-02-29T00:00:00Z", "2024-02-29T00:00:00.000Z"),
            ("2000-02-29T23:59:59Z", "2000-02-29T23:59:59.000Z"),
            ("1970-01-01T00:00:00Z", "1970-01-01T00:00:00.000Z"),
            ("1969-12-31T23:59:59.999Z", "1969-12-31T23:59:59.999Z"),
            ("0001-01-01T00:00:00Z", "0001-01-01T00:00:00.000Z"),
            ("9999-12-31T23:59:59.999Z", "9999-12-31T23:59:59.999Z"),
        ];
        for (input, want) in table {
            let stamp = Timestamp::parse(input).unwrap_or_else(|| panic!("{input} rejected"));
            assert_eq!(stamp.to_string(), want, "{input}");
            // The canonical form is a fixed point.
            assert_eq!(Timestamp::parse(want).unwrap(), stamp, "{input}");
            assert_eq!(serde_json::to_value(stamp).unwrap(), json!(want));
        }
    }

    #[test]
    fn timestamps_know_their_instant() {
        let table = [
            ("1970-01-01T00:00:00Z", 0),
            ("1970-01-01T00:00:00.001Z", 1),
            ("1969-12-31T23:59:59.999Z", -1),
            ("2026-10-09T12:00:45.000Z", 1_791_547_245_000),
            ("2000-03-01T00:00:00Z", 951_868_800_000),
            ("0001-01-01T00:00:00Z", ZERO_UNIX_MILLIS),
        ];
        for (input, want) in table {
            assert_eq!(
                Timestamp::parse(input).unwrap().unix_millis(),
                want,
                "{input}"
            );
        }
        assert!(Timestamp::parse("0001-01-01T00:00:00Z").unwrap().is_zero());
        assert!(
            !Timestamp::parse("2026-10-09T12:00:45.000Z")
                .unwrap()
                .is_zero()
        );
        let earlier = Timestamp::parse("2026-10-09T11:58:02.250Z").unwrap();
        let later = Timestamp::parse("2026-10-09T12:00:31.000Z").unwrap();
        assert!(earlier < later);
    }

    #[test]
    fn malformed_timestamps_are_rejected() {
        for input in [
            "",
            "2026-10-09",
            "2026-10-09T12:00:45",
            "2026-10-09 12:00:45Z",
            "2026-10-09t12:00:45Z",
            "2026-10-09T12:00:45z",
            "2026-10-09T12:00:45.Z",
            "2026-10-09T12:00:45,000Z",
            "2026-13-09T12:00:45Z",
            "2026-00-09T12:00:45Z",
            "2026-02-29T12:00:45Z",
            "2026-04-31T12:00:45Z",
            "2026-10-09T24:00:45Z",
            "2026-10-09T12:60:45Z",
            "2026-10-09T12:00:60Z",
            "2026-10-09T12:00:45+0200",
            "2026-10-09T12:00:45+24:00",
            "2026-10-09T12:00:45+02:60",
            "2026-10-09T12:00:45ZZ",
            "2026-10-09T12:00:45Z ",
            " 2026-10-09T12:00:45Z",
            "２026-10-09T12:00:45Z",
            "0000-01-01T00:00:00+00:01",
            "9999-12-31T23:59:59-00:01",
        ] {
            assert!(Timestamp::parse(input).is_none(), "{input:?} accepted");
        }
        assert!(serde_json::from_value::<Timestamp>(json!(1_791_547_245)).is_err());
        assert!(serde_json::from_value::<Timestamp>(json!(null)).is_err());
    }

    #[test]
    fn days_and_dates_agree_over_four_centuries() {
        // Every day of a whole 400-year cycle, plus the years around the epoch and year 1.
        let mut previous = days_from_civil(1599, 12, 31);
        for year in 1600..=2000 {
            for month in 1..=12 {
                for day in 1..=days_in_month(year, month) {
                    let days = days_from_civil(year, month, day);
                    assert_eq!(days, previous + 1, "{year}-{month}-{day}");
                    assert_eq!(civil_from_days(days), (year, month, day));
                    previous = days;
                }
            }
        }
        assert_eq!(days_from_civil(1970, 1, 1), 0);
        assert_eq!(civil_from_days(-719_162), (1, 1, 1));
    }

    fn grant(audience: Option<Audience>) -> VoiceRoomGrant {
        VoiceRoomGrant {
            room: Secret::new("conch-FAKE-room"),
            token: Secret::new("FAKE-join-token"),
            can_publish: true,
            expires_at: Timestamp::parse("2026-10-09T12:00:45.000Z").unwrap(),
            audience,
        }
    }

    fn session(rooms: Vec<VoiceRoomGrant>) -> VoiceSessionResponseV1 {
        VoiceSessionResponseV1 {
            livekit_url: "wss://voice.example".into(),
            identity: "p7".into(),
            rooms,
        }
    }

    fn problem(result: Result<(), Error>) -> String {
        match result {
            Err(Error::Invalid { problem, .. }) => problem,
            other => panic!("expected Error::Invalid, got {other:?}"),
        }
    }

    #[test]
    fn a_session_is_validated_as_the_schema_validates_it() {
        let zero = Timestamp::parse("0001-01-01T00:00:00Z").unwrap();
        let table: Vec<(&str, VoiceSessionResponseV1, Option<&str>)> = vec![
            ("one channel grant", session(vec![grant(None)]), None),
            (
                "a channel grant and a net grant",
                session(vec![grant(None), grant(Some(Audience::net(3)))]),
                None,
            ),
            (
                "two nets",
                session(vec![
                    grant(Some(Audience::net(3))),
                    grant(Some(Audience::net(4))),
                ]),
                None,
            ),
            (
                "no address",
                VoiceSessionResponseV1 {
                    livekit_url: String::new(),
                    ..session(vec![grant(None)])
                },
                Some("livekit_url is required"),
            ),
            (
                "no identity",
                VoiceSessionResponseV1 {
                    identity: String::new(),
                    ..session(vec![grant(None)])
                },
                Some("identity is required"),
            ),
            ("no grants", session(vec![]), Some("at least one grant")),
            (
                "a grant with no room",
                session(vec![VoiceRoomGrant {
                    room: Secret::new(""),
                    ..grant(None)
                }]),
                Some("room is required"),
            ),
            (
                "a grant with no token",
                session(vec![VoiceRoomGrant {
                    token: Secret::new(""),
                    ..grant(None)
                }]),
                Some("token is required"),
            ),
            (
                "a grant with no expiry",
                session(vec![VoiceRoomGrant {
                    expires_at: zero,
                    ..grant(None)
                }]),
                Some("expires_at is required"),
            ),
            (
                "a grant with a malformed audience",
                session(vec![grant(Some(Audience::net(0)))]),
                Some("net_id must be positive"),
            ),
            (
                "two channel grants",
                session(vec![grant(None), grant(None)]),
                Some("more than one grant for the same audience"),
            ),
            (
                "two grants for one net",
                session(vec![
                    grant(Some(Audience::net(3))),
                    grant(Some(Audience::net(3))),
                ]),
                Some("more than one grant for the same audience"),
            ),
            (
                "two grants for one set of principals in another order",
                session(vec![
                    grant(Some(Audience::principals(vec![3, 7]))),
                    grant(Some(Audience::principals(vec![7, 3]))),
                ]),
                Some("more than one grant for the same audience"),
            ),
        ];
        for (name, value, want) in table {
            match want {
                None => value.validate().unwrap_or_else(|e| panic!("{name}: {e}")),
                Some(want) => {
                    let got = problem(value.validate());
                    assert!(got.contains(want), "{name}: {got}");
                }
            }
        }
    }

    #[test]
    fn an_audience_is_validated_as_the_schema_validates_it() {
        let table: Vec<(&str, Audience, Option<&str>)> = vec![
            ("a net", Audience::net(3), None),
            ("principals", Audience::principals(vec![3, 4, 7]), None),
            (
                "net zero",
                Audience::net(0),
                Some("net_id must be positive"),
            ),
            (
                "a negative net",
                Audience::net(-1),
                Some("net_id must be positive"),
            ),
            (
                "a net with principals",
                Audience {
                    principal_ids: vec![3],
                    ..Audience::net(3)
                },
                Some("must not list principal_ids"),
            ),
            (
                "principals with a net",
                Audience {
                    net_id: 3,
                    ..Audience::principals(vec![3])
                },
                Some("must not carry net_id"),
            ),
            (
                "no principals",
                Audience::principals(vec![]),
                Some("at least one principal_id"),
            ),
            (
                "too many principals",
                Audience::principals((1..=65).collect()),
                Some("more than the maximum 64"),
            ),
            (
                "exactly the most principals",
                Audience::principals((1..=64).collect()),
                None,
            ),
            (
                "a principal that is not positive",
                Audience::principals(vec![3, 0]),
                Some("principal_id must be positive"),
            ),
            (
                "a principal twice",
                Audience::principals(vec![3, 4, 3]),
                Some("principal_id 3 more than once"),
            ),
        ];
        for (name, value, want) in table {
            match want {
                None => value.validate().unwrap_or_else(|e| panic!("{name}: {e}")),
                Some(want) => {
                    let got = problem(value.validate());
                    assert!(got.contains(want), "{name}: {got}");
                }
            }
        }
        // A kind outside the two fails at decoding, so it fails the whole document.
        assert!(
            serde_json::from_value::<Audience>(json!({"kind": "role", "role": "admin"})).is_err()
        );
        assert!(serde_json::from_value::<Audience>(json!({"kind": "Net", "net_id": 3})).is_err());
    }

    fn participant(principal_id: i64) -> VoiceParticipant {
        VoiceParticipant {
            principal_id,
            can_publish: true,
            transmitting: false,
            joined_at: Timestamp::parse("2026-10-09T12:00:31.000Z").unwrap(),
        }
    }

    fn presence(rooms: Vec<VoicePresenceRoom>) -> VoicePresenceV1 {
        VoicePresenceV1 {
            schema: VOICE_PRESENCE_SCHEMA_V1.into(),
            channel_id: 7,
            configured: true,
            available: true,
            rooms,
        }
    }

    fn room(audience: Option<Audience>, participants: Vec<VoiceParticipant>) -> VoicePresenceRoom {
        VoicePresenceRoom {
            audience,
            participants,
        }
    }

    #[test]
    fn presence_is_validated_as_the_schema_validates_it() {
        let zero = Timestamp::parse("0001-01-01T00:00:00Z").unwrap();
        let table: Vec<(&str, VoicePresenceV1, Option<&str>)> = vec![
            ("nobody connected", presence(vec![room(None, vec![])]), None),
            ("no rooms at all", presence(vec![]), None),
            (
                "two people, one talking",
                presence(vec![room(
                    None,
                    vec![
                        VoiceParticipant {
                            transmitting: true,
                            ..participant(3)
                        },
                        participant(7),
                    ],
                )]),
                None,
            ),
            (
                "a channel room and a net room",
                presence(vec![
                    room(None, vec![participant(3)]),
                    room(Some(Audience::net(3)), vec![participant(3)]),
                ]),
                None,
            ),
            (
                "not configured",
                VoicePresenceV1 {
                    configured: false,
                    available: false,
                    ..presence(vec![])
                },
                None,
            ),
            (
                "another schema name",
                VoicePresenceV1 {
                    schema: "conch.voice_presence.v2".into(),
                    ..presence(vec![])
                },
                Some("schema must be"),
            ),
            (
                "no schema name",
                VoicePresenceV1 {
                    schema: String::new(),
                    ..presence(vec![])
                },
                Some("schema must be"),
            ),
            (
                "channel zero",
                VoicePresenceV1 {
                    channel_id: 0,
                    ..presence(vec![])
                },
                Some("channel_id must be positive"),
            ),
            (
                "available but not configured",
                VoicePresenceV1 {
                    configured: false,
                    ..presence(vec![])
                },
                Some("cannot be available when not configured"),
            ),
            (
                "rooms while unavailable",
                VoicePresenceV1 {
                    available: false,
                    ..presence(vec![room(None, vec![])])
                },
                Some("no rooms when not available"),
            ),
            (
                "a participant with no id",
                presence(vec![room(None, vec![participant(0)])]),
                Some("principal_id must be positive"),
            ),
            (
                "transmitting without being able to publish",
                presence(vec![room(
                    None,
                    vec![VoiceParticipant {
                        can_publish: false,
                        transmitting: true,
                        ..participant(3)
                    }],
                )]),
                Some("cannot be transmitting without can_publish"),
            ),
            (
                "a participant with no join time",
                presence(vec![room(
                    None,
                    vec![VoiceParticipant {
                        joined_at: zero,
                        ..participant(3)
                    }],
                )]),
                Some("joined_at is required"),
            ),
            (
                "a principal twice in one room",
                presence(vec![room(None, vec![participant(3), participant(3)])]),
                Some("principal 3 more than once"),
            ),
            (
                "two channel rooms",
                presence(vec![room(None, vec![]), room(None, vec![])]),
                Some("more than one room for the same audience"),
            ),
            (
                "a room with a malformed audience",
                presence(vec![room(Some(Audience::principals(vec![])), vec![])]),
                Some("at least one principal_id"),
            ),
        ];
        for (name, value, want) in table {
            match want {
                None => value.validate().unwrap_or_else(|e| panic!("{name}: {e}")),
                Some(want) => {
                    let got = problem(value.validate());
                    assert!(got.contains(want), "{name}: {got}");
                }
            }
        }
    }

    #[test]
    fn presence_and_sessions_tolerate_fields_they_do_not_know() {
        let mut doc = fixture("voice-presence-v1.json");
        doc["later_field"] = json!({"anything": [1, 2, 3]});
        doc["rooms"][0]["later_field"] = json!(true);
        doc["rooms"][0]["participants"][0]["later_field"] = json!("x");
        let decoded: VoicePresenceV1 = serde_json::from_value(doc).unwrap();
        decoded.validate().unwrap();
        assert_eq!(
            serde_json::to_value(&decoded).unwrap(),
            fixture("voice-presence-v1.json")
        );

        let mut doc = fixture("voice-session-response-v1-net.json");
        doc["later_field"] = json!(1);
        doc["rooms"][1]["later_field"] = json!(1);
        doc["rooms"][1]["audience"]["later_field"] = json!(1);
        let decoded: VoiceSessionResponseV1 = serde_json::from_value(doc).unwrap();
        decoded.validate().unwrap();

        let decoded: ErrorBody =
            serde_json::from_value(json!({"code": "c", "message": "m", "request_id": "r"}))
                .unwrap();
        assert_eq!(decoded.code, "c");
    }

    #[test]
    fn a_field_the_schema_always_writes_must_be_there() {
        for missing in ["schema", "channel_id", "configured", "available", "rooms"] {
            let mut doc = fixture("voice-presence-v1.json");
            doc.as_object_mut().unwrap().remove(missing);
            assert!(
                serde_json::from_value::<VoicePresenceV1>(doc).is_err(),
                "presence without {missing}"
            );
        }
        for missing in ["principal_id", "can_publish", "transmitting", "joined_at"] {
            let mut doc = fixture("voice-presence-v1.json");
            doc["rooms"][0]["participants"][0]
                .as_object_mut()
                .unwrap()
                .remove(missing);
            assert!(
                serde_json::from_value::<VoicePresenceV1>(doc).is_err(),
                "participant without {missing}"
            );
        }
        for missing in ["room", "token", "can_publish", "expires_at"] {
            let mut doc = fixture("voice-session-response-v1.json");
            doc["rooms"][0].as_object_mut().unwrap().remove(missing);
            assert!(
                serde_json::from_value::<VoiceSessionResponseV1>(doc).is_err(),
                "grant without {missing}"
            );
        }
        let mut doc = fixture("voice-presence-v1.json");
        doc["rooms"] = Value::Null;
        assert!(
            serde_json::from_value::<VoicePresenceV1>(doc).is_err(),
            "rooms: null"
        );
    }

    #[test]
    fn a_transmit_report_rejects_fields_it_does_not_declare_at_any_depth() {
        for body in [
            json!({"state": "started", "at": "2026-10-09T12:00:45.000Z"}),
            json!({"state": "started", "seq": 4}),
            json!({"state": "started", "principal_id": 7}),
            json!({"state": "started", "room": "conch-FAKE-room"}),
            json!({"state": "started", "token": "FAKE-join-token"}),
            json!({"state": "started", "audience": {"kind": "net", "net_id": 3, "room": "x"}}),
            json!({"state": "Started"}),
            json!({"state": "paused"}),
            json!({}),
        ] {
            assert!(
                serde_json::from_value::<VoiceTransmitReportV1>(body.clone()).is_err(),
                "{body} accepted"
            );
        }
        // null for audience is the same as leaving it out.
        let report: VoiceTransmitReportV1 =
            serde_json::from_value(json!({"state": "stopped", "audience": null})).unwrap();
        assert_eq!(
            report,
            VoiceTransmitReportV1 {
                state: VoiceTransmitState::Stopped,
                audience: None
            }
        );

        let report = VoiceTransmitReportV1 {
            state: VoiceTransmitState::Started,
            audience: Some(Audience::net(0)),
        };
        assert!(problem(report.validate()).contains("net_id must be positive"));
    }

    #[test]
    fn presence_carries_no_token_and_no_room_name() {
        fn keys(value: &Value, into: &mut BTreeSet<String>) {
            match value {
                Value::Object(map) => {
                    for (key, inner) in map {
                        into.insert(key.clone());
                        keys(inner, into);
                    }
                }
                Value::Array(items) => items.iter().for_each(|item| keys(item, into)),
                _ => {}
            }
        }
        let full = presence(vec![
            room(None, vec![participant(3)]),
            room(Some(Audience::net(3)), vec![participant(3)]),
            room(Some(Audience::principals(vec![3, 7])), vec![participant(7)]),
        ]);
        let mut seen = BTreeSet::new();
        keys(&serde_json::to_value(&full).unwrap(), &mut seen);
        for forbidden in ["token", "room", "room_name"] {
            assert!(
                !seen.contains(forbidden),
                "presence encodes a {forbidden} field"
            );
        }
    }
}
