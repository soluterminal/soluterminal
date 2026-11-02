import { afterEach, expect, it, vi } from 'vitest';

afterEach(() => { localStorage.clear(); vi.resetModules(); });

it('drops the retired Warden destination while preserving unrelated settings', async () => {
  localStorage.setItem('solu.settings.lastCategory', 'agents');
  localStorage.setItem('solu.settings.lastSection', 'agents.warden');
  localStorage.setItem('unrelated', 'keep');
  vi.resetModules();
  const { useSettings } = await import('./settings');
  expect(useSettings.getState().category).toBe('appearance');
  expect(localStorage.getItem('solu.settings.lastSection')).toBeNull();
  expect(localStorage.getItem('solu.settings.lastCategory')).toBeNull();
  expect(localStorage.getItem('unrelated')).toBe('keep');
});
