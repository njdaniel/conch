//! The device list `conch-voice devices --json` writes (`schema.VoiceDevicesV1`,
//! `pkg/schema/voice_devices.go`; issues #649 and #650).
//!
//! The helper only writes it, so these types are encoded by serde's derive and decoded by
//! the same strict reader the other copies use. The decoding is here so that the golden
//! fixtures are read and written by a Rust type, and so that a test can check that what the
//! helper builds is what the Go reader accepts. [`VoiceDevicesV1::validate`] makes the
//! checks `schema.VoiceDevicesV1.Validate` makes, so the helper can check its own document
//! before it writes it.
//!
//! The document carries device-supplied text (names and descriptions) and nothing of
//! conch-voice's own: no path, no token, no server address.

use serde::{Deserialize, Deserializer, Serialize};

use crate::error::Error;
use crate::types::{invalid, wire_object, wire_words};

/// The value of `schema` in a v1 device list.
pub const VOICE_DEVICES_SCHEMA_V1: &str = "conch.voice_devices.v1";

/// The most devices either list holds (`schema.VoiceDevicesMaxListed`). Any beyond it are
/// counted in `unshown_*`.
pub const VOICE_DEVICES_MAX_LISTED: usize = 64;

/// The longest device name, in bytes (`schema.VoiceDevicesMaxNameBytes`).
pub const VOICE_DEVICES_MAX_NAME_BYTES: usize = 255;

/// The longest description, in characters (`schema.VoiceDevicesMaxDescriptionChars`).
pub const VOICE_DEVICES_MAX_DESCRIPTION_CHARS: usize = 80;

/// Whether PipeWire could be reached to list devices.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum VoiceDevicesPipeWire {
    /// PipeWire was reached and the lists are its.
    Ok,
    /// PipeWire could not be reached; both lists are empty and both counts zero.
    Unavailable,
}

wire_words!(VoiceDevicesPipeWire, "a voice devices pipewire state", {
    "ok" => Ok,
    "unavailable" => Unavailable,
});

/// What became of `voice.toml`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum VoiceDevicesConfig {
    /// The file was read.
    Ok,
    /// There is a configuration directory and no `voice.toml` in it.
    Missing,
    /// `voice.toml` is there and was refused.
    Refused,
    /// There is no configuration directory, so there is no file to read.
    None,
}

wire_words!(VoiceDevicesConfig, "a voice devices config state", {
    "ok" => Ok,
    "missing" => Missing,
    "refused" => Refused,
    "none" => None,
});

/// One microphone or speaker (`schema.VoiceDevice`).
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceDevice {
    /// The PipeWire `node.name`, which is what `[audio] input` or `output` takes.
    pub name: String,
    /// The device's own description; empty when it has none worth showing.
    pub description: String,
    /// Whether this is PipeWire's default for its kind.
    pub default: bool,
}

wire_object!(VoiceDevice, "a voice device", unknown = ignored, {
    name: required,
    description: required,
    default: required,
});

/// The device list (`schema.VoiceDevicesV1`).
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceDevicesV1 {
    /// Always [`VOICE_DEVICES_SCHEMA_V1`].
    pub schema: String,
    /// Whether PipeWire could be reached.
    pub pipewire: VoiceDevicesPipeWire,
    /// Whether PipeWire finished listing its devices within the wait.
    pub synced: bool,
    /// Every source fit to show, at most [`VOICE_DEVICES_MAX_LISTED`].
    pub microphones: Vec<VoiceDevice>,
    /// Every sink fit to show, at most [`VOICE_DEVICES_MAX_LISTED`].
    pub speakers: Vec<VoiceDevice>,
    /// How many sources are not in `microphones`.
    pub unshown_microphones: usize,
    /// How many sinks are not in `speakers`.
    pub unshown_speakers: usize,
    /// What became of `voice.toml`.
    pub config: VoiceDevicesConfig,
    /// `[audio] input` when the file was read and names a device; absent for PipeWire's
    /// default.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub configured_input: Option<String>,
    /// `[audio] output`, by the same rules.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub configured_output: Option<String>,
}

wire_object!(VoiceDevicesV1, "a voice devices document", unknown = ignored, {
    schema: required,
    pipewire: required,
    synced: required,
    microphones: required,
    speakers: required,
    unshown_microphones: required,
    unshown_speakers: required,
    config: required,
    configured_input: maybe,
    configured_output: maybe,
});

/// Whether `name` can be written into `voice.toml` between quotes as it stands: 1 to
/// [`VOICE_DEVICES_MAX_NAME_BYTES`] bytes, no control character, no `"` and no `\`.
#[must_use]
pub fn is_writable_device_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= VOICE_DEVICES_MAX_NAME_BYTES
        && !name.chars().any(char::is_control)
        && !name.contains(['"', '\\'])
}

impl VoiceDevicesV1 {
    /// Checks what `schema.VoiceDevicesV1.Validate` checks: the schema name, both lists no
    /// longer than the most, every device under the name and description rules, nothing
    /// listed or counted while PipeWire is unavailable, and `configured_*` only while the
    /// file was read, under the name rule, and never the value `default`.
    ///
    /// # Errors
    ///
    /// [`Error::Invalid`], saying which rule; it never repeats a name or a description.
    pub fn validate(&self) -> Result<(), Error> {
        self.check().map_err(invalid("voice devices"))
    }

    fn check(&self) -> Result<(), String> {
        if self.schema != VOICE_DEVICES_SCHEMA_V1 {
            return Err("voice devices schema is not conch.voice_devices.v1".into());
        }
        check_list("microphones", &self.microphones)?;
        check_list("speakers", &self.speakers)?;
        if self.pipewire == VoiceDevicesPipeWire::Unavailable
            && (!self.microphones.is_empty()
                || !self.speakers.is_empty()
                || self.unshown_microphones != 0
                || self.unshown_speakers != 0)
        {
            return Err(
                "voice devices must list and count nothing while pipewire is unavailable".into(),
            );
        }
        for (field, value) in [
            ("configured_input", &self.configured_input),
            ("configured_output", &self.configured_output),
        ] {
            let Some(value) = value else { continue };
            if self.config != VoiceDevicesConfig::Ok {
                return Err(format!("voice devices {field} is only for config ok"));
            }
            if !is_writable_device_name(value) {
                return Err(format!(
                    "voice devices {field} is not a name voice.toml can hold"
                ));
            }
            if value == "default" {
                return Err(format!(
                    "voice devices {field} says the default by absence, not by a value"
                ));
            }
        }
        Ok(())
    }
}

fn check_list(field: &str, list: &[VoiceDevice]) -> Result<(), String> {
    if list.len() > VOICE_DEVICES_MAX_LISTED {
        return Err(format!(
            "voice devices {field} lists more than {VOICE_DEVICES_MAX_LISTED} devices"
        ));
    }
    for device in list {
        if !is_writable_device_name(&device.name) {
            return Err(format!(
                "voice devices {field} has a name voice.toml cannot hold"
            ));
        }
        if device.description.chars().count() > VOICE_DEVICES_MAX_DESCRIPTION_CHARS
            || device.description.chars().any(char::is_control)
        {
            return Err(format!(
                "voice devices {field} has a description that is too long or not one line"
            ));
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::path::{Path, PathBuf};

    use super::*;

    /// `pkg/schema/testdata/voice-devices`, read in place.
    fn fixtures() -> PathBuf {
        Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../pkg/schema/testdata/voice-devices")
    }

    fn fixture_text(name: &str) -> String {
        let path = fixtures().join(name);
        std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {}: {e}", path.display()))
    }

    /// JSON text with the white space outside its strings taken out: Go's indented
    /// encoding, made into the compact line the helper writes.
    fn compact(text: &str) -> String {
        let mut out = String::with_capacity(text.len());
        let (mut in_string, mut escaped) = (false, false);
        for c in text.chars() {
            if in_string {
                out.push(c);
                match (escaped, c) {
                    (true, _) => escaped = false,
                    (false, '\\') => escaped = true,
                    (false, '"') => in_string = false,
                    _ => {}
                }
            } else if c == '"' {
                in_string = true;
                out.push(c);
            } else if !c.is_whitespace() {
                out.push(c);
            }
        }
        out
    }

    /// Every golden document; `cases.json` and `sensitive.json` are the reader's.
    const GOLDEN: [&str; 7] = [
        "voice-devices-v1.json",
        "voice-devices-v1-config-none.json",
        "voice-devices-v1-config-refused.json",
        "voice-devices-v1-configured-input-absent.json",
        "voice-devices-v1-pipewire-unavailable.json",
        "voice-devices-v1-unshown.json",
        "voice-devices-v1-virtual-microphone.json",
    ];

    #[test]
    fn every_golden_document_round_trips_byte_for_byte() {
        for name in GOLDEN {
            let text = fixture_text(name);
            let typed: VoiceDevicesV1 = serde_json::from_str(&text)
                .unwrap_or_else(|e| panic!("{name} does not decode: {e}"));
            typed
                .validate()
                .unwrap_or_else(|e| panic!("{name} does not validate: {e}"));
            // The Go fixtures are indented; the helper writes the same fields compactly,
            // in the same order.
            assert_eq!(
                serde_json::to_string(&typed).unwrap(),
                compact(&text),
                "{name}"
            );
        }
    }

    #[test]
    fn every_fixture_in_the_directory_is_claimed() {
        let mut unclaimed = Vec::new();
        for entry in std::fs::read_dir(fixtures()).unwrap() {
            let name = entry.unwrap().file_name().into_string().unwrap();
            let known =
                GOLDEN.contains(&name.as_str()) || name == "cases.json" || name == "sensitive.json";
            if !known {
                unclaimed.push(name);
            }
        }
        assert!(unclaimed.is_empty(), "no test here reads {unclaimed:?}");
    }

    #[test]
    fn no_field_has_a_name_the_schema_forbids() {
        #[derive(Deserialize)]
        struct Sensitive {
            forbidden_field_fragments: Vec<String>,
        }
        let sensitive: Sensitive = serde_json::from_str(&fixture_text("sensitive.json")).unwrap();
        let document: serde_json::Value =
            serde_json::from_str(&fixture_text("voice-devices-v1.json")).unwrap();
        let mut names: Vec<String> = document.as_object().unwrap().keys().cloned().collect();
        names.extend(["name".into(), "description".into(), "default".into()]);
        names.extend(["configured_input".into(), "configured_output".into()]);
        for name in names {
            for fragment in &sensitive.forbidden_field_fragments {
                assert!(!name.contains(fragment.as_str()), "{name} holds {fragment}");
            }
        }
    }

    #[test]
    fn a_document_is_read_as_the_reader_reads_it() {
        let mut doc: serde_json::Value =
            serde_json::from_str(&fixture_text("voice-devices-v1.json")).unwrap();
        // A field this version does not declare is ignored.
        doc["keyboards"] = serde_json::json!([]);
        assert!(serde_json::from_value::<VoiceDevicesV1>(doc.clone()).is_ok());
        // A declared field missing, or null where it may be absent, is not read.
        for field in ["pipewire", "synced", "microphones", "config"] {
            let mut without = doc.clone();
            without.as_object_mut().unwrap().remove(field);
            assert!(
                serde_json::from_value::<VoiceDevicesV1>(without).is_err(),
                "{field}"
            );
        }
        doc["configured_input"] = serde_json::Value::Null;
        assert!(serde_json::from_value::<VoiceDevicesV1>(doc).is_err());
    }

    #[test]
    fn validate_refuses_what_the_go_validator_refuses() {
        let good: VoiceDevicesV1 =
            serde_json::from_str(&fixture_text("voice-devices-v1.json")).unwrap();
        let mut cases: Vec<(&str, VoiceDevicesV1)> = Vec::new();
        let mut c = good.clone();
        c.microphones[0].name = "a\"b".into();
        cases.push(("a quotation mark in a name", c));
        let mut c = good.clone();
        c.microphones[0].name = "a".repeat(VOICE_DEVICES_MAX_NAME_BYTES + 1);
        cases.push(("a name over the most bytes", c));
        let mut c = good.clone();
        c.speakers[0].description = "x".repeat(VOICE_DEVICES_MAX_DESCRIPTION_CHARS + 1);
        cases.push(("a long description", c));
        let mut c = good.clone();
        c.speakers[0].description = "a\nb".into();
        cases.push(("a description of two lines", c));
        let mut c = good.clone();
        c.microphones = vec![good.microphones[0].clone(); VOICE_DEVICES_MAX_LISTED + 1];
        cases.push(("too many devices", c));
        let mut c = good.clone();
        c.pipewire = VoiceDevicesPipeWire::Unavailable;
        cases.push(("devices while unavailable", c));
        let mut c = good.clone();
        c.config = VoiceDevicesConfig::Missing;
        cases.push(("configured_input while config is not ok", c));
        let mut c = good.clone();
        c.configured_input = Some("default".into());
        cases.push(("the default said by a value", c));
        let mut c = good;
        c.schema = "conch.voice_devices.v2".into();
        cases.push(("another schema", c));
        for (what, document) in cases {
            assert!(document.validate().is_err(), "{what}");
        }
    }
}
