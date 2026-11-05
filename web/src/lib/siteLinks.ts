// Public website links shown inside the app. Kept in one place so the
// privacy page and contact address match the site (website/site.config.json).
export const SITE_PRIVACY_URL = 'https://soluterminal.com/privacy/';
// The help pages, opened from the top-bar "?" menu.
export const SITE_HELP_URL = 'https://soluterminal.com/help/';
// The recovery steps for a person who lost every approved browser or app.
export const SITE_HELP_LOST_DEVICES_URL = 'https://soluterminal.com/help/fix-a-problem/#lost-devices';
// The download page: installer, checksum and install steps. Download links
// in the app point here rather than at a release host.
export const SITE_DOWNLOAD_URL = 'https://soluterminal.com/download/';
export const SITE_CONTACT_EMAIL = 'hello@soluterminal.com';
// The hosted app's address as users read and type it (the cloud's custom
// domain). The API base the daemon connects to is configured separately.
export const APP_SITE_URL = 'https://app.soluterminal.com';
// The installer file itself, mirrored on the website. The hosted app's
// get-started card links here rather than to the download page, because the
// download page leads visitors back to the hosted app to create an account.
// The cloud can override both through /api/config.
export const SITE_INSTALLER_URL = 'https://soluterminal.com/download/SoluTerminal-Setup.exe';
// The cloud the static installer (shared/installer/install.sh) pairs with
// unless it is given --cloud.
export const INSTALL_SCRIPT_DEFAULT_CLOUD = 'https://soluterminal-cloud.onrender.com';
// The installer script as attached to the latest release, with its signature
// (install.sh.sig) beside it. The cloud's /install.sh serves the same bytes
// and stays available for networks that block GitHub.
export const INSTALL_SCRIPT_URL =
  'https://github.com/soluterminal/soluterminal-releases/releases/latest/download/install.sh';
export const SITE_INSTALLER_SIZE = '74 MB';
