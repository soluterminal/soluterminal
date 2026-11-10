// `browser_pane_ping` is the shell-version sentinel: the web UI invokes it to
// learn which installed Tauri shell is running (the version check and the
// shell-update banner read it). It outlived the in-pane native browser it was
// named for; the command name stays so older and newer web bundles both find
// it.

use tauri::AppHandle;

#[tauri::command]
pub async fn browser_pane_ping(app: AppHandle) -> Result<String, String> {
    // Return the app's real version (from tauri.conf.json, which
    // matches the NSIS installer version) — not CARGO_PKG_VERSION
    // which stays pinned at 0.0.1 for the lib crate.
    Ok(app.package_info().version.to_string())
}
