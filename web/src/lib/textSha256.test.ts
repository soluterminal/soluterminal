// @vitest-environment node
import { expect, it } from 'vitest';
import { canHashText, textSha256 } from './textSha256';

// The same values daemon/pane_unsent_hash_test.go pins for unsentTextSha256.
it.each([
  ['make test', '22cc66aa7d2624b4eb4d5b61658614cd6c32477b123afbf7dbc7fc3d7d0f3713'],
  ['echo café ✓ 日本 🚀', 'aa51c94bc53b3c46dcc127e263bca4254b8c3d2ad45654bca9094494dff6642d'],
])('hashes the UTF-8 bytes of %j as lowercase hex', async (text, want) => {
  expect(canHashText()).toBe(true);
  expect(await textSha256(text)).toBe(want);
});
