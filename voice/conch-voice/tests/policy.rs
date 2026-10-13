//! How what the SDK and `conchd` say is mapped onto the connection policy's outcomes, and
//! what the policy then says: every row of both tables of `docs/design/conch-voice.md` §5.

#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::time::Duration;

use conch_voice::sdk::DisconnectReason;
use conch_voice::session::{disconnect_outcome, session_outcome};
use conch_voice_api::{Error as ApiError, Refusal};
use conch_voice_control::{ConnectionPolicy, NextStep, Outcome};

fn refusal(status: u16, code: &str) -> Refusal {
    Refusal {
        status,
        code: code.to_owned(),
        message: "said the server".to_owned(),
    }
}

/// What a new policy says about `outcome`: "stop", "ask now" or "wait".
fn first_step(outcome: Outcome) -> &'static str {
    match ConnectionPolicy::new().next(Duration::ZERO, outcome, 0) {
        NextStep::Stop { exit_code, .. } => {
            assert_ne!(exit_code, 0);
            "stop"
        }
        NextStep::AskNow => "ask now",
        NextStep::RetryAfter(wait) => {
            assert!(wait >= Duration::from_millis(500) && wait <= Duration::from_secs(1));
            "wait"
        }
    }
}

#[test]
fn each_reason_the_sdk_gives_for_a_disconnect_leads_to_the_policys_row_for_it() {
    for (reason, outcome, name, step) in [
        (
            DisconnectReason::RoomDeleted,
            Outcome::RoomDeleted,
            "room_deleted",
            "ask now",
        ),
        (
            DisconnectReason::ParticipantRemoved,
            Outcome::ParticipantRemoved,
            "participant_removed",
            "ask now",
        ),
        (
            DisconnectReason::DuplicateIdentity,
            Outcome::DuplicateIdentity,
            "duplicate_identity",
            "stop",
        ),
        (
            DisconnectReason::ServerShutdown,
            Outcome::ServerShutdown,
            "server_shutdown",
            "wait",
        ),
        // No reason given, which is what follows a `Reconnecting` the SDK gave up on.
        (
            DisconnectReason::Lost,
            Outcome::ConnectionLost,
            "connection_lost",
            "wait",
        ),
    ] {
        assert_eq!(disconnect_outcome(reason), (outcome, name));
        assert_eq!(first_step(outcome), step, "{reason:?}");
    }
}

#[test]
fn each_refusal_of_a_session_is_its_own_outcome() {
    let cases = [
        (
            ApiError::Unauthenticated(refusal(401, "unauthorized")),
            Outcome::NotSignedIn,
            "stop",
        ),
        (
            ApiError::VoiceRequiresAuth(refusal(400, "voice_requires_auth")),
            Outcome::VoiceRequiresAuth,
            "stop",
        ),
        (
            ApiError::Forbidden(refusal(403, "forbidden")),
            Outcome::NotAPerson,
            "stop",
        ),
        (
            ApiError::ChannelNotFound(refusal(404, "channel_not_found")),
            Outcome::NotAMember,
            "stop",
        ),
        (
            ApiError::NotConfigured(refusal(503, "voice_not_configured")),
            Outcome::VoiceNotConfigured,
            "stop",
        ),
        (
            ApiError::Unavailable(refusal(503, "voice_unavailable")),
            Outcome::VoiceUnavailable,
            "wait",
        ),
        (ApiError::Timeout, Outcome::ServerUnreachable, "wait"),
        (
            ApiError::Connect {
                detail: "connection refused".into(),
            },
            Outcome::ServerUnreachable,
            "wait",
        ),
        (
            ApiError::Transport {
                detail: "reset".into(),
            },
            Outcome::ServerUnreachable,
            "wait",
        ),
        // Something between this client and conchd, or conchd in trouble: it may pass.
        (
            ApiError::UnexpectedResponse { status: 502 },
            Outcome::ServerUnreachable,
            "wait",
        ),
        (
            ApiError::Refused(refusal(500, "internal")),
            Outcome::ServerUnreachable,
            "wait",
        ),
    ];
    for (error, outcome, step) in cases {
        let (mapped, name) = session_outcome(&error).unwrap_or_else(|| panic!("{error}"));
        assert_eq!(mapped, outcome, "{error}");
        assert!(!name.is_empty());
        assert_eq!(first_step(mapped), step, "{error}");
    }
}

#[test]
fn an_error_no_retry_can_change_is_not_given_to_the_policy_at_all() {
    // The client ends with the error itself: there is no row for these, and retrying a
    // request that cannot succeed would only repeat it.
    for error in [
        ApiError::InvalidChannel {
            reason: "it is empty",
        },
        ApiError::InvalidToken,
        ApiError::Redirected {
            status: 301,
            location: "https://elsewhere.example".into(),
        },
        ApiError::Refused(refusal(418, "teapot")),
        // Answers to a transmit report, never to a session request.
        ApiError::NoSession(refusal(409, "voice_no_session")),
        ApiError::RateLimited(refusal(429, "voice_report_rate_limited")),
    ] {
        assert!(session_outcome(&error).is_none(), "{error}");
    }
}
