//! The small window shown while the backends start.
//!
//! Startup used to block the main thread until every daemon answered, and the
//! main window was only built after that, so nothing was on screen while the
//! WSL daemon started. When it could not start at all (2026-10-28: an older
//! daemon in a second WSL distro held its port) that was two minutes and more
//! of nothing, on every launch. The backends now start on a worker thread and
//! this window says what is happening. It is served by the shell itself over a
//! custom protocol, so it needs no daemon, has no IPC capability, polls
//! `/status` for the current step, and is closed as soon as the main window
//! exists.

use std::sync::Mutex;
use tauri::{AppHandle, Manager, WebviewUrl, WebviewWindowBuilder};

pub const LABEL: &str = "startup";
pub const SCHEME: &str = "solustartup";

pub const TITLE: &str = "Starting SoluTerminal…";
pub const WSL_FALLBACK: &str = "WSL didn't start; using Windows only for now";

struct Status {
    step: String,
    notice: String,
    detail: String,
}

static STATUS: Mutex<Status> = Mutex::new(Status {
    step: String::new(),
    notice: String::new(),
    detail: String::new(),
});

/// The step the startup window shows, e.g. "Starting the WSL background
/// service". Callable from any thread; a no-op when no window is shown.
pub fn step(text: &str) {
    if let Ok(mut status) = STATUS.lock() {
        status.step = text.to_string();
    }
}

/// The WSL backend failed and startup continues on Windows only.
#[cfg_attr(not(windows), allow(dead_code))]
pub fn wsl_fell_back(err: &str) {
    if let Ok(mut status) = STATUS.lock() {
        status.notice = WSL_FALLBACK.to_string();
        status.detail = fallback_detail(err);
    }
}

pub fn has_notice() -> bool {
    STATUS.lock().map(|s| !s.notice.is_empty()).unwrap_or(false)
}

/// One plain sentence under the fallback notice. The warning dialog still
/// carries the full explanation; this is what fits on the startup window.
fn fallback_detail(err: &str) -> String {
    if let Some(rest) = err.strip_prefix("WSL port held: ") {
        let mut chars = rest.chars();
        return match chars.next() {
            Some(first) => first.to_uppercase().chain(chars).collect(),
            None => String::new(),
        };
    }
    if err.contains("not reachable within") {
        return "The WSL background service did not answer in time.".to_string();
    }
    String::new()
}

fn status_json() -> String {
    let (step, notice, detail) = match STATUS.lock() {
        Ok(s) => (s.step.clone(), s.notice.clone(), s.detail.clone()),
        Err(_) => Default::default(),
    };
    serde_json::json!({ "title": TITLE, "step": step, "notice": notice, "detail": detail })
        .to_string()
}

fn escape_html(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    for c in text.chars() {
        match c {
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&quot;"),
            '\'' => out.push_str("&#39;"),
            c => out.push(c),
        }
    }
    out
}

/// The page, rendered with the current status so it is right even before the
/// first poll.
fn page() -> String {
    let (step, notice, detail) = match STATUS.lock() {
        Ok(s) => (s.step.clone(), s.notice.clone(), s.detail.clone()),
        Err(_) => Default::default(),
    };
    PAGE.replace("{{title}}", &escape_html(TITLE))
        .replace("{{step}}", &escape_html(&step))
        .replace("{{notice}}", &escape_html(&notice))
        .replace("{{detail}}", &escape_html(&detail))
        .replace("{{notice_hidden}}", if notice.is_empty() { "hidden" } else { "" })
}

/// The custom-protocol handler: `/status` is the JSON the page polls, and
/// every other path is the page.
pub fn respond(path: &str) -> tauri::http::Response<Vec<u8>> {
    let (content_type, body) = if path == "/status" {
        ("application/json", status_json())
    } else {
        ("text/html; charset=utf-8", page())
    };
    tauri::http::Response::builder()
        .header("Content-Type", content_type)
        .header("Cache-Control", "no-store")
        .header(
            "Content-Security-Policy",
            "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'",
        )
        .body(body.into_bytes())
        .unwrap_or_else(|_| tauri::http::Response::new(Vec::new()))
}

/// Where Tauri serves a custom protocol on this platform.
fn page_url() -> String {
    if cfg!(windows) {
        format!("http://{SCHEME}.localhost/")
    } else {
        format!("{SCHEME}://localhost/")
    }
}

fn is_page_url(url: &tauri::Url) -> bool {
    url.as_str().starts_with(&page_url())
}

/// Show the startup window. Best-effort: without it startup proceeds exactly
/// as before, just with nothing on screen until the main window appears.
pub fn open(app: &AppHandle) {
    let Ok(url) = page_url().parse::<tauri::Url>() else {
        return;
    };
    // Built hidden and shown after build(), like the main window: focusing or
    // minimizing a window while WebView2 is still being created deadlocked
    // the main thread (see StartupWindowPlan::start_hidden).
    let built = WebviewWindowBuilder::new(app, LABEL, WebviewUrl::External(url))
        .title("SoluTerminal")
        .inner_size(520.0, 320.0)
        .resizable(false)
        .maximizable(false)
        .center()
        .visible(false)
        .background_color(tauri::window::Color(17, 19, 24, 255))
        .on_navigation(|url| is_page_url(url))
        .build();
    match built {
        Ok(window) => {
            let _ = window.show();
            let _ = window.set_focus();
        }
        Err(e) => eprintln!("desktop: startup window unavailable: {e}"),
    }
}

/// Remove the startup window once the main window exists.
pub fn close(app: &AppHandle) {
    if let Some(window) = app.get_webview_window(LABEL) {
        let _ = window.destroy();
    }
}

const PAGE: &str = r#"<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>SoluTerminal</title>
<style>
  :root { color-scheme: dark; }
  html, body { margin: 0; height: 100%; background: #111318; color: #e6e8ee;
    font: 14px/1.45 "Segoe UI", system-ui, -apple-system, sans-serif; }
  main { box-sizing: border-box; height: 100%; padding: 28px 32px;
    display: flex; flex-direction: column; justify-content: center; gap: 10px; }
  h1 { margin: 0; font-size: 18px; font-weight: 600; display: flex; align-items: center; gap: 12px; }
  .spin { width: 16px; height: 16px; border-radius: 50%; border: 2px solid #3a3f4b;
    border-top-color: #7aa2f7; animation: spin 0.9s linear infinite; flex: none; }
  @keyframes spin { to { transform: rotate(360deg); } }
  @media (prefers-reduced-motion: reduce) { .spin { animation: none; } }
  #step { margin: 0; color: #aeb4c2; }
  #notice-box { margin-top: 6px; padding: 10px 12px; border-radius: 6px;
    background: #2a2416; border: 1px solid #5c4a1c; }
  #notice { margin: 0; color: #f0c674; font-weight: 600; }
  #detail { margin: 4px 0 0; color: #d8cfb8; font-size: 12.5px; user-select: text; word-break: break-word; }
  [hidden] { display: none !important; }
</style>
</head>
<body>
<main>
  <h1><span class="spin" aria-hidden="true"></span><span id="title">{{title}}</span></h1>
  <p id="step" role="status" aria-live="polite">{{step}}</p>
  <div id="notice-box" {{notice_hidden}}>
    <p id="notice">{{notice}}</p>
    <p id="detail">{{detail}}</p>
  </div>
</main>
<script>
  const el = (id) => document.getElementById(id);
  function render(s) {
    el("title").textContent = s.title || "";
    el("step").textContent = s.step || "";
    el("notice").textContent = s.notice || "";
    el("detail").textContent = s.detail || "";
    el("notice-box").hidden = !s.notice;
  }
  async function poll() {
    try {
      const response = await fetch("/status", { cache: "no-store" });
      if (response.ok) render(await response.json());
    } catch (_) {}
    setTimeout(poll, 400);
  }
  setTimeout(poll, 400);
</script>
</body>
</html>
"#;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn port_held_fallback_shows_the_distro_and_the_command() {
        let err = "WSL port held: port 8787 is in use by an older SoluTerminal service \
                   (version 0.0.1189, no backend id, pid 600) in the WSL distro \"Ubuntu-24.04\", \
                   so the WSL background service in \"Ubuntu\" cannot start. To stop it, run in \
                   PowerShell: wsl -d Ubuntu-24.04 -- kill 600";
        let detail = fallback_detail(err);
        assert!(detail.starts_with("Port 8787 is in use by an older SoluTerminal service"));
        assert!(detail.contains("wsl -d Ubuntu-24.04 -- kill 600"));
        assert_eq!(
            fallback_detail("WSL daemon was not reachable within 120s via []: x"),
            "The WSL background service did not answer in time."
        );
        assert_eq!(fallback_detail("spawn wsl.exe: nope"), "");
    }

    #[test]
    fn page_escapes_status_text_and_hides_an_empty_notice() {
        step("Starting the WSL background service");
        let html = page();
        assert!(html.contains("Starting SoluTerminal…"));
        assert!(html.contains(">Starting the WSL background service<"));
        assert!(html.contains("id=\"notice-box\" hidden"));
        assert_eq!(escape_html("<b>\"x\" & 'y'</b>"), "&lt;b&gt;&quot;x&quot; &amp; &#39;y&#39;&lt;/b&gt;");
    }

    #[test]
    fn only_the_page_itself_may_load_in_the_startup_window() {
        assert!(is_page_url(&page_url().parse().unwrap()));
        assert!(is_page_url(&format!("{}status", page_url()).parse().unwrap()));
        assert!(!is_page_url(&"http://127.0.0.1:8787/".parse().unwrap()));
        assert!(!is_page_url(&"https://example.com/".parse().unwrap()));
    }

    #[test]
    fn status_is_served_as_json_and_everything_else_as_the_page() {
        let status = respond("/status");
        assert_eq!(status.headers()["Content-Type"], "application/json");
        let body: serde_json::Value = serde_json::from_slice(status.body()).unwrap();
        assert_eq!(body["title"], TITLE);
        let page = respond("/");
        assert!(page.headers()["Content-Type"].to_str().unwrap().starts_with("text/html"));
        assert!(page.headers()["Content-Security-Policy"].to_str().unwrap().contains("default-src 'none'"));
    }
}
