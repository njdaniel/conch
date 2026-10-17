# Security policy

## Reporting a vulnerability

Report it privately, through GitHub: open the repository's [Security tab](https://github.com/njdaniel/conch/security) and choose **Report a vulnerability**. Only the maintainer sees the report.

Please do not put the details in a public issue, pull request or discussion. Leave out real passwords, invite links and credentials (anything starting with `conch_`); a made-up one shows the problem just as well.

A useful report says what is affected (a component, an endpoint, a file), how to reproduce it, the revision you tested (`conchd version`, or the `/healthz` answer), and what an attacker gains.

## What is in scope

- `conchd`, the server: its REST, WebSocket and MCP APIs, authentication, tokens, invites, capabilities, approvals and audit.
- The clients: `conch` (CLI and TUI), the web client under `web/` that `conchd` serves at `/app/`, `conch-voice` under `voice/`, `conch-adapter` and `conch-bot`.
- The deploy files under [ops/deploy/](ops/deploy/): the systemd units, the Caddy and LiveKit configuration, the firewall, backup, restore and upgrade scripts.

Out of scope: weaknesses in LiveKit, Caddy, ntfy or another third-party project that Conch does not make worse (report those to that project), and attacks that need the operator's own credential or write access to the server's files.

## Supported versions

Conch is pre-alpha and has no releases yet. Only the latest `main` is supported: fixes land there, and nothing is backported.

## What happens after a report

- You get an acknowledgement on the advisory, normally within a week.
- The maintainer confirms the problem or explains why it is not one, and agrees a disclosure date with you, normally within 90 days of the report.
- The fix lands on `main`, and the advisory is published with it, crediting you unless you would rather not be named.
