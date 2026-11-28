// The route shell for an open conversation, and the typed slot its view fills.
//
// The shell owns what is around the view (a heading to come back from, the
// conversation's key, the draft that outlives it) and nothing in it: the view
// is a DmConversationView, a function of DmConversationViewProps. The default
// slot is the exported transcript, so mounting the shell mounts it; another
// view (a placeholder in a test, a later design) is a different value of the
// same type. The view is keyed by scope and conversation, so another
// conversation, account or server is a new view with nothing carried over.
import type { ReactNode } from 'react';
import { useDm } from './DmContext';
import { DmTranscript, type DmConversationViewProps } from './DmTranscript';

export type DmConversationView = (props: DmConversationViewProps) => ReactNode;

export const transcriptView: DmConversationView = (props) => <DmTranscript {...props} />;

export function DmRoute({ conversationId, view = transcriptView, onClose, viewing = true }: {
  readonly conversationId: number;
  readonly view?: DmConversationView;
  readonly onClose?: () => void;
  readonly viewing?: boolean;
}) {
  const dm = useDm();
  if (dm === null) return null;
  const { api, scope, me, inbox, drafts, sendsFor } = dm;
  return (
    <div className="dm-route" data-conversation={`dm:${String(conversationId)}`} onKeyDown={(event) => {
      if (onClose === undefined || event.key !== 'Escape' || event.defaultPrevented || event.nativeEvent.isComposing) return;
      event.preventDefault();
      onClose();
    }}>
      {onClose === undefined ? null : (
        <button type="button" className="dm-back" data-dm-entry onClick={onClose}>Back</button>
      )}
      <div key={`${scope}/${String(conversationId)}`} className="dm-view">
        {view({
          conversationId,
          api,
          scope,
          viewer: me,
          draft: drafts.get(conversationId),
          onDraft: (conversation, text) => { drafts.set(conversation, text); },
          pendingSends: sendsFor(conversationId),
          onRead: inbox.read,
          enabled: inbox.state.enabled,
          snapshotVersion: inbox.state.snapshotVersion,
          viewing,
        })}
      </div>
    </div>
  );
}
