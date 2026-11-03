// The console's Members section (ADR-014 design §10.2, §10.3, issue #1020, and
// #972): everyone on the server, from the community directory, with the
// operator fields (role, owner, voice, member links, who invited them) for a
// holder of manage_members, and on each row one ⋯ menu of the actions this
// viewer may take on that person (memberActions.ts). The same rows, and the
// same menu, are the member panel's (Members.tsx).
//
// Each item is shown only when whoami says the viewer holds what it needs and
// the target is not protected from them. That hides, and never enforces: the
// server decides every request again, and its refusals are shown in this
// page's own words by code and permission. On a phone the menu is a sheet from
// the bottom, every item at least 44px.
//
// "Add a person" opens the member-link panel of the page, which needs
// make_member_links. The section is the same for an operator with no member
// permission at all: it lists the people the server shows them and offers no
// action.
import { useContext } from 'react';
import { AddPerson, SettingsOpen } from '../joiningSettings';
import { CommunityMembers } from '../Members';
import { usePhone } from '../phone';
import type { WhoAmIResponseV1 } from '../schema/principal';
import { canDo } from './permissions';

export const membersHint = 'Each person has a ⋯ menu with what you may do. A right-click or a long press on a person opens it too.';

export interface MembersSectionProps {
  readonly who: WhoAmIResponseV1;
  readonly origin: string;
  // Counts the times the server said the permissions changed.
  readonly refresh: number;
  readonly channelIds: string;
  readonly onUnauthorized: () => void;
}

export function MembersSection({ who, origin, refresh, channelIds, onUnauthorized }: MembersSectionProps) {
  const shown = useContext(SettingsOpen);
  const addPerson = useContext(AddPerson);
  const phone = usePhone();
  const mayAdd = canDo(who, 'make_member_links') && addPerson !== null;
  return (
    <CommunityMembers
      viewer={who.id}
      who={who}
      origin={origin}
      channelIds={channelIds}
      permissionsTick={refresh}
      title="Members"
      level={4}
      showTitle={!phone}
      active={shown}
      onUnauthorized={onUnauthorized}
      header={
        <>
          <p className="hint">{membersHint}</p>
          {mayAdd ? (
            <button type="button" onClick={addPerson}>
              Add a person
            </button>
          ) : null}
        </>
      }
    />
  );
}
