import { describe, expect, it } from 'vitest';
import { IOS_ADD_TO_HOME_SCREEN_STEPS, IOS_BLOCKED_NOTIFICATIONS_HELP, IOS_ENABLE_NOTIFICATIONS_HELP } from './iosPush';

describe('iOS push help copy', () => {
  it('Home Screen steps follow Apple\'s Share → Add to Home Screen → Open as Web App → Add flow', () => {
    expect(IOS_ADD_TO_HOME_SCREEN_STEPS).toMatch(/Share button.*Add to Home Screen.*Open as Web App.*tap Add\./);
  });

  it('enable help: add to Home Screen, open from there, turn alerts on', () => {
    const steps = IOS_ENABLE_NOTIFICATIONS_HELP.split('\n').filter((l) => /^\d\./.test(l));
    expect(steps).toHaveLength(3);
    expect(steps[0]).toContain('Add to Home Screen');
    expect(steps[1]).toBe('2. Open SoluTerminal from its new Home Screen icon.');
    expect(steps[2]).toBe('3. Open Alerts there and turn Notifications on.');
  });

  it('blocked help uses the iPhone Notifications path, not the Mac Safari Websites path', () => {
    expect(IOS_BLOCKED_NOTIFICATIONS_HELP).toBe(
      'iPhone / iPad (Home Screen app): Settings → Notifications → SoluTerminal (below Notification Style) → turn on Allow Notifications.',
    );
    expect(IOS_BLOCKED_NOTIFICATIONS_HELP).not.toContain('Websites');
  });
});
