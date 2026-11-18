import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  arrivalIntent,
  captureArrival,
  cleanTag,
  downloadCountTag,
  readArrival,
  setArrivalIntent,
  signupMetadata,
  trackingRefused,
} from './arrival';
import { resolveSiteInstaller, siteDownloadPage } from './siteInstaller';

function arriveAt(path: string) {
  window.history.replaceState(null, '', path);
  return captureArrival();
}

describe('arrival parameters', () => {
  beforeEach(() => {
    sessionStorage.clear();
    window.history.replaceState(null, '', '/');
  });
  afterEach(() => {
    sessionStorage.clear();
    window.history.replaceState(null, '', '/');
    vi.restoreAllMocks();
  });

  it('keeps the start intent and tags, and strips only them from the URL', () => {
    const arrival = arriveAt('/?intent=start&ref=hn&utm_source=newsletter&b=win#view=tasks');
    expect(arrival).toEqual({ intent: 'start', firstTouch: { ref: 'hn', utm_source: 'newsletter' } });
    expect(window.location.search).toBe('?b=win');
    expect(window.location.hash).toBe('#view=tasks');
    expect(arrivalIntent()).toBe('start');
    // Survives a reload in the same tab (the visitor waits for the code).
    expect(JSON.parse(sessionStorage.getItem('solu.arrival') ?? 'null')).toEqual(arrival);
  });

  it('leaves a URL without arrival parameters alone', () => {
    const replace = vi.spyOn(window.history, 'replaceState');
    arriveAt('/m/abc?b=wsl');
    replace.mockClear();
    captureArrival();
    expect(replace).not.toHaveBeenCalled();
    expect(readArrival()).toEqual({ intent: null, firstTouch: null });
  });

  it('cleans and length-caps tags, and ignores unknown intents', () => {
    expect(cleanTag('<script>alert(1)</script>')).toBe('scriptalert1/script');
    expect(cleanTag('x'.repeat(100))).toHaveLength(40);
    expect(cleanTag('   ')).toBeNull();
    expect(cleanTag(42)).toBeNull();
    const arrival = arriveAt(`/?intent=pay&ref=${encodeURIComponent('hn"><img src=x>')}&utm_campaign=${'c'.repeat(80)}`);
    expect(arrival.intent).toBeNull();
    expect(arrival.firstTouch).toEqual({ ref: 'hnimg srcx', utm_campaign: 'c'.repeat(40) });
    expect(window.location.search).toBe('');
  });

  it('keeps the first tags seen in a tab', () => {
    arriveAt('/?ref=hn');
    arriveAt('/?intent=start&ref=reddit');
    expect(readArrival()).toEqual({ intent: 'start', firstTouch: { ref: 'hn' } });
  });

  it('reads "Sign in" as signin, and the retired download intent as start', () => {
    expect(arriveAt('/?intent=signin').intent).toBe('signin');
    expect(arrivalIntent()).toBe('signin');
    sessionStorage.clear();
    expect(arriveAt('/?intent=download&ref=hn')).toEqual({ intent: 'start', firstTouch: { ref: 'hn' } });
    expect(window.location.search).toBe('');
    // A tab that stored the old value before this release reads it as start.
    sessionStorage.setItem('solu.arrival', JSON.stringify({ intent: 'download', firstTouch: null }));
    expect(arrivalIntent()).toBe('start');
  });

  describe.each([
    ['Do Not Track', () => Object.defineProperty(window.navigator, 'doNotTrack', { configurable: true, get: () => '1' })],
    ['Global Privacy Control', () => Object.defineProperty(window.navigator, 'globalPrivacyControl', { configurable: true, get: () => true })],
    ['an automated browser', () => Object.defineProperty(window.navigator, 'webdriver', { configurable: true, get: () => true })],
  ])('with %s', (_name, refuse) => {
    afterEach(() => {
      for (const key of ['doNotTrack', 'globalPrivacyControl', 'webdriver']) delete (window.navigator as unknown as Record<string, unknown>)[key];
    });

    it('keeps the intent but no tags, strips them from the URL, and sends none with a sign-up', () => {
      refuse();
      expect(trackingRefused()).toBe(true);
      const arrival = arriveAt('/?intent=start&ref=hn&utm_source=newsletter&b=win');
      expect(arrival).toEqual({ intent: 'start', firstTouch: null });
      expect(window.location.search).toBe('?b=win');
      expect(JSON.parse(sessionStorage.getItem('solu.arrival') ?? 'null')).toEqual({ intent: 'start', firstTouch: null });
      expect(signupMetadata()).toEqual({ signup_surface: 'web-start' });
      expect(downloadCountTag()).toBeNull();
    });

    it('ignores tags kept earlier in the tab', () => {
      arriveAt('/?intent=start&utm_source=newsletter');
      expect(downloadCountTag()).toBe('newsletter');
      refuse();
      expect(signupMetadata()).toEqual({ signup_surface: 'web-start' });
      expect(downloadCountTag()).toBeNull();
    });
  });

  it('allows tracking when the browser sends no such signal', () => {
    expect(trackingRefused()).toBe(false);
  });

  it('builds the sign-up metadata and the download-count tag', () => {
    arriveAt('/?intent=start&ref=hn&utm_source=x');
    expect(signupMetadata()).toEqual({
      signup_surface: 'web-start',
      first_touch: { ref: 'hn', utm_source: 'x' },
    });
    expect(downloadCountTag()).toBe('x');
    setArrivalIntent(null);
    expect(signupMetadata()).toEqual({ signup_surface: 'web', first_touch: { ref: 'hn', utm_source: 'x' } });
    expect(arrivalIntent()).toBeNull();
  });

  it('turns a sign-in into a sign-up and keeps the first-touch tags', () => {
    arriveAt('/?intent=signin&ref=hn&utm_campaign=launch');
    setArrivalIntent('start');
    expect(readArrival()).toEqual({ intent: 'start', firstTouch: { ref: 'hn', utm_campaign: 'launch' } });
    expect(signupMetadata()).toEqual({
      signup_surface: 'web-start',
      first_touch: { ref: 'hn', utm_campaign: 'launch' },
    });
  });

  it('drops tampered stored values', () => {
    sessionStorage.setItem('solu.arrival', JSON.stringify({ intent: 'x', firstTouch: { ref: '<b>', evil: 'y' } }));
    expect(readArrival()).toEqual({ intent: null, firstTouch: { ref: 'b' } });
  });
});

describe('site installer config', () => {
  it('links the installer file itself, never the download page or a release host', () => {
    const fallback = 'https://soluterminal.com/download/SoluTerminal-Setup.exe';
    expect(resolveSiteInstaller()).toEqual({ url: fallback, size: '42 MB' });
    expect(resolveSiteInstaller('https://soluterminal.com/download/').url).toBe(fallback);
    expect(resolveSiteInstaller('https://github.com/o/r/releases/download/v1/Setup.exe').url).toBe(fallback);
    expect(resolveSiteInstaller('http://soluterminal.com/x.exe').url).toBe(fallback);
    expect(resolveSiteInstaller('https://www.soluterminal.com/download/SoluTerminal-Setup.exe', '80 MB')).toEqual({
      url: 'https://www.soluterminal.com/download/SoluTerminal-Setup.exe',
      size: '80 MB',
    });
    expect(resolveSiteInstaller(undefined, '<b>big</b>').size).toBe('42 MB');
  });

  it('finds the download page beside the installer', () => {
    expect(siteDownloadPage('https://soluterminal.com/download/SoluTerminal-Setup.exe')).toBe(
      'https://soluterminal.com/download/',
    );
    expect(siteDownloadPage('not a url')).toBe('https://soluterminal.com/download/');
  });
});
