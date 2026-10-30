// A fresh process standing for an app start: load the system roots as main
// does, then ask the server given as the argument, printing what happened.
import tls from 'node:tls';

import { isCertificateFailure, trustSystemRoots } from '../../main/api/trust.ts';

const origin = process.argv[2] ?? '';
if (process.env['CONCH_SKIP_SYSTEM_ROOTS'] !== '1') trustSystemRoots(tls);
try {
  const response = await fetch(`${origin}/v1/whoami`);
  const body = (await response.json()) as { id: number };
  console.log(`ok ${String(body.id)}`);
} catch (error) {
  console.log(isCertificateFailure(error) ? 'certificate' : 'network');
}
