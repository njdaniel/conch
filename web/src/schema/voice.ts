// The voice shapes of pkg/schema/voice.go, generated into generated.ts: the
// session that admits the page to a room, the presence snapshot the voice
// socket streams, the transmit report, the screen-share claim, why the
// reader's own share ended, the camera claim and why the reader's own camera
// ended (ADR-015), the viewer grant that lets a tab watch beside native voice
// (issue #936), and how many are in each channel's voice.
import type { VoicePresenceV2, VoicePresenceV3 } from './generated';

export type VoicePresence = VoicePresenceV2 | VoicePresenceV3;

export {
  voiceCameraV1,
  voiceCompanionRequestV1,
  voiceCompanionResponseV1,
  voiceOccupancyV1,
  voicePresenceV1,
  voicePresenceV2,
  voicePresenceV2Schema,
  voicePresenceV3,
  voicePresenceV3Schema,
  voiceSessionResponseV1,
  voiceShareRequestV1,
  voiceShareV1,
  voiceShareV2,
  voiceShareV3,
  voiceTransmitState,
  type VoiceCameraEnded,
  type VoiceCameraEndReason,
  type VoiceCameraV1,
  type VoiceCompanionRequestV1,
  type VoiceCompanionResponseV1,
  type VoiceChannelOccupancy,
  type VoiceOccupancyV1,
  type VoiceParticipant,
  type VoiceParticipantV2,
  type VoicePresenceRoom,
  type VoicePresenceRoomV2,
  type VoicePresenceV1,
  type VoicePresenceV2,
  type VoicePresenceV3,
  type VoiceRoomGrant,
  type VoiceSessionResponseV1,
  type VoiceShareEndReason,
  type VoiceShareEnded,
  type VoiceShareQuality,
  type VoiceShareQualityV3,
  type VoiceShareRequestV1,
  type VoiceShareRequestV2,
  type VoiceShareResolution,
  type VoiceShareResolutionV3,
  type VoiceShareV1,
  type VoiceShareV2,
  type VoiceShareV3,
  type VoiceTransmitReportV1,
  type VoiceTransmitState,
} from './generated';
