// Whether this page may take a password or start a guest visit (issue #713,
// design §5.4). Every page with such a form asks here first: the front door,
// an invite's page and a guest's Create an account.

// needsHttps says whether a page served from origin cannot take a password:
// plain HTTP from anywhere but this machine. The server refuses such a
// password itself (403 https_required), but only once it has crossed the
// network; the page says why before anyone types one, and offers no form
// that would send it.
export function needsHttps(origin: string): boolean {
  let url: URL;
  try {
    url = new URL(origin);
  } catch {
    return false;
  }
  if (url.protocol !== 'http:') return false;
  const host = url.hostname.replace(/^\[|\]$/g, '');
  return !(host === 'localhost' || host === '::1' || /^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(host));
}

// What a page says in place of such a form.
export const insecureLine =
  'This page is not served over HTTPS, so the server will not take a password or start a guest visit from it: anyone on the network could read what is sent. Ask the operator to serve Conch over HTTPS, for example behind a reverse proxy that holds a certificate (conchd started with --trusted-proxies naming it). Then open its HTTPS address.';
