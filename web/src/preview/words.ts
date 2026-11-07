// The page's own words in Preview as member (issue #1517). The server's
// refusals are shown by code elsewhere; these are the fixed lines of the
// placeholders and the controls.
import { canDo } from '../admin/permissions';
import type { WhoAmIResponseV1 } from '../schema/principal';
import { timeOf } from '../time';

export const messagesPlaceholder = 'Messages are not shown in preview. Open the channel as yourself to read it.';
export const readOnlyLine = 'Read-only preview: nothing can be sent, changed or joined from here.';
export const voicePreviewNote = 'Voice is a permission preview only. Nothing joins, and your microphone is not used.';

export const loadedLine = (at: number): string => `· read at ${timeOf(new Date(at).toISOString())}`;

export interface ControlLine {
  readonly label: string;
  readonly available: boolean;
  // One line of why, for a control that is not available, or what it allows.
  readonly reason: string;
}

// controlLines is what the previewed person holds, in the words of the
// controls they would see. Every answer is canDo on the target, the helper
// the page uses for itself; this file holds no table of permissions.
export function controlLines(who: WhoAmIResponseV1): readonly ControlLine[] {
  const people = who.role === 'operator' ? 'Operators' : 'Members';
  const channels = canDo(who, 'create_public_channels') || canDo(who, 'create_private_channels');
  const links = canDo(who, 'make_member_links') && who.may_invite !== false;
  const voice = canDo(who, 'use_voice') && who.voice_allowed !== false;
  return [
    {
      label: 'Create channel',
      available: channels,
      reason: channels ? `${who.name} can create channels.` : `${people} can't create channels on this server.`,
    },
    {
      label: 'Member links',
      available: links,
      reason: links ? `${who.name} can make member links.` : who.may_invite === false && canDo(who, 'make_member_links') ? `Member links are off for ${who.name}.` : `${people} can't make member links on this server.`,
    },
    {
      label: 'Voice',
      available: voice,
      reason: voice ? `${who.name} could join voice here.` : canDo(who, 'use_voice') ? `Voice is off for ${who.name}.` : `Voice is off for ${people.toLowerCase()}.`,
    },
    {
      label: 'Share screen',
      available: voice && canDo(who, 'share_screen'),
      reason: voice && canDo(who, 'share_screen') ? `${who.name} could share their screen.` : `${people} can't share their screen.`,
    },
    {
      label: 'Camera',
      available: voice && canDo(who, 'use_camera'),
      reason: voice && canDo(who, 'use_camera') ? `${who.name} could turn a camera on.` : `${people} can't use a camera.`,
    },
    {
      label: 'See everyone on the server',
      available: canDo(who, 'see_member_directory'),
      reason: canDo(who, 'see_member_directory') ? `${who.name} sees every member.` : `${who.name} sees the people they share a channel with.`,
    },
    {
      label: 'Manage members',
      available: canDo(who, 'manage_members'),
      reason: canDo(who, 'manage_members') ? `${who.name} manages members.` : `${who.name} does not manage members.`,
    },
    {
      label: 'Manage channels',
      available: canDo(who, 'manage_channels'),
      reason: canDo(who, 'manage_channels') ? `${who.name} manages channels.` : `${who.name} does not manage channels.`,
    },
  ];
}
