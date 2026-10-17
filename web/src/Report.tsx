import { useId, useState, type SyntheticEvent } from 'react';
import * as api from './api';
import { recent, voiceState } from './diagnostics';
import type { CreateProblemReportRequestV1, ProblemReportOften } from './schema/report';

// "Report a problem" (issue #446): a short form in the bug template's words.
// The page adds its version, the browser, the system, the window's size and
// its diagnostic ring itself, and shows the person exactly what will be sent
// before anything is. The report goes to this server's operator, who reads it
// before it goes anywhere else.

// The most characters one answer may hold (schema.MaxProblemReportText).
export const maxAnswer = 4000;

export interface Answers {
  readonly did: string;
  readonly expected: string;
  readonly happened: string;
  readonly often: ProblemReportOften | '';
}

const noAnswers: Answers = { did: '', expected: '', happened: '', often: '' };

const oftenLabels: readonly (readonly [ProblemReportOften, string])[] = [
  ['every_time', 'Every time'],
  ['sometimes', 'Sometimes'],
  ['once', 'Once so far'],
];

// clean keeps a describing string to one line of printable text, at most max
// characters, as the server takes it.
function clean(s: string, max: number): string {
  let out = '';
  for (const ch of s) {
    const c = ch.codePointAt(0) ?? 0;
    out += c < 0x20 || c === 0x7f ? ' ' : ch;
  }
  return out.slice(0, max);
}

// platformOf is the system part of a user agent: what its first parentheses
// hold ("X11; Linux x86_64", "iPhone; CPU iPhone OS 18_0 like Mac OS X").
function platformOf(userAgent: string): string {
  const inside = /\(([^)]*)\)/.exec(userAgent)?.[1]?.trim() ?? '';
  return inside === '' ? 'unknown' : clean(inside, 128);
}

// buildReport is the whole request: the person's answers and what the page
// knows of itself. It is built once, when the person asks to review, and that
// very object is what is shown and sent.
export function buildReport(answers: Answers): CreateProblemReportRequestV1 {
  const userAgent = clean(navigator.userAgent, 512);
  const version = api.clientVersion();
  const voice = voiceState();
  const width = Math.round(window.innerWidth);
  const height = Math.round(window.innerHeight);
  return {
    did: answers.did.trim(),
    expected: answers.expected.trim(),
    happened: answers.happened.trim(),
    often: answers.often === '' ? 'once' : answers.often,
    client: {
      name: 'conch-web',
      version: version !== null && /^[\x21-\x7e]{1,64}$/.test(version) ? version : 'unknown',
      ...(userAgent === '' ? {} : { user_agent: userAgent }),
      platform: platformOf(userAgent),
      ...(width > 0 && height > 0 && width < 100000 && height < 100000 ? { screen: `${String(width)}x${String(height)}` } : {}),
      ...(voice === null ? {} : { voice }),
    },
    diagnostics: recent(),
  };
}

type Step =
  | { readonly kind: 'form' }
  | { readonly kind: 'review'; readonly report: CreateProblemReportRequestV1 }
  | { readonly kind: 'sent'; readonly id: number };

// refusalLine says what a refusal means to the person.
function refusalLine(result: { readonly failure: api.ApiFailure; readonly code?: string; readonly message?: string; readonly retryAfter?: number }): string {
  if (result.failure === 'network') return 'Cannot reach the server. Your report is still here; try again.';
  if (result.code === 'rate_limited') {
    const minutes = Math.max(1, Math.ceil((result.retryAfter ?? 60) / 60));
    return `You have sent several reports recently. Try again in ${String(minutes)} minute${minutes === 1 ? '' : 's'}.`;
  }
  if (result.code === 'request_too_large') return 'The report is too long. Shorten your answers and try again.';
  if (result.code === 'human_required') return 'Reports are for people, not for this account.';
  return result.message === undefined ? 'The server refused the report.' : `The server refused the report: ${result.message}`;
}

export function ReportProblem({ onUnauthorized }: { readonly onUnauthorized: () => void }) {
  const id = useId();
  const [open, setOpen] = useState(false);
  const [answers, setAnswers] = useState<Answers>(noAnswers);
  const [step, setStep] = useState<Step>({ kind: 'form' });
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string | null>(null);

  const complete = answers.did.trim() !== '' && answers.expected.trim() !== '' && answers.happened.trim() !== '' && answers.often !== '';

  function review(event: SyntheticEvent) {
    event.preventDefault();
    if (!complete) return;
    setProblem(null);
    setStep({ kind: 'review', report: buildReport(answers) });
  }

  function send(report: CreateProblemReportRequestV1) {
    if (busy) return;
    setBusy(true);
    setProblem(null);
    void api.sendReport(report).then((result) => {
      setBusy(false);
      if (result.ok) {
        setAnswers(noAnswers);
        setStep({ kind: 'sent', id: result.value.id });
        return;
      }
      if (result.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      setProblem(refusalLine(result));
    });
  }

  const field = (key: 'did' | 'expected' | 'happened', label: string) => (
    <>
      <label htmlFor={`${id}-${key}`}>{label}</label>
      <textarea
        id={`${id}-${key}`}
        rows={3}
        maxLength={maxAnswer}
        value={answers[key]}
        onChange={(e) => {
          setAnswers({ ...answers, [key]: e.target.value });
        }}
      />
    </>
  );

  return (
    <div className="report">
      <button
        type="button"
        aria-expanded={open}
        aria-controls={`${id}-panel`}
        onClick={() => {
          setOpen(!open);
          setProblem(null);
          if (step.kind === 'sent') setStep({ kind: 'form' });
        }}
      >
        Report a problem
      </button>
      <div id={`${id}-panel`} className="report-panel" hidden={!open}>
        {step.kind === 'form' ? (
          <form aria-label="Report a problem" onSubmit={review}>
            <p className="hint">
              Your report goes to this server’s operator, who reads it before it goes anywhere else. Please leave out passwords, invite links and credentials
              (anything starting with conch_).
            </p>
            {field('did', 'What you did')}
            {field('expected', 'What you expected')}
            {field('happened', 'What happened instead')}
            <label htmlFor={`${id}-often`}>How often does it happen?</label>
            <select
              id={`${id}-often`}
              value={answers.often}
              onChange={(e) => {
                const value = oftenLabels.find(([v]) => v === e.target.value)?.[0] ?? '';
                setAnswers({ ...answers, often: value });
              }}
            >
              <option value="">Choose one</option>
              {oftenLabels.map(([value, label]) => (
                <option key={value} value={value}>
                  {label}
                </option>
              ))}
            </select>
            <button type="submit" disabled={!complete}>
              Review report
            </button>
          </form>
        ) : step.kind === 'review' ? (
          <section aria-label="Review your report">
            <p className="hint">This is exactly what will be sent. The server adds its version, the time, and the ids of your recent requests.</p>
            <pre className="report-preview" tabIndex={0} aria-label="The report as it will be sent">
              {JSON.stringify(step.report, null, 2)}
            </pre>
            <div className="actions">
              <button
                type="button"
                disabled={busy}
                onClick={() => {
                  send(step.report);
                }}
              >
                Send report
              </button>
              <button
                type="button"
                disabled={busy}
                onClick={() => {
                  setStep({ kind: 'form' });
                  setProblem(null);
                }}
              >
                Edit
              </button>
            </div>
          </section>
        ) : (
          <p role="status">Thank you. Your report was sent as number {step.id}; quote it if you are asked about it.</p>
        )}
        {problem === null ? null : (
          <p role="alert" className="notice">
            {problem}
          </p>
        )}
      </div>
    </div>
  );
}
