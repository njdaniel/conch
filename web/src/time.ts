// Every time and date the page shows comes from here (F26), so a message's
// "21:36" and an invite's "expires Nov 6, 2026, 20:36" cannot disagree about
// the hour cycle: all four are one Intl.DateTimeFormat family, in the reader's
// own locale (for the order and names of a date) and time zone, on a 24-hour
// clock, as the page's message times always were (accessible names and tests
// read "21:36", not "9:36 PM"). A value that is not a time is
// returned as it came, never thrown on.
type Instant = string | number | Date;

const hours: Intl.DateTimeFormatOptions = { hour: '2-digit', hourCycle: 'h23' };

const formats = new Map<string, Intl.DateTimeFormat>();

function format(kind: string, options: Intl.DateTimeFormatOptions, at: Instant): string {
  const date = at instanceof Date ? at : new Date(at);
  if (Number.isNaN(date.getTime())) return String(at);
  let f = formats.get(kind);
  if (f === undefined) {
    f = new Intl.DateTimeFormat(undefined, options);
    formats.set(kind, f);
  }
  return f.format(date);
}

// timeOf: the time of day, to the minute.
export const timeOf = (at: Instant): string => format('time', { ...hours, minute: '2-digit' }, at);

// timeWithSeconds: the time of day, to the second.
export const timeWithSeconds = (at: Instant): string => format('seconds', { ...hours, minute: '2-digit', second: '2-digit' }, at);

// dateOf: the day.
export const dateOf = (at: Instant): string => format('date', { day: 'numeric', month: 'short', year: 'numeric' }, at);

// dateTimeOf: the day and the time of day, to the minute.
export const dateTimeOf = (at: Instant): string => format('datetime', { day: 'numeric', month: 'short', year: 'numeric', ...hours, minute: '2-digit' }, at);
