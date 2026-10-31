# Code signing policy

Windows code signing is not enabled yet. This policy governs signing once it
is approved and configured. Until then, verify downloads using the hashes and
build attestations in [the official build guide](OFFICIAL-BUILD.md#checking-a-build-or-a-release-yourself).

## What is signed

Only the Windows background service `solud.exe` (downloaded as
`solud-windows-amd64.exe`) and the NSIS installer
`soluterminal_<version>_x64-setup.exe` built by
`.github/workflows/official-build.yml` in
[`soluterminal/soluterminal`](https://github.com/soluterminal/soluterminal)
are eligible for signing.

Only successful **tag-push runs** for `vX.Y.Z` may be submitted. Manual
`workflow_dispatch` runs are never eligible, even if attested. Local builds
and third-party binaries are not signed. The daemon's two independent builds
must match before signing; the installer is built once. Signing must happen
before the final hashes and build attestations are recorded.

## Who is responsible

The responsible GitHub organisation team is
[@soluterminal/release](https://github.com/orgs/soluterminal/teams/release).

| Role | Responsibility |
| --- | --- |
| Committers | Repository administrators maintain the source and push release tags. |
| Reviewers | Repository administrators review contributor changes before merging. |
| Approvers | Release maintainers check the tag, changes, build run and artifacts, then manually approve each signing request. |

Signing requires multi-factor authentication for repository and signing-service
access, and a protected signing environment with a required reviewer.

## Data transfers

The software does not transfer information to other networked systems unless
the user asks it to, with these exceptions: the update check, the optional
anonymous daily check-in, and the optional hosted cloud when signed in.
Self-built development versions send no check-in. See the
[privacy policy](https://soluterminal.com/privacy/) for details and controls.

## Verify a signed download

On Windows, right-click the downloaded `.exe`, open **Properties → Digital
Signatures**, select the signature and open **Details**. Windows must report
that the signature is valid. Alternatively, use PowerShell's
[`Get-AuthenticodeSignature`](https://learn.microsoft.com/powershell/module/microsoft.powershell.security/get-authenticodesignature):

```powershell
Get-AuthenticodeSignature -LiteralPath '.\solud-windows-amd64.exe' |
    Format-List Status, SignerCertificate
```

Repeat for the installer; `Status` must be `Valid`. Also check the download's
hash and build attestation using the official build guide above to confirm it
is this project's release. If a release advertised as signed has a missing or
invalid signature, do not run it.

Report signing problems or suspected tampering privately to
**security@soluterminal.com**, or use the private reporting route in
[SECURITY.md](../SECURITY.md).
