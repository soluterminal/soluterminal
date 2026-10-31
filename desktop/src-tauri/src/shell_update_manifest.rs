// Signed-manifest check for the cloud-mode installer update (shell_update.rs).
//
// The release manifest (update.json) names the installer URL and its SHA-256.
// Before this module, the shell trusted both straight from whatever the release
// host served, so anyone who could serve that file could hand it an installer.
// Now PowerShell only fetches bytes: the pinned update.json and its detached
// update.json.sig. Rust checks the Ed25519 signature with the key compiled in
// below, then reads the installer URL and digest from the verified bytes, and
// refuses anything that is not a versioned installer on the releases repo.
// Every failure stops the update (fail closed).
//
// The repo names come from shared/releases, the same files the daemon, the
// cloud and the release scripts read.
//
// Flavours (daemon/update_flavor.go): this shell is a "public" or "team"
// build (SOLU_SHELL_FLAVOR, set by build.rs from the daemons it bundles). The
// signed manifest must name its flavour ("flavor"), so a public shell never
// installs the team installer and a team shell never installs the public
// one. A manifest with no flavour (published before flavours) is refused too.
// Accounts move an install between flavours through the daemon, whose
// updater follows the account; this cloud-mode path, which runs without a
// local daemon, keeps the shell's own flavour.
//
// Only the public shell updates itself here. Team releases live in a private
// repo and reach team machines through the cloud, which serves them only to a
// paired machine's token (daemon/team_updates.go). A cloud-mode window has no
// local daemon and so no machine token: a team shell reports that its
// installer updates come through the app on this computer, stays on its
// installer, and never falls back to the public repo. This source names no
// team repo.

use base64::Engine as _;
use ed25519_dalek::{Signature, Verifier, VerifyingKey};

/// Same raw Ed25519 public key as the daemon (daemon/updater.go
/// defaultUpdateManifestPubKey), the cloud (cloud/release_provenance.go) and
/// the release scripts (scripts/release-ldflags.sh).
pub(crate) const DEFAULT_UPDATE_MANIFEST_PUBKEY: &str =
    "sm+MMrCMET4jQkfdIHySZ7RJWHLSV22qcBqKUVeU5k4=";

/// The key this build trusts. Like the daemon's -ldflags override, a release
/// build may bake in another key with SOLU_UPDATE_MANIFEST_PUBKEY at COMPILE
/// time (publish.sh exports it). There is deliberately no runtime override, and
/// unlike the daemon an empty value never turns verification off: it falls
/// back to the default key, so the shell always fails closed.
pub(crate) const UPDATE_MANIFEST_PUBKEY: &str = match option_env!("SOLU_UPDATE_MANIFEST_PUBKEY") {
    Some(key) if !key.is_empty() => key,
    _ => DEFAULT_UPDATE_MANIFEST_PUBKEY,
};

/// "<owner>/<name>" of the public releases repo, then its previous home.
pub(crate) fn release_repos() -> [&'static str; 2] {
    [
        include_str!("../../../shared/releases/repo.txt").trim(),
        include_str!("../../../shared/releases/legacy-repo.txt").trim(),
    ]
}

/// This shell's flavour: "team" or "public" (build.rs).
pub(crate) const SHELL_FLAVOR: &str = env!("SOLU_SHELL_FLAVOR");

/// The repos a shell of `flavor` reads, current owner first: none for "team"
/// (its releases come through the cloud, with a machine token this window
/// does not have), the public repo and its previous home for anything else.
pub(crate) fn release_repos_for(flavor: &str) -> Vec<&'static str> {
    if flavor == "team" {
        return Vec::new();
    }
    release_repos().to_vec()
}

/// What a team shell in cloud mode says instead of updating itself.
#[cfg_attr(not(windows), allow(dead_code))]
pub(crate) const TEAM_SHELL_UPDATE_UNAVAILABLE: &str =
    "Team installer updates come through the app running on this computer, \
     which fetches them from SoluTerminal cloud with this computer's sign-in. Switch to running \
     on this computer to update; this window stays on its current installer.";

/// The installer a verified manifest points at.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct VerifiedShellAsset {
    pub url: String,
    pub sha256: String,
    pub version: String,
}

#[derive(serde::Deserialize)]
struct ManifestAsset {
    url: String,
    sha256: String,
}

#[derive(serde::Deserialize)]
struct ManifestShell {
    version: String,
    platforms: std::collections::HashMap<String, ManifestAsset>,
}

#[derive(serde::Deserialize)]
struct Manifest {
    version: String,
    #[serde(default)]
    flavor: Option<String>,
    shell: Option<ManifestShell>,
}

/// Matches releaseVersionRe in daemon/updater_manifest.go.
pub(crate) fn valid_release_version(v: &str) -> bool {
    let (core, pre) = match v.split_once('-') {
        Some((core, pre)) => (core, Some(pre)),
        None => (v, None),
    };
    let parts: Vec<&str> = core.split('.').collect();
    let core_ok = (2..=4).contains(&parts.len())
        && parts
            .iter()
            .all(|p| !p.is_empty() && p.bytes().all(|b| b.is_ascii_digit()));
    let pre_ok = match pre {
        None => true,
        Some(p) => {
            !p.is_empty()
                && p
                    .bytes()
                    .all(|b| b.is_ascii_alphanumeric() || b == b'.' || b == b'-')
        }
    };
    core_ok && pre_ok
}

fn decode_base64_flexible(s: &str) -> Result<Vec<u8>, String> {
    use base64::engine::general_purpose::{STANDARD, STANDARD_NO_PAD, URL_SAFE, URL_SAFE_NO_PAD};
    let s = s.trim();
    for engine in [STANDARD, STANDARD_NO_PAD, URL_SAFE, URL_SAFE_NO_PAD] {
        if let Ok(bytes) = engine.decode(s) {
            return Ok(bytes);
        }
    }
    Err("not base64".to_string())
}

/// Verifies `sig_text` (base64 detached Ed25519 signature) over the exact
/// manifest bytes with `pubkey_b64`.
fn verify_signature(body: &[u8], sig_text: &str, pubkey_b64: &str) -> Result<(), String> {
    let key_bytes: [u8; 32] = decode_base64_flexible(pubkey_b64)
        .map_err(|_| "update key is invalid".to_string())?
        .try_into()
        .map_err(|_| "update key is invalid".to_string())?;
    let key = VerifyingKey::from_bytes(&key_bytes).map_err(|_| "update key is invalid".to_string())?;
    let sig_bytes: [u8; 64] = decode_base64_flexible(sig_text)
        .map_err(|_| "update signature is malformed".to_string())?
        .try_into()
        .map_err(|_| "update signature is malformed".to_string())?;
    key.verify(body, &Signature::from_bytes(&sig_bytes))
        .map_err(|_| "update signature verification failed".to_string())
}

/// Accepts only https://github.com/<repo>/releases/download/v<ver>/soluterminal_<shell>_x64-setup.exe
/// for one of `repos`.
fn installer_url_allowed(url: &str, repos: &[&str], shell_version: &str) -> bool {
    let Some(rest) = url.strip_prefix("https://github.com/") else {
        return false;
    };
    if url.contains(['?', '#', '%', '\\', '@']) || url.chars().any(char::is_whitespace) {
        return false;
    }
    repos.iter().any(|repo| {
        let Some(path) = rest
            .strip_prefix(*repo)
            .and_then(|p| p.strip_prefix("/releases/download/v"))
        else {
            return false;
        };
        let Some((tag, leaf)) = path.split_once('/') else {
            return false;
        };
        valid_release_version(tag) && leaf == format!("soluterminal_{shell_version}_x64-setup.exe")
    })
}

/// Checks the signature over `body`, then returns the windows-amd64 installer
/// it names. `expected_version` is the version the latest alias resolved to;
/// the pinned manifest must agree with it, and must be signed for `flavor`.
pub(crate) fn verify_shell_manifest(
    body: &[u8],
    sig_text: &str,
    pubkey_b64: &str,
    expected_version: &str,
    repos: &[&str],
    flavor: &str,
) -> Result<VerifiedShellAsset, String> {
    verify_signature(body, sig_text, pubkey_b64)?;
    let manifest: Manifest =
        serde_json::from_slice(body).map_err(|_| "update manifest is not valid".to_string())?;
    if manifest.version != expected_version {
        return Err("update manifest does not match the latest release".to_string());
    }
    if manifest.flavor.as_deref() != Some(flavor) {
        return Err(format!("update manifest is not for the {flavor} build"));
    }
    let shell = manifest
        .shell
        .ok_or_else(|| "no versioned windows-amd64 shell installer in manifest".to_string())?;
    let asset = shell
        .platforms
        .get("windows-amd64")
        .ok_or_else(|| "no versioned windows-amd64 shell installer in manifest".to_string())?;
    let parts: Vec<&str> = shell.version.split('.').collect();
    if parts.len() != 3 || !parts.iter().all(|p| !p.is_empty() && p.bytes().all(|b| b.is_ascii_digit())) {
        return Err("update manifest has an invalid installer version".to_string());
    }
    if asset.sha256.len() != 64 || !asset.sha256.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f')) {
        return Err("update manifest has an invalid installer digest".to_string());
    }
    if !installer_url_allowed(&asset.url, repos, &shell.version) {
        return Err("update manifest names an unexpected installer location".to_string());
    }
    Ok(VerifiedShellAsset {
        url: asset.url.clone(),
        sha256: asset.sha256.clone(),
        version: shell.version,
    })
}

/// PowerShell that fetches the latest release's update.json and signature into
/// `dir` as raw bytes, trying each repo in turn (the current owner first; the
/// legacy owner while the repo has not moved). Only the version is read from
/// the mutable latest alias; the bytes Rust verifies come from that release's
/// immutable per-tag URLs. Prints manifest-version=, manifest= and signature=.
/// Errors name no URLs: they reach the update dialog.
#[cfg(any(windows, test))]
pub(crate) fn fetch_manifest_script(repos: &[&str]) -> String {
    let repo_list = repos
        .iter()
        .map(|r| format!("'{}'", r.replace('\'', "''")))
        .collect::<Vec<_>>()
        .join(",");
    format!(
        r#"$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'
$dir = Join-Path $env:LOCALAPPDATA 'soluterminal\shell-update'
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$manifestPath = Join-Path $dir 'update.json'
$sigPath = Join-Path $dir 'update.json.sig'
Remove-Item -Force -LiteralPath $manifestPath, $sigPath -ErrorAction SilentlyContinue
$version = $null
$base = $null
$lastErr = 'no release found'
foreach ($repo in @({repo_list})) {{
  try {{ $probe = Invoke-RestMethod -UseBasicParsing -Uri ('https://github.com/' + $repo + '/releases/latest/download/update.json') }} catch {{ $lastErr = $_.Exception.Message; continue }}
  if ($probe -is [string]) {{ $probe = $probe | ConvertFrom-Json }}
  $v = [string]$probe.version
  if ($v -notmatch '^[0-9]+(\.[0-9]+){{1,3}}(-[0-9A-Za-z.-]+)?$') {{ [Console]::Error.WriteLine('update manifest has an invalid version'); exit 3 }}
  $version = $v
  $base = 'https://github.com/' + $repo + '/releases/download/v' + $v + '/'
  break
}}
if (-not $base) {{ [Console]::Error.WriteLine('could not reach the update server: ' + $lastErr); exit 2 }}
try {{
  Invoke-WebRequest -UseBasicParsing -Uri ($base + 'update.json') -OutFile $manifestPath
  Invoke-WebRequest -UseBasicParsing -Uri ($base + 'update.json.sig') -OutFile $sigPath
}} catch {{ [Console]::Error.WriteLine('could not download the update manifest: ' + $_.Exception.Message); exit 2 }}
[Console]::Out.WriteLine('manifest-version=' + $version)
[Console]::Out.WriteLine('manifest=' + $manifestPath)
[Console]::Out.WriteLine('signature=' + $sigPath)"#
    )
}

// Preserve the updater's process-scoped policy: staging uses Get-FileHash,
// which can require loading a script module on Windows PowerShell 5.1.
// This changes no persistent policy and does not override Group Policy.
#[cfg(any(windows, test))]
pub(crate) fn powershell_command(ps: &str, script: &str) -> std::process::Command {
    let mut command = std::process::Command::new(ps);
    command.args([
        "-NoProfile",
        "-NonInteractive",
        "-ExecutionPolicy",
        "Bypass",
        "-Command",
        script,
    ]);
    command
}

/// Runs the fetch script and verifies what it downloaded.
#[cfg(windows)]
pub(crate) fn fetch_verified_shell_asset(ps: &str) -> Result<VerifiedShellAsset, String> {
    let repos = release_repos_for(SHELL_FLAVOR);
    if repos.is_empty() {
        return Err(TEAM_SHELL_UPDATE_UNAVAILABLE.to_string());
    }
    let out = powershell_command(ps, &fetch_manifest_script(&repos))
        .output()
        .map_err(|e| format!("could not run powershell ({ps}): {e}"))?;
    if !out.status.success() {
        let code = out.status.code().unwrap_or(-1);
        return Err(format!(
            "installer update failed (exit {code}): {}",
            String::from_utf8_lossy(&out.stderr).trim()
        ));
    }
    let stdout = String::from_utf8_lossy(&out.stdout);
    let field = |name: &str| {
        stdout
            .lines()
            .find_map(|line| line.strip_prefix(name))
            .map(|v| v.trim().to_string())
            .filter(|v| !v.is_empty())
            .ok_or_else(|| "update manifest download did not complete".to_string())
    };
    let version = field("manifest-version=")?;
    let body = std::fs::read(field("manifest=")?)
        .map_err(|e| format!("could not read the update manifest: {e}"))?;
    let sig = std::fs::read_to_string(field("signature=")?)
        .map_err(|e| format!("could not read the update signature: {e}"))?;
    verify_shell_manifest(&body, &sig, UPDATE_MANIFEST_PUBKEY, &version, &repos, SHELL_FLAVOR)
}

#[cfg(test)]
mod tests {
    #[test]
    fn updater_commands_keep_process_scoped_execution_policy() {
        let ps = r"C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe";
        let script = r"Get-FileHash -LiteralPath 'C:\Users\O''Brien\setup.exe'";
        let command = super::powershell_command(ps, script);
        assert_eq!(command.get_program(), ps);
        assert_eq!(
            command.get_args().collect::<Vec<_>>(),
            [
                "-NoProfile",
                "-NonInteractive",
                "-ExecutionPolicy",
                "Bypass",
                "-Command",
                script
            ]
        );
    }

    use super::*;
    use ed25519_dalek::{Signer, SigningKey};

    const SHA: &str = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

    fn key() -> SigningKey {
        SigningKey::from_bytes(&[7u8; 32])
    }

    fn pubkey(k: &SigningKey) -> String {
        base64::engine::general_purpose::STANDARD.encode(k.verifying_key().to_bytes())
    }

    fn sign(k: &SigningKey, body: &[u8]) -> String {
        base64::engine::general_purpose::STANDARD.encode(k.sign(body).to_bytes())
    }

    fn manifest(url: &str) -> Vec<u8> {
        flavored_manifest(Some("public"), url)
    }

    fn flavored_manifest(flavor: Option<&str>, url: &str) -> Vec<u8> {
        let field = flavor.map(|f| format!(r#""flavor":"{f}","#)).unwrap_or_default();
        format!(
            r#"{{{field}"version":"0.0.1213","platforms":{{}},"shell":{{"version":"0.0.1213","platforms":{{"windows-amd64":{{"url":"{url}","sha256":"{SHA}"}}}}}}}}"#
        )
        .into_bytes()
    }

    fn url(repo: &str) -> String {
        format!("https://github.com/{repo}/releases/download/v0.0.1213/soluterminal_0.0.1213_x64-setup.exe")
    }

    fn check(body: &[u8], sig: &str, pubkey: &str) -> Result<VerifiedShellAsset, String> {
        verify_shell_manifest(body, sig, pubkey, "0.0.1213", &release_repos(), "public")
    }

    #[test]
    fn accepts_a_signed_manifest_under_either_owner() {
        let k = key();
        for repo in release_repos() {
            let body = manifest(&url(repo));
            let got = check(&body, &sign(&k, &body), &pubkey(&k)).expect("valid manifest");
            assert_eq!(got.url, url(repo));
            assert_eq!(got.sha256, SHA);
            assert_eq!(got.version, "0.0.1213");
        }
    }

    #[test]
    fn rejects_a_manifest_changed_after_signing() {
        let k = key();
        let signed = manifest(&url(release_repos()[0]));
        let tampered = String::from_utf8(signed.clone())
            .unwrap()
            .replace(SHA, &"f".repeat(64))
            .into_bytes();
        let err = check(&tampered, &sign(&k, &signed), &pubkey(&k)).unwrap_err();
        assert_eq!(err, "update signature verification failed");
    }

    #[test]
    fn rejects_a_manifest_signed_by_another_key() {
        let attacker = SigningKey::from_bytes(&[9u8; 32]);
        let body = manifest(&url(release_repos()[0]));
        let err = check(&body, &sign(&attacker, &body), UPDATE_MANIFEST_PUBKEY).unwrap_err();
        assert_eq!(err, "update signature verification failed");
    }

    #[test]
    fn rejects_missing_or_malformed_signatures() {
        let k = key();
        let body = manifest(&url(release_repos()[0]));
        for sig in ["", "not base64!", &base64::engine::general_purpose::STANDARD.encode([0u8; 12])] {
            assert!(check(&body, sig, &pubkey(&k)).is_err(), "signature {sig:?} accepted");
        }
        let zero = base64::engine::general_purpose::STANDARD.encode([0u8; 64]);
        assert!(check(&body, &zero, &pubkey(&k)).is_err());
    }

    #[test]
    fn rejects_signed_manifests_with_unsafe_contents() {
        let k = key();
        let repo = release_repos()[0];
        let bad_urls = [
            "http://github.com/".to_string() + repo + "/releases/download/v0.0.1213/soluterminal_0.0.1213_x64-setup.exe",
            "https://evil.example/".to_string() + repo + "/releases/download/v0.0.1213/soluterminal_0.0.1213_x64-setup.exe",
            url("someone-else/soluterminal-releases"),
            url(repo) + "?x=1",
            format!("https://github.com/{repo}/releases/latest/download/soluterminal_0.0.1213_x64-setup.exe"),
            format!("https://github.com/{repo}/releases/download/v0.0.1213/solud-windows-amd64.exe"),
            format!("https://github.com/{repo}/releases/download/v0.0.1213/..%2Fsoluterminal_0.0.1213_x64-setup.exe"),
            format!("https://github.com/{repo}/releases/download/v0.0.1213/soluterminal_0.0.1212_x64-setup.exe"),
        ];
        for bad in bad_urls {
            let body = manifest(&bad);
            let err = check(&body, &sign(&k, &body), &pubkey(&k)).unwrap_err();
            assert!(err.contains("unexpected installer location"), "{bad}: {err}");
        }
        let body = String::from_utf8(manifest(&url(repo))).unwrap().replace(SHA, &"A".repeat(64)).into_bytes();
        assert!(check(&body, &sign(&k, &body), &pubkey(&k)).unwrap_err().contains("digest"));
        let body = br#"{"flavor":"public","version":"0.0.1213","platforms":{}}"#;
        assert!(check(body, &sign(&k, body), &pubkey(&k)).unwrap_err().contains("no versioned"));
    }

    #[test]
    fn rejects_a_pinned_manifest_for_another_version() {
        let k = key();
        let body = manifest(&url(release_repos()[0]));
        let err = verify_shell_manifest(&body, &sign(&k, &body), &pubkey(&k), "0.0.1214", &release_repos(), "public")
            .unwrap_err();
        assert!(err.contains("does not match"), "{err}");
    }

    #[test]
    fn compiled_key_and_repos_match_the_rest_of_the_release_system() {
        let key = DEFAULT_UPDATE_MANIFEST_PUBKEY;
        let go = include_str!("../../../daemon/updater.go");
        assert!(go.contains(&format!("defaultUpdateManifestPubKey = \"{key}\"")));
        // The cloud and the release tooling are not part of the public source
        // export, so read them at run time. The product tree (marked by its
        // cloud/ directory, which the export leaves out) must have both.
        let root = concat!(env!("CARGO_MANIFEST_DIR"), "/../..");
        let product_tree = std::path::Path::new(root).join("cloud").is_dir();
        for (rel, needle) in [
            ("cloud/release_provenance.go", format!("defaultUpdateManifestPubKey = \"{key}\"")),
            ("scripts/release-ldflags.sh", format!("SOLU_DEFAULT_UPDATE_MANIFEST_PUBKEY=\"{key}\"")),
        ] {
            match std::fs::read_to_string(format!("{root}/{rel}")) {
                Ok(text) => assert!(text.contains(&needle), "{rel} disagrees with the shell's update key"),
                Err(e) => assert!(!product_tree, "{rel}: {e}"),
            }
        }
        // The public source copy has no previous home: legacy equals current.
        let [current, legacy] = release_repos();
        assert!(current.contains('/') && legacy.contains('/'));
    }

    #[test]
    fn accepts_only_a_manifest_signed_for_its_own_flavour() {
        let k = key();
        // A team installer as the private repo would name it.
        let team_repos = vec!["example-owner/team-releases"];
        for (shell, repos) in [("public", release_repos().to_vec()), ("team", team_repos)] {
            let url = url(repos[0]);
            for manifest_flavor in [Some("public"), Some("team"), None, Some("Team"), Some("migration")] {
                let body = flavored_manifest(manifest_flavor, &url);
                let got = verify_shell_manifest(&body, &sign(&k, &body), &pubkey(&k), "0.0.1213", &repos, shell);
                if manifest_flavor == Some(shell) {
                    assert_eq!(got.expect("own flavour").url, url);
                } else {
                    let err = got.expect_err("another flavour's installer was accepted");
                    assert!(err.contains("not for the"), "{shell} shell, manifest {manifest_flavor:?}: {err}");
                }
            }
        }
    }

    #[test]
    fn a_relabelled_flavour_fails_the_signature() {
        let k = key();
        let url = url(release_repos()[0]);
        let signed = flavored_manifest(Some("team"), &url);
        let relabelled = String::from_utf8(signed.clone())
            .unwrap()
            .replace(r#""flavor":"team""#, r#""flavor":"public""#)
            .into_bytes();
        assert_ne!(signed, relabelled);
        let err = check(&relabelled, &sign(&k, &signed), &pubkey(&k)).unwrap_err();
        assert_eq!(err, "update signature verification failed");
    }

    #[test]
    fn each_flavour_reads_only_its_own_repo() {
        let public = release_repos_for("public");
        assert_eq!(public, release_repos().to_vec());
        for flavor in ["", "Team", "nightly", "public"] {
            assert_eq!(release_repos_for(flavor), public, "flavour {flavor:?}");
        }
        // A team shell reads no GitHub repo at all: never the public one, and
        // the private team repo only through the cloud (not from here).
        assert!(release_repos_for("team").is_empty());
        assert!(TEAM_SHELL_UPDATE_UNAVAILABLE.contains("this computer"));
        // A team installer whose URL sits in the public repo is refused.
        let k = key();
        let body = flavored_manifest(Some("team"), &url(public[0]));
        let err = verify_shell_manifest(&body, &sign(&k, &body), &pubkey(&k), "0.0.1213", &["example-owner/team-releases"], "team")
            .unwrap_err();
        assert!(err.contains("unexpected installer location"), "{err}");
        assert!(SHELL_FLAVOR == "public" || SHELL_FLAVOR == "team");
    }

    #[test]
    fn version_check_matches_the_daemon() {
        for ok in ["0.0.1213", "1.2", "1.2.3.4", "1.2.3-rc.1"] {
            assert!(valid_release_version(ok), "{ok}");
        }
        for bad in ["", "1", "1.2.3.4.5", "../1.2", "1.2/3", "v1.2.3", "1.2.3-", "1..2"] {
            assert!(!valid_release_version(bad), "{bad}");
        }
    }

    #[test]
    fn fetch_script_pins_the_tag_and_tries_the_legacy_owner_second() {
        let repos = release_repos();
        let script = fetch_manifest_script(&repos);
        let current = script.find(&format!("'{}'", repos[0])).unwrap();
        if repos[0] != repos[1] {
            let legacy = script.rfind(&format!("'{}'", repos[1])).unwrap();
            assert!(current < legacy, "current owner must be tried first");
        }
        assert!(script.contains("/releases/latest/download/update.json"));
        assert!(script.contains("'/releases/download/v' + $v + '/'"));
        assert!(script.contains("-OutFile $manifestPath") && script.contains("-OutFile $sigPath"));
        // The mutable alias is never what gets verified.
        assert!(!script.contains("$probe | ConvertTo-Json"));
    }
}
