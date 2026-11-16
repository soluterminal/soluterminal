// Single source of truth for the managed runtime pins. install.mjs installs
// exactly these versions, launch.mjs refuses a runtime that has drifted from
// them, and scripts/check-dsh-pin.mjs reports npm drift for each entry.
export const PINS = { deepseek: '0.2.2-rc.1', pi: '1.5.2' };

// Pi 1.5 ships no npm-shrinkwrap.json, so SoluTerminal ships its own lockfile
// (pi-lock.json, resolved once from registry.npmjs.org, an integrity hash for
// every package). The installer refuses a lockfile whose bytes differ from
// this hash, then installs exactly that tree with `npm ci --ignore-scripts`.
// Regenerate both together when PINS.pi changes (see install.mjs).
export const PI_LOCK_SHA256 = 'eb61ad4446622c094607c8f9af1f4aecd83dfc6309dac1475637e2d39d3486de';

// DeepSeek Harness 0.2.2 pulls in 724 locked packages (654 on one platform),
// five with install scripts, so it gets the same gate as Pi: SoluTerminal's own
// lockfile (dsh-lock.json), hash-pinned here, installed with
// `npm ci --ignore-scripts`. Regenerate both together when PINS.deepseek changes,
// in the one-entry-per-line layout of formatHarnessLock (launch.mjs).
export const DSH_LOCK_SHA256 = 'ecf9cd9381b732a5c722e70077f8d61a6484658acbf0ac8bdb3ea626effa3eee';

// The one install-script effect DeepSeek Harness needs. With scripts off,
// dsh-subprocess-local's postinstall does not run. That script sets mode 0755
// on node-pty's prebuilt spawn-helper, which only the macOS prebuilds ship. The
// installer does it instead, for exactly this node-pty version and only for a
// file with the pinned SHA-256 (hashed from the locked tarball). Linux and
// Windows prebuilds have no helper.
export const DSH_SPAWN_HELPER = {
  nodePty: '1.2.0-beta.15',
  mode: 0o755,
  sha256: {
    'darwin-arm64': '08bc83a084651d095645010d34783f94b9af443614747e953e66d6578027e999',
    'darwin-x64': 'a3bed36ae3ed83b2ac2fce475bd578c51f5b536041f0b2dfe116987f565e9758',
  },
};
