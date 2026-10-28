# D1 read-only inventory and experiment entry points

This directory is isolated research tooling for issue #769, not code imported
by `conchd`, `conch`, the browser client or a desktop product.

Run `python3 inventory.py` from this directory, or use its full relative path
from the repository root. It needs Python 3 and uses installed `gdbus`,
`dpkg-query`, `pw-cli` and `wpctl` if present. Optional
`--helper /absolute/path/to/trusted/conch-voice` invokes only `--version`.
Output is an offline JSON evidence receipt, not an application wire schema.

The script does not bind shortcuts, open input devices, capture media, read
secret items, install packages or change settings. Each external command has
an eight-second timeout; the whole inventory may take longer. A denied bus or
hidden device in a sandbox must be recorded as such. Repeat with authorized
host read access before concluding a portal interface is absent. Packages and
display environment variables do not establish runtime support.

The existing synthetic network experiment is:

```sh
go run ./e2e/sdkprobe -only reasons -reps 1 -raw /tmp/conch-desktop-sdk
```

It requires the already approved repository toolchain and pinned LiveKit
Docker image, creates its own container, uses generated audio and cleans up.
Do not substitute a production server. The command includes several outage
cases, including a 90-second pause. Its native stdout may contain non-JSON
lines; retained duplicate receipts therefore use `.log` extensions.

See the [design](../../linux-desktop.md) and
[capability report](../../../reviews/linux-desktop-capabilities.md) for exact
versions, results, limitations and the human hardware procedure. The companion
participant (#897), the helper's subscription rule (#906), the Linux events
endpoint (#893) and the portal backend each need approved versioned
contracts or dependencies before proof code is added. No Electron runtime or
new protocol/dependency is installed by this research tooling.
