// A fresh process standing for an app start, for the feed socket over TLS:
// load the system roots as main does, make one request (as the sign-in does
// before any feed, which is when Node sets its dispatcher), then open a feed
// socket to the server given as the argument and print the first thing that
// happens: "frame <text>" or "close <code>", and which socket it was.
import tls from 'node:tls';

import { agentConstructor, createNodeConnect } from '../../main/api/socket.ts';
import { trustSystemRoots } from '../../main/api/trust.ts';

const origin = process.argv[2] ?? '';
trustSystemRoots(tls);
await fetch(`${origin}/healthz`).catch(() => undefined);
const kind = agentConstructor() === null ? 'plain' : 'agent';
const handle = createNodeConnect(() => undefined)(`${origin.replace(/^http/u, 'ws')}/v2/ws`, {}, {
  open: () => undefined,
  frame: (data) => {
    console.log(`${kind} frame ${String(data)}`);
    handle.close();
    process.exit(0);
  },
  close: (code) => {
    console.log(`${kind} close ${String(code)}`);
    process.exit(0);
  },
});
