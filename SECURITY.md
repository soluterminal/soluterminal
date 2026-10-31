# Security

## Reporting a vulnerability

Please report it privately, not in a public issue:

- GitHub: use **Report a vulnerability** on this repository's
  [Security tab](https://github.com/soluterminal/soluterminal/security/advisories/new), or
- email **security@soluterminal.com**.

Include the affected version, how to reproduce it and what an attacker could
do. We read every report and reply as soon as we can; there is no fixed
response time yet. Please check the issue against the latest version first.

## What is in scope

- The local daemon (`daemon/`), the desktop shell (`desktop/`), the web app
  (`web/`) and the protocol (`shared/`).
- How the app decides who may approve agent actions, the update signature
  check, and anything that lets a web page, another program or the cloud relay
  act on your computer.

The hosted cloud service is not in this repository, but reports about what it
can see or do through the relay are welcome here too.

## How releases are protected

Builds with an embedded verification key enforce signed update manifests;
older builds may use HTTPS and hashes alone. Check the actual distributed
artifact. See [update verification](docs/UPDATE_SIGNING.md) for the boundary
and the separate assurances it does not establish.

## Known limits

These are boundaries of the design, not bugs; reports that move them are
still welcome.

- **Agents running as you.** Approving agent work, answering permission
  prompts and changing autonomy settings are reserved for you unless you turn
  on Agent permissions. The daemon enforces this for requests made through its
  tools and API by processes it can trace to a pane, and for the web UI in
  your browser. It cannot reliably tell you apart from a program that runs as
  your own operating-system user and deliberately evades that tracing (for
  example by hiding its process details, sending the request from a Windows
  program through WSL, or reading your browser's cookies). Run code you don't
  trust under a separate user account, a container or the agent sandbox.
- **End-to-end encryption covers approved devices.** A browser or phone that
  the computer approved with matching codes, and that pinned the computer's
  key, uses the end-to-end channel by default and never falls back to the
  plain relay for that computer. A device that isn't approved that way (for
  example, while "Only devices I approve" is off) sends its sessions through
  the relay readable. With encryption, the relay still sees that a connection
  exists, which device it's from, when and how much data passes, and IP
  addresses, and it can block or delay the connection. Outside the channel:
  the web app's own code (served by our cloud), approval and status checks,
  voice transcription, and push alerts (without session details under
  "Only devices I approve").
- **First approval.** A device's first approval is protected by you comparing
  the code on both screens; a relay that alters the request makes the codes
  differ. Approvals made with an older app don't verify the computer and are
  marked as such until approved again.
- **Unattended desktop updates.** When the Windows desktop app restarts itself
  to install an update while you are idle, the installer is checked against the
  update's published SHA-256 (in the official app, from the signed update
  manifest) before it is staged, and checked again right before it runs.
  Between the two, a minute or so can pass while the update is staged and the
  app closes. The second check catches a swapped installer. A program that runs
  as your own operating-system user can still rewrite the update's script files
  in the same folder during that time, or swap the installer in the instant
  after the last check, and get its own file run; that is the same boundary as
  "Agents running as you" above. The official
  public app does this only after we publish a signed setting that turns it on
  (it is off today); once it is on, you can switch it off with Settings ›
  Devices › Updates › Desktop app restarts.
