// Pinned generator; no timestamps, git-derived versions or host paths in COFF.
import { readFileSync, writeFileSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { execFileSync } from 'node:child_process';
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const version = process.argv[2] || JSON.parse(readFileSync(path.join(root, 'desktop/src-tauri/tauri.conf.json'))).version;
if (!/^\d+\.\d+\.\d+$/.test(version) || version.split('.').some(n => Number(n) > 65535)) {
  throw new Error('Windows release version must be X.Y.Z with components <= 65535');
}
// Release builds verify the committed input without changing the source tree.
const check = process.argv[3] === '--check';
const target = path.resolve(check ? path.join(root, 'daemon/rsrc_windows_amd64.syso') :
  process.argv[3] || path.join(root, 'daemon/rsrc_windows_amd64.syso'));
const temp = mkdtempSync(path.join(tmpdir(), 'solu-winres-'));
const out = check ? path.join(temp, 'resource.syso') : target;
// The fetched generator needs Go's toolchain/cache settings, never release
// signing keys or other application credentials (including on `go env`).
const env = Object.fromEntries(Object.entries(process.env).filter(([key]) =>
  /^(PATH|HOME|TMPDIR|TMP|TEMP|SYSTEMROOT|USERPROFILE|LOCALAPPDATA|APPDATA)$/i.test(key) ||
  /^GO[A-Z0-9_]*$/.test(key)));
try {
  const info = {
    ProductName: 'SoluTerminal', FileDescription: 'SoluTerminal background service',
    CompanyName: 'The SoluTerminal Authors', LegalCopyright: 'Apache-2.0',
    OriginalFilename: 'solud.exe', FileVersion: version, ProductVersion: version,
  };
  writeFileSync(path.join(temp, 'winres.json'), JSON.stringify({
    RT_VERSION: { '#1': { '0000': {
      fixed: { file_version: version + '.0', product_version: version + '.0' },
      info: { '0409': info },
    } } },
    RT_MANIFEST: { '#1': { '0409': 'solud.manifest' } },
  }));
  writeFileSync(path.join(temp, 'solud.manifest'), `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3"><security><requestedPrivileges>
    <requestedExecutionLevel level="asInvoker" uiAccess="false"/>
  </requestedPrivileges></security></trustInfo>
</assembly>
`);
  // A caller may be cross-compiling. The generator itself runs on the host.
  const [GOOS, GOARCH] = execFileSync('go', ['env', 'GOHOSTOS', 'GOHOSTARCH'], { env, encoding: 'utf8' }).trim().split(/\r?\n/);
  execFileSync('go', ['run', 'github.com/tc-hib/go-winres@v0.3.3', 'make',
    '--in', path.join(temp, 'winres.json'), '--arch', 'amd64', '--no-suffix', '--out', out], {
    env: { ...env, GOOS, GOARCH }, stdio: 'inherit',
  });
  if (check && !readFileSync(out).equals(readFileSync(target))) {
    throw new Error('Committed Windows resource is stale; regenerate with node scripts/windows-resources.mjs ' + version + ' and commit daemon/rsrc_windows_amd64.syso');
  }
} finally { rmSync(temp, { recursive: true, force: true }); }
