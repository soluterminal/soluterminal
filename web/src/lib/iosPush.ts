// iOS delivers web push only to a site added to the Home Screen (iOS 16.4+),
// in every iOS browser since they all run WebKit. A Safari tab cannot
// subscribe, so the bell explains how to get push instead of failing quietly.

export function isIosDevice(): boolean {
  if (typeof navigator === 'undefined') return false;
  const ua = navigator.userAgent;
  // iPadOS reports a Mac platform; touch points tell it apart.
  return /iPad|iPhone|iPod/.test(ua) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
}

export function isStandaloneDisplay(): boolean {
  if (typeof window === 'undefined') return false;
  try {
    if (window.matchMedia('(display-mode: standalone)').matches) return true;
  } catch { /* older browser */ }
  // iOS Safari reports Home Screen launches through a non-standard flag.
  return !!(window.navigator as unknown as { standalone?: boolean }).standalone;
}

export function needsHomeScreenForPush(): boolean {
  return isIosDevice() && !isStandaloneDisplay();
}

export const IOS_PUSH_HINT = 'On iPhone, add SoluTerminal to your Home Screen to get push alerts.';

// How to add the site to the Home Screen, for any iOS browser. Apple's iPhone
// User Guide ("Turn a website into an app in Safari"): Share (behind ⋯ in
// some Safari layouts) → Add to Home Screen → Open as Web App → Add. Since
// iOS 16.4 Chrome, Edge and Firefox offer Add to Home Screen in their Share
// menu too.
export const IOS_ADD_TO_HOME_SCREEN_STEPS =
  'Tap your browser\'s Share button (in Safari it can be under ⋯), then Add to Home Screen. ' +
  'If you see Open as Web App, leave it on, then tap Add.';

// Shown when an iPhone/iPad browser tab tries to turn notifications on: tabs
// have no Notification API at all, so the generic "not supported" was wrong.
export const IOS_ENABLE_NOTIFICATIONS_HELP =
  'On iPhone and iPad, notifications work only when SoluTerminal is opened from the Home Screen.\n\n' +
  `1. ${IOS_ADD_TO_HOME_SCREEN_STEPS}\n` +
  '2. Open SoluTerminal from its new Home Screen icon.\n' +
  '3. Open Alerts there and turn Notifications on.\n\n' +
  'In-app toasts still work here.';

// Where iOS keeps a Home Screen web app's notification permission (Apple's
// "Change notification settings on iPhone": Settings → Notifications → app
// below Notification Style → Allow Notifications). Not Settings → Websites,
// which is Safari on Mac.
export const IOS_BLOCKED_NOTIFICATIONS_HELP =
  'iPhone / iPad (Home Screen app): Settings → Notifications → SoluTerminal (below Notification Style) → turn on Allow Notifications.';
