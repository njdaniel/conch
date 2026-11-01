// Settings → Reports, for an operator (issue #1029, ADR-014 §4.2: reading and
// filing problem reports is a right of every operator): the reports people
// sent from "Report a problem", one opened to read, and Mark filed with the
// address of the issue it became. It is what `conch admin reports
// list|show|file` does, in the page.
//
// The server decides who may read them: every route is operator-only, and a
// caller it refuses (403 forbidden) is told so in one line. The page never
// redacts or un-redacts what the route returns. A report is other people's
// text, and the issue address is text an operator typed: every value is
// rendered as text, and the address becomes a link only if the message log's
// rules (linkify.ts) would make it one.
import { useCallback, useEffect, useId, useRef, useState, type SyntheticEvent } from 'react';
import * as api from '../api';
import { linkify } from '../linkify';
import type { ProblemReportOften, ProblemReportSummaryV1, ProblemReportV1 } from '../schema/report';

// schema.MaxProblemReportIssueURL, in bytes.
export const maxIssueAddress = 512;

// issueRefusal is a deliberately minimal pre-check of an issue address, to
// save a round trip for the plain mistakes; the server (UpdateProblemReport
// RequestV1.Validate) stays the authority and its refusal is shown as it
// comes. The address is trimmed of surrounding whitespace, and the trimmed
// string is what is checked and what is sent (issueAddress). It must be 1 to
// 512 UTF-8 bytes and start with an http or https scheme and two slashes, in either case.
// Everything else is the server's call.
export function issueAddress(typed: string): string {
  return typed.trim();
}

export function issueRefusal(typed: string): string | null {
  const address = issueAddress(typed);
  const bytes = new TextEncoder().encode(address).length;
  if (bytes === 0 || bytes > maxIssueAddress) return `The issue address must be 1 to ${String(maxIssueAddress)} bytes.`;
  if (!/^https?:[/][/]/i.test(address)) return 'The issue address must be an http or https address.';
  return null;
}

const oftenLines: Readonly<Record<ProblemReportOften, string>> = {
  every_time: 'Every time',
  sometimes: 'Sometimes',
  once: 'Once so far',
};

const voiceLines: Readonly<Record<string, string>> = {
  ok: 'working',
  unavailable: 'unavailable',
  not_configured: 'not set up',
};

// Who sent a report: their name where the page knows it, else their number.
function senderOf(id: number, names: ReadonlyMap<number, string>): string {
  return names.get(id) ?? `person #${String(id)}`;
}

function when(at: string) {
  return <time dateTime={at}>{new Date(at).toLocaleString()}</time>;
}

// IssueLink shows a filed report's address: a link when the message log's
// rules make the whole address one link, else the same characters as text.
function IssueLink({ address }: { readonly address: string }) {
  const parts = linkify(address);
  const only = parts.length === 1 ? parts[0] : undefined;
  if (only?.kind === 'link' && only.text === address) {
    return (
      <a href={only.href} target="_blank" rel="noopener noreferrer">
        {address}
      </a>
    );
  }
  return <>{address}</>;
}

type Why = 'forbidden' | 'network' | 'other';

function whyOf(result: { readonly failure: api.ApiFailure; readonly code?: string }): Why {
  if (result.failure === 'network') return 'network';
  return result.code === 'forbidden' ? 'forbidden' : 'other';
}

function summaryOf(report: ProblemReportV1): ProblemReportSummaryV1 {
  return {
    id: report.id,
    state: report.state,
    created_at: report.created_at,
    principal_id: report.principal_id,
    client_name: report.client.name,
    client_version: report.client.version,
    server_version: report.server_version,
    often: report.often,
    ...(report.issue === undefined ? {} : { issue: report.issue }),
  };
}

export function ReportsSection({ names, onUnauthorized }: { readonly names?: ReadonlyMap<number, string>; readonly onUnauthorized: () => void }) {
  const id = useId();
  const known = names ?? new Map<number, string>();
  const [open, setOpen] = useState(false);
  const [reports, setReports] = useState<readonly ProblemReportSummaryV1[] | null>(null);
  const [loadLine, setLoadLine] = useState<string | null>(null);
  const [forbidden, setForbidden] = useState(false);
  const [opened, setOpened] = useState<ProblemReportV1 | null>(null);
  const [openLine, setOpenLine] = useState<string | null>(null);

  // Counts the reads started, so an answer to an earlier one is ignored.
  const reads = useRef(0);
  const shown = useRef(0);
  // The reports this page has filed. A read that started before a filing may
  // land after it with the report still open; whatever is read is reconciled
  // with this before it is shown. A filed report never goes back to open, so
  // the server's own word wins once it says filed.
  const filedById = useRef(new Map<number, ProblemReportV1>());

  const read = useCallback(() => {
    const mine = ++reads.current;
    void api.listReports().then((result) => {
      if (mine !== reads.current) return;
      if (result.ok) {
        setReports(
          result.value.map((r) => {
            const mine = filedById.current.get(r.id);
            return mine !== undefined && r.state !== 'filed' ? summaryOf(mine) : r;
          }),
        );
        setLoadLine(null);
        return;
      }
      if (result.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      const why = whyOf(result);
      if (why === 'forbidden') setForbidden(true);
      else setLoadLine(why === 'network' ? 'Cannot reach the server.' : 'Could not read the reports.');
    });
  }, [onUnauthorized]);

  // Read afresh each time the section opens.
  useEffect(() => {
    if (open) read();
  }, [open, read]);

  function show(summary: ProblemReportSummaryV1) {
    setOpenLine(null);
    // Only the report asked for last is shown: an answer for an earlier
    // choice that arrives late is dropped.
    const mine = ++shown.current;
    void api.getReport(summary.id).then((result) => {
      if (mine !== shown.current) return;
      if (result.ok) {
        const mine = filedById.current.get(result.value.id);
        setOpened(mine !== undefined && result.value.state !== 'filed' ? mine : result.value);
        return;
      }
      if (result.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      const why = whyOf(result);
      if (why === 'forbidden') setForbidden(true);
      else setOpenLine(why === 'network' ? 'Cannot reach the server.' : 'Could not read that report.');
    });
  }

  function filed(report: ProblemReportV1) {
    filedById.current.set(report.id, report);
    // The row is updated whichever report is open; the open one is replaced
    // only if it is still this one, so a draft for another is kept.
    setOpened((cur) => (cur !== null && cur.id === report.id ? report : cur));
    setReports((was) => was?.map((r) => (r.id === report.id ? summaryOf(report) : r)) ?? null);
  }

  if (forbidden) return <p className="hint reports-forbidden">Reports are not open to you on this server.</p>;

  return (
    <div className="guest-settings reports-section">
      <button
        type="button"
        aria-expanded={open}
        aria-controls={`${id}-panel`}
        onClick={() => {
          setOpen(!open);
          shown.current++;
          setOpened(null);
          setOpenLine(null);
        }}
      >
        Reports
      </button>
      <div id={`${id}-panel`} role="region" aria-label="Reports" hidden={!open} className="guest-settings-panel">
        {loadLine === null ? null : (
          <p role="alert" className="notice">
            {loadLine}
          </p>
        )}
        {opened !== null ? (
          <ReportView
            key={opened.id}
            report={opened}
            names={known}
            onBack={() => {
              shown.current++;
              setOpened(null);
            }}
            onFiled={filed}
            onUnauthorized={onUnauthorized}
            onForbidden={() => {
              setForbidden(true);
            }}
          />
        ) : reports === null ? (
          loadLine === null ? (
            <p className="hint">Loading…</p>
          ) : null
        ) : reports.length === 0 ? (
          <p className="hint">No reports yet.</p>
        ) : (
          <>
            {openLine === null ? null : (
              <p role="alert" className="notice">
                {openLine}
              </p>
            )}
            <ul className="signin-list report-list" aria-label="Reports">
              {reports.map((r) => (
                <li key={r.id}>
                  <button
                    type="button"
                    className="report-open"
                    onClick={() => {
                      show(r);
                    }}
                  >
                    Report #{r.id}
                  </button>{' '}
                  <span>{r.state === 'filed' ? 'Filed' : 'Open'}</span>
                  <span className="hint">
                    {' '}
                    · {when(r.created_at)} · from {senderOf(r.principal_id, known)} · {r.client_name} {r.client_version} · {oftenLines[r.often]}
                  </span>
                  {r.issue === undefined ? null : (
                    <span className="hint">
                      {' '}
                      · <IssueLink address={r.issue} />
                    </span>
                  )}
                </li>
              ))}
            </ul>
          </>
        )}
      </div>
    </div>
  );
}

function Text({ label, value }: { readonly label: string; readonly value: string }) {
  return (
    <>
      <h4>{label}</h4>
      <p className="report-text">{value}</p>
    </>
  );
}

function ReportView({
  report,
  names,
  onBack,
  onFiled,
  onUnauthorized,
  onForbidden,
}: {
  readonly report: ProblemReportV1;
  readonly names: ReadonlyMap<number, string>;
  readonly onBack: () => void;
  readonly onFiled: (report: ProblemReportV1) => void;
  readonly onUnauthorized: () => void;
  readonly onForbidden: () => void;
}) {
  const id = useId();
  const [address, setAddress] = useState('');
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<string | null>(null);

  function file(event: SyntheticEvent) {
    event.preventDefault();
    if (busy) return;
    const trimmed = issueAddress(address);
    const refusal = issueRefusal(trimmed);
    if (refusal !== null) {
      setLine(refusal);
      return;
    }
    setBusy(true);
    setLine(null);
    void api.fileReport(report.id, trimmed).then((result) => {
      setBusy(false);
      if (result.ok) {
        onFiled(result.value);
        return;
      }
      if (result.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      if (result.failure === 'network') setLine('Cannot reach the server. The report was not marked filed.');
      else if (result.code === 'forbidden') onForbidden();
      else setLine(`The server refused${result.code === undefined ? '' : ` (${result.code})`}. The report was not marked filed.`);
    });
  }

  const voice = voiceLines[report.voice] ?? report.voice;
  return (
    <div className="report-view" role="group" aria-label={`Report #${String(report.id)}`}>
      <button type="button" onClick={onBack}>
        Back to the reports
      </button>
      <h3>Report #{report.id}</h3>
      <p>
        {report.state === 'filed' ? 'Filed' : 'Open'} · {when(report.created_at)} · from {senderOf(report.principal_id, names)}
      </p>
      <Text label="What they did" value={report.did} />
      <Text label="What they expected" value={report.expected} />
      <Text label="What happened" value={report.happened} />
      <p>How often: {oftenLines[report.often]}</p>
      <p>
        Client: {report.client.name} {report.client.version}, {report.client.platform}
      </p>
      <p>Server: {report.server_version}</p>
      <p>Voice: {voice}</p>
      <details>
        <summary>Technical details</summary>
        <h4>Diagnostics</h4>
        <pre className="report-technical">{JSON.stringify(report.diagnostics, null, 2)}</pre>
        <h4>What the server saw</h4>
        <pre className="report-technical">{JSON.stringify(report.server, null, 2)}</pre>
      </details>
      {report.state === 'filed' ? (
        <p>
          Filed as {report.issue === undefined ? 'an issue' : <IssueLink address={report.issue} />}
          {report.filed_at === undefined ? null : <> on {when(report.filed_at)}</>}.
        </p>
      ) : (
        <form aria-label="Mark filed" onSubmit={file}>
          <label htmlFor={`${id}-issue`}>Issue address</label>
          <input
            id={`${id}-issue`}
            type="text"
            inputMode="url"
            autoComplete="off"
            spellCheck={false}
            value={address}
            aria-describedby={`${id}-help`}
            onChange={(event) => {
              setAddress(event.target.value);
              setLine(null);
            }}
          />
          <p id={`${id}-help`} className="hint">
            The address of the issue this report became (http or https).
          </p>
          <button type="submit" aria-busy={busy}>
            {busy ? 'Marking…' : 'Mark filed'}
          </button>
          <p role="status" className={line === null ? 'hint' : 'notice'}>
            {line ?? ''}
          </p>
        </form>
      )}
    </div>
  );
}
