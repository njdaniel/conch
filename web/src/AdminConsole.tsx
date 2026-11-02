// The administration console (ADR-014 design §10.1, §10.3, issue #1018): the
// area of Settings an operator or the owner sees, and a member does not. It is
// a list of sections, each one an issue of its own:
//
//   Server (this issue), Roles and permissions (A7, #1019), Members (A8,
//   #1020) and Channels (A9, #1021), then Reports (#1029).
//
// A section whose issue has not merged is omitted, never shown as working: a
// later issue adds its row to `sections` below and nothing else here moves.
//
// On a wide window the sections are one under another in the Settings drawer.
// On a phone Settings is a page and the sections are a list; each opens as its
// own page with a Back control, and no section scrolls sideways.
//
// Every control inside keys on whoami (admin/permissions.ts). The server
// decides every request again: nothing here is enforcement.
import { useContext, useEffect, useId, useRef, useState, type ReactNode } from 'react';
import { JoiningSettings } from './joiningSettings';
import { usePhone } from './phone';
import type { ChannelV0 } from './schema/channel';
import type { WhoAmIResponseV1 } from './schema/principal';
import { ReportsSection } from './admin/ReportsSection';
import { ServerSection } from './admin/ServerSection';

interface Section {
  readonly id: string;
  readonly label: string;
  readonly render: () => ReactNode;
}

export interface AdminConsoleProps {
  readonly who: WhoAmIResponseV1;
  readonly origin: string;
  readonly channels: readonly ChannelV0[];
  // The names the page knows, for the Reports section.
  readonly names?: ReadonlyMap<number, string>;
  // Counts the times the server said the permissions changed.
  readonly permissionsChanged: number;
  readonly onUnauthorized: () => void;
}

export function AdminConsole({ who, origin, channels, names, permissionsChanged, onUnauthorized }: AdminConsoleProps) {
  const phone = usePhone();
  const id = useId();
  // The section open as a page on a phone; null is the list.
  const [page, setPage] = useState<string | null>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  const opener = useRef<string | null>(null);
  const list = useRef<HTMLUListElement>(null);

  // An operator sent to Joining from a refused invite lands on the Server
  // section, whichever page the phone was on.
  const sentHere = useContext(JoiningSettings)?.count ?? 0;
  useEffect(() => {
    if (sentHere > 0) setPage('server');
  }, [sentHere]);

  // The sections that exist, in order. A7 to A9 add theirs between Server and
  // Reports when they merge.
  const sections: readonly Section[] = [
    {
      id: 'server',
      label: 'Server',
      render: () => (
        <ServerSection who={who} origin={origin} channels={channels} refresh={permissionsChanged} onUnauthorized={onUnauthorized} />
      ),
    },
    {
      id: 'reports',
      label: 'Reports',
      render: () => <ReportsSection {...(names === undefined ? {} : { names })} onUnauthorized={onUnauthorized} />,
    },
  ];

  const current = sections.find((s) => s.id === page) ?? null;

  // A page takes focus when it opens; the list gets it back on Back.
  useEffect(() => {
    if (!phone) return;
    if (current !== null) heading.current?.focus();
    else if (opener.current !== null) {
      list.current?.querySelector<HTMLElement>(`[data-section="${opener.current}"]`)?.focus();
      opener.current = null;
    }
  }, [phone, current]);

  if (!phone) {
    return (
      <div className="admin-console">
        <h3 id={`${id}-title`}>Server administration</h3>
        {sections.map((s) => (
          <div key={s.id} className="admin-section">
            {s.render()}
          </div>
        ))}
      </div>
    );
  }

  if (current === null) {
    return (
      <div className="admin-console phone">
        <h3 id={`${id}-title`}>Server administration</h3>
        <ul ref={list} className="section-list" aria-labelledby={`${id}-title`}>
          {sections.map((s) => (
            <li key={s.id}>
              <button
                type="button"
                data-section={s.id}
                onClick={() => {
                  opener.current = s.id;
                  setPage(s.id);
                }}
              >
                {s.label}
              </button>
            </li>
          ))}
        </ul>
      </div>
    );
  }

  return (
    <div className="admin-console phone page">
      <button
        type="button"
        className="back"
        onClick={() => {
          setPage(null);
        }}
      >
        Back to administration
      </button>
      <h3 ref={heading} tabIndex={-1}>
        {current.label}
      </h3>
      {current.render()}
    </div>
  );
}
