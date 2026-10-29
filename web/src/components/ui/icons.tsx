// Shared chrome icon set — small, dependency-free inline SVGs (lucide-style,
// currentColor stroke) so the app chrome renders crisp, platform-consistent,
// state-recolorable glyphs instead of emoji (🎤 🔔 ⚙ …) that render heavier and
// in fixed colors that can't track state, and differ across Windows / macOS.
//
// Modeled on composer/icons.tsx (the reference we agreed to reuse). Every icon
// is aria-hidden + focusable=false — the button that hosts it carries the
// aria-label/title. Default size 18px is the chrome standard.

import type { SVGProps } from 'react';

type IconProps = SVGProps<SVGSVGElement> & { size?: number };

function svgProps({ size = 18, ...rest }: IconProps): SVGProps<SVGSVGElement> {
  return {
    width: size,
    height: size,
    viewBox: '0 0 24 24',
    fill: 'none',
    stroke: 'currentColor',
    strokeWidth: 2,
    strokeLinecap: 'round',
    strokeLinejoin: 'round',
    'aria-hidden': true,
    focusable: false,
    ...rest,
  };
}

/** Grid layout picker (was ▦). */
export function IconGrid(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <rect x="3" y="3" width="7" height="7" rx="1" />
      <rect x="14" y="3" width="7" height="7" rx="1" />
      <rect x="14" y="14" width="7" height="7" rx="1" />
      <rect x="3" y="14" width="7" height="7" rx="1" />
    </svg>
  );
}

/** Refresh / realign — two circular arrows (fix garbled pane rendering). */
export function IconRefresh(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8" />
      <path d="M21 3v5h-5" />
      <path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16" />
      <path d="M3 21v-5h5" />
    </svg>
  );
}

/** Text width — text lines over a horizontal expand arrow (reflow wide). */
export function IconTextWidth(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M3 5h18" />
      <path d="M3 10h12" />
      <path d="M7 14l-4 3 4 3" />
      <path d="M17 14l4 3-4 3" />
      <path d="M3 17h18" />
    </svg>
  );
}

/** Microphone — hold-to-talk (was 🎤). */
export function IconMic(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3Z" />
      <path d="M19 10v2a7 7 0 0 1-14 0v-2" />
      <line x1="12" x2="12" y1="19" y2="22" />
    </svg>
  );
}

/** Down chevron — dropdown caret (was ▾). */
export function IconChevronDown(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="m6 9 6 6 6-6" />
    </svg>
  );
}

/** Bell — alerts / notification center (was 🔔). */
export function IconBell(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9" />
      <path d="M10.3 21a1.94 1.94 0 0 0 3.4 0" />
    </svg>
  );
}

/** Bell with a slash — notifications muted (was 🔕). */
export function IconBellOff(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M8.7 3A6 6 0 0 1 18 8c0 2.1.4 3.8 1 5" />
      <path d="M17 17H3s3-2 3-9a4.67 4.67 0 0 1 .3-1.7" />
      <path d="M10.3 21a1.94 1.94 0 0 0 3.4 0" />
      <path d="m2 2 20 20" />
    </svg>
  );
}

/** Speaker with waves — voice alerts on (was 🔊). */
export function IconVolume(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <polygon points="11 5 6 9 2 9 2 15 6 15 11 19 11 5" />
      <path d="M15.54 8.46a5 5 0 0 1 0 7.07" />
      <path d="M19.07 4.93a10 10 0 0 1 0 14.14" />
    </svg>
  );
}

/** Gear — preferences / settings (was ⚙). */
export function IconSettings(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2Z" />
      <circle cx="12" cy="12" r="3" />
    </svg>
  );
}

/** Question mark in a circle — help / keyboard shortcuts (was a ? kbd). */
export function IconHelp(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <circle cx="12" cy="12" r="10" />
      <path d="M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3" />
      <path d="M12 17h.01" />
    </svg>
  );
}

/** Clock with a rewind arrow — transcript history (was 🕘). */
export function IconHistory(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8" />
      <path d="M3 3v5h5" />
      <path d="M12 7v5l4 2" />
    </svg>
  );
}

/** Stacked layers — a saved workspace / layout (context pill mark). */
export function IconLayers(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M12.83 2.18a2 2 0 0 0-1.66 0L2.6 6.08a1 1 0 0 0 0 1.83l8.58 3.91a2 2 0 0 0 1.66 0l8.58-3.9a1 1 0 0 0 0-1.83Z" />
      <path d="m22 17.65-9.17 4.16a2 2 0 0 1-1.66 0L2 17.65" />
      <path d="m22 12.65-9.17 4.16a2 2 0 0 1-1.66 0L2 12.65" />
    </svg>
  );
}

/** Lightning bolt — Local hosting (was ⚡). */
export function IconBolt(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M4 14a1 1 0 0 1-.78-1.63l9.9-10.2a.5.5 0 0 1 .86.46l-1.92 6.02A1 1 0 0 0 13 10h7a1 1 0 0 1 .78 1.63l-9.9 10.2a.5.5 0 0 1-.86-.46l1.92-6.02A1 1 0 0 0 11 14Z" />
    </svg>
  );
}

/** Cloud — Cloud hosting (was ☁). */
export function IconCloud(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M17.5 19H9a7 7 0 1 1 6.71-9h1.79a4.5 4.5 0 1 1 0 9Z" />
    </svg>
  );
}

/** Two overlapping frames — open a new window (multi-monitor). */
export function IconNewWindow(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <rect width="14" height="14" x="8" y="8" rx="2" ry="2" />
      <path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2" />
    </svg>
  );
}

/** Smartphone — phone-access pairing status (was a dot + "Phone" / 📱). */
export function IconSmartphone(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <rect width="14" height="20" x="5" y="2" rx="2" ry="2" />
      <path d="M12 18h.01" />
    </svg>
  );
}

/** Diamond — the background-agents / fleet cockpit chip (was a raw ◇/● glyph).
 *  Deliberately NOT a gear: ⚙ already means "workflow" in pane footers. */
export function IconDiamond(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M2.7 10.3a2.41 2.41 0 0 0 0 3.41l7.59 7.59a2.41 2.41 0 0 0 3.41 0l7.59-7.59a2.41 2.41 0 0 0 0-3.41L13.7 2.71a2.41 2.41 0 0 0-3.41 0Z" />
    </svg>
  );
}

/** Up arrow — an available update (was ↑). */
export function IconArrowUp(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="m5 12 7-7 7 7" />
      <path d="M12 19V5" />
    </svg>
  );
}

/** Clock — a queued / deferred update (was ⏳). */
export function IconClock(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <circle cx="12" cy="12" r="10" />
      <polyline points="12 6 12 12 16 14" />
    </svg>
  );
}

/** Warning triangle — a backend fallback / recoverable problem (was ⚠). */
export function IconAlertTriangle(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3Z" />
      <path d="M12 9v4" />
      <path d="M12 17h.01" />
    </svg>
  );
}

/** Speech bubble — send feedback / report a bug or share an idea (was 💬). */
export function IconMessage(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />
    </svg>
  );
}

/** Sidebar panel with a left rail — toggle (collapse/expand) the sidebar. The
 *  ChatGPT/VS Code convention for "hide the side panel". */
export function IconPanelLeft(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <rect width="18" height="18" x="3" y="3" rx="2" />
      <path d="M9 3v18" />
    </svg>
  );
}

/** Check — a selected menu option. */
export function IconCheck(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M20 6 9 17l-5-5" />
    </svg>
  );
}

/** Folder — the Files section. */
export function IconFolder(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z" />
    </svg>
  );
}

/** Account identity (was 👤) — monochrome so it matches the statusbar chrome.
    A person knocked out of a FILLED disc (evenodd), unlike the stroked rest
    of the set. This shape is deliberate, third try at 13-14px render size:
    the stroked lucide user's open shoulders read as "cut off", and even a
    solid bust reads as sliced because its flat bottom edge has no visible
    rounding at this scale. A circular boundary can't read as truncated, and
    all-fill means no thin strokes for 100%-DPI antialiasing to eat. */
export function IconUser(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)} fill="currentColor" stroke="none">
      {/* Cutouts stay well inside the disc (≥4 viewBox units): a thin bottom
          rim antialiases OPEN at fractional devicePixelRatios (broken circle
          at Windows 150%), and even an intact-but-wide shoulder cutout near
          the rim reads as a bite out of the disc at small sizes. Verified at
          dpr 1 / 1.5 / 2, true-size. */}
      <path
        fillRule="evenodd"
        d="M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20ZM12 6.2a3 3 0 1 1 0 6 3 3 0 0 1 0-6Zm0 7.9a4.4 2.2 0 1 1 0 4.4 4.4 2.2 0 0 1 0-4.4Z"
      />
    </svg>
  );
}

/** Globe — the Localhost ports section (dev servers you can open in a browser). */
export function IconGlobe(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <circle cx="12" cy="12" r="10" />
      <path d="M12 2a14.5 14.5 0 0 0 0 20 14.5 14.5 0 0 0 0-20" />
      <path d="M2 12h20" />
    </svg>
  );
}

/** Terminal in a frame — the Background sessions section (panes alive in the
 *  daemon but not on screen). */
export function IconTerminalSquare(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <rect x="3" y="3" width="18" height="18" rx="2" />
      <path d="m7 11 2-2-2-2" />
      <path d="M11 13h4" />
    </svg>
  );
}

/** Usage (spend / turns / sessions) — three chart bars. Replaced the
    speedometer gauge: its arc is OPEN at the bottom by design, which at the
    footer's 14px render size permanently reads as a circle with a bite cut
    out of it. Bars have no boundary that can look truncated. */
export function IconGauge(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M6 20v-6" />
      <path d="M12 20V4" />
      <path d="M18 20v-10" />
    </svg>
  );
}

/** Key — the Credentials section (logins you own). */
export function IconKey(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="m15.5 7.5 2.3 2.3a1 1 0 0 0 1.4 0l2.1-2.1a1 1 0 0 0 0-1.4L19 4" />
      <path d="m21 2-9.6 9.6" />
      <circle cx="7.5" cy="15.5" r="5.5" />
    </svg>
  );
}

/** Plug — the MCP servers section. */
export function IconPlug(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M12 22v-5" />
      <path d="M9 8V2" />
      <path d="M15 8V2" />
      <path d="M18 8v5a4 4 0 0 1-4 4h-4a4 4 0 0 1-4-4V8Z" />
    </svg>
  );
}

/** Plus — add / create (was ＋). */
export function IconPlus(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M5 12h14" />
      <path d="M12 5v14" />
    </svg>
  );
}

/** X — close / delete / remove (was ×). */
export function IconX(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M18 6 6 18" />
      <path d="m6 6 12 12" />
    </svg>
  );
}

/** Flag — a flagged pane (was ⚑). */
export function IconFlag(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M4 15s1-1 4-1 5 2 8 2 4-1 4-1V3s-1 1-4 1-5-2-8-2-4 1-4 1z" />
      <line x1="4" x2="4" y1="22" y2="15" />
    </svg>
  );
}

/** Pencil — rename / edit (was ✎). */
export function IconPencil(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M21.174 6.812a1 1 0 0 0-3.986-3.987L3.842 16.174a2 2 0 0 0-.5.83l-1.321 4.352a.5.5 0 0 0 .623.622l4.353-1.32a2 2 0 0 0 .83-.497z" />
      <path d="m15 5 4 4" />
    </svg>
  );
}

/** Pin — pinned / anchored (was 📌). */
export function IconPin(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M12 17v5" />
      <path d="M9 10.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24V16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V7a1 1 0 0 1 1-1 2 2 0 0 0 0-4H8a2 2 0 0 0 0 4 1 1 0 0 1 1 1z" />
    </svg>
  );
}

/** Magnifier — search / filter (was 🔍). */
export function IconSearch(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <circle cx="11" cy="11" r="8" />
      <path d="m21 21-4.3-4.3" />
    </svg>
  );
}

/** Padlock — a secret / protected file (was 🔒). */
export function IconLock(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <rect width="18" height="11" x="3" y="11" rx="2" ry="2" />
      <path d="M7 11V7a5 5 0 0 1 10 0v4" />
    </svg>
  );
}

/** Sparkle — an agent/AI touch (was ✦ / ✨). Single lucide sparkle so it reads
 *  at chip size; matches the stroked rest of the set. */
export function IconSparkle(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M9.937 15.5A2 2 0 0 0 8.5 14.063l-6.135-1.582a.5.5 0 0 1 0-.962L8.5 9.936A2 2 0 0 0 9.937 8.5l1.582-6.135a.5.5 0 0 1 .962 0L14.063 8.5A2 2 0 0 0 15.5 9.937l6.135 1.582a.5.5 0 0 1 0 .962L15.5 14.063a2 2 0 0 0-1.437 1.437l-1.582 6.135a.5.5 0 0 1-.962 0z" />
    </svg>
  );
}

/** Filled dot — a live / running status (was ●). Solid, stroke-none so it reads
 *  as a solid pip at 10–11px next to the stroked idle/off marks. Color comes
 *  from the host (e.g. ap-dot-ok) via currentColor. */
export function IconDotFilled(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)} fill="currentColor" stroke="none">
      <circle cx="12" cy="12" r="5" />
    </svg>
  );
}

/** Hollow dot — a neutral / idle status (was ○). */
export function IconCircle(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <circle cx="12" cy="12" r="8" />
    </svg>
  );
}

/** Circle with slash — a cancelled / prohibited status. */
export function IconBan(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <circle cx="12" cy="12" r="8" />
      <path d="m6.3 6.3 11.4 11.4" />
    </svg>
  );
}

/** Eye — reveal a hidden/secret value (was 👁). */
export function IconEye(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M2.062 12.348a1 1 0 0 1 0-.696 10.75 10.75 0 0 1 19.876 0 1 1 0 0 1 0 .696 10.75 10.75 0 0 1-19.876 0" />
      <circle cx="12" cy="12" r="3" />
    </svg>
  );
}

/** Eye with a slash — hide a revealed value (was 🙈). */
export function IconEyeOff(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M10.733 5.076a10.744 10.744 0 0 1 11.205 6.575 1 1 0 0 1 0 .696 10.747 10.747 0 0 1-1.444 2.49" />
      <path d="M14.084 14.158a3 3 0 0 1-4.242-4.242" />
      <path d="M17.479 17.499a10.75 10.75 0 0 1-15.417-5.151 1 1 0 0 1 0-.696 10.75 10.75 0 0 1 4.446-5.143" />
      <path d="m2 2 20 20" />
    </svg>
  );
}

/** Maximize — zoom a pane to fill (was ⛶). */
export function IconMaximize(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M8 3H5a2 2 0 0 0-2 2v3" />
      <path d="M21 8V5a2 2 0 0 0-2-2h-3" />
      <path d="M3 16v3a2 2 0 0 0 2 2h3" />
      <path d="M16 21h3a2 2 0 0 0 2-2v-3" />
    </svg>
  );
}

/** Minimize — restore a zoomed pane (was ❐). */
export function IconMinimize(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M8 3v3a2 2 0 0 1-2 2H3" />
      <path d="M21 8h-3a2 2 0 0 1-2-2V3" />
      <path d="M3 16h3a2 2 0 0 1 2 2v3" />
      <path d="M16 21v-3a2 2 0 0 1 2-2h3" />
    </svg>
  );
}

/** Hamburger — open the menu / drawer (was ☰). */
export function IconMenu(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M4 12h16" />
      <path d="M4 6h16" />
      <path d="M4 18h16" />
    </svg>
  );
}

/** Share / send-out — a test push or hand-off (was 📤). */
export function IconShare(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M4 12v8a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-8" />
      <polyline points="16 6 12 2 8 6" />
      <line x1="12" x2="12" y1="2" y2="15" />
    </svg>
  );
}

/** Notebook — a Jupyter/notebook edit (was 📓). */
export function IconNotebook(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M2 6h4" />
      <path d="M2 10h4" />
      <path d="M2 14h4" />
      <path d="M2 18h4" />
      <rect width="16" height="20" x="4" y="2" rx="2" />
      <path d="M16 2v20" />
    </svg>
  );
}

/** Info — an informational notice (was a bare "!" / "ⓘ"). */
export function IconInfo(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <circle cx="12" cy="12" r="10" />
      <path d="M12 16v-4" />
      <path d="M12 8h.01" />
    </svg>
  );
}

/** Pause — a skipped / paused run (was ⏸). */
export function IconPause(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <rect x="6" y="4" width="4" height="16" rx="1" />
      <rect x="14" y="4" width="4" height="16" rx="1" />
    </svg>
  );
}

/** Shield — the Warden (opt-in fleet caretaker). */
export function IconShield(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />
    </svg>
  );
}

/** Document with lines — an exported/saved file or report (was 📄). */
export function IconFileText(p: IconProps): React.ReactElement {
  return (
    <svg {...svgProps(p)}>
      <path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7z" />
      <path d="M14 2v5h5" />
      <path d="M8 13h8" />
      <path d="M8 17h8" />
      <path d="M8 9h2" />
    </svg>
  );
}
