import { pruneLayout } from './lib/layout';
import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

describe('app layout styles', () => {
  it('keeps top-row toasts from widening the app past the viewport', () => {
    const styles = readFileSync('src/styles.css', 'utf8');
    const appBlock = styles.match(/\.app\s*\{([^}]*)\}/)?.[1];

    // Top-row toasts must not widen the app past the viewport.
    expect(appBlock).toContain('grid-template-columns: minmax(0, 1fr);');
  });

  it('reserves space for pane menu and close while middle text can shrink', () => {
    const styles = readFileSync('src/styles.css', 'utf8');
    const actions = styles.match(/^\.pane-actions\s*\{([^}]*)\}/m)?.[1];
    expect(actions).toContain('flex-shrink: 0');
    expect(actions).toContain('margin-left: auto');
    const middle = styles.match(/\.pane-header > :is\([^}]+\)\s*\{([^}]*)\}/)?.[1];
    expect(middle).toContain('min-width: 0');
    expect(middle).toContain('flex-shrink: 1');
    expect(middle).toContain('overflow: hidden');
    const unsentLabel = styles.match(/^\.pane-unsent-message-label\s*\{([^}]*)\}/m)?.[1];
    expect(unsentLabel).toContain('min-width: 0');
    expect(unsentLabel).toContain('overflow: hidden');
    const unsentButtons = styles.match(/^\.pane-unsent-message button\s*\{([^}]*)\}/m)?.[1];
    expect(unsentButtons).toContain('flex-shrink: 0');
    const title = styles.match(/^\.pane-title\s*\{([^}]*)\}/m)?.[1];
    expect(title).toContain('min-width: 2.5rem');
    expect(styles).toMatch(/\.pane-header:has\(\.pane-identity-chip\) \.pane-title\s*\{\s*flex: 0 1 auto;/);
    const message = styles.match(/^\.pane-message-chip\s*\{([^}]*)\}/m)?.[1];
    expect(message).toContain('min-width: 0');
    expect(message).toContain('flex: 0 4 auto');
  });

  it('folds new pane header chips in priority order and preserves signals', () => {
    const styles = readFileSync('src/styles.css', 'utf8');
    const tiers = [...styles.matchAll(/@container panehdr \(max-width: ([\d.]+)rem\)\s*\{((?:[^{}]|\{[^{}]*\})*)\}/g)];
    const order = [
      '.pane-cwd', '.pane-identity-working-on', '.pane-status',
      '.pane-identity-chip:not(.is-attention):not(.is-warn)',
      '.pane-message-chip:not(.is-attention):not(.is-warn):not(.unread)',
      '.pane-resume-badge', '.pane-origin:not(.is-attention):not(.is-warn)', '.pane-zoom-btn',
    ];
    const widths = order.map((selector) => {
      const tier = tiers.find((match) => match[2].includes(selector));
      expect(tier, `${selector} must fold in a panehdr tier`).toBeDefined();
      expect(tier?.[2]).toContain('display: none');
      return Number(tier?.[1]);
    });
    widths.slice(1).forEach((width, i) => expect(width).toBeLessThan(widths[i]));
    const workingOn = tiers.find((match) => match[2].includes('.pane-identity-working-on'))?.[2];
    expect(workingOn).toContain('.pane-identity-chip:has(.pane-identity-handle) .pane-identity-working-on');
    const badges = tiers.find((match) => match[2].includes('.pane-resume-badge'))?.[2];
    expect(badges).toContain('.pane-delegated-chip:not(.is-attention)');
    expect(badges).toContain('.pane-delegated-pending-chip:not(.is-attention)');
    expect(badges).toContain('.pane-account-chip:not(.is-warn)');
    for (const tier of tiers) {
      expect(tier[2]).not.toMatch(/\.(?:pane-actions|pane-menu-btn|pane-title|activity-dot)\b/);
    }
  });

  it('keeps native composer geometry and suggestion metrics stable', () => {
    const styles = readFileSync('src/styles.css', 'utf8');
    const composer = readFileSync('src/components/composer/composer.css', 'utf8');
    const ghostBlock = composer.match(/\.composer-ghost\s*\{([^}]*)\}/)?.[1] ?? '';
    const slotBlock = composer.match(/\.composer-primary-slot\s*\{([^}]*)\}/)?.[1] ?? '';
    const nativeSlotBlock = styles.match(/\.native-pane \.composer-primary-slot\s*\{([^}]*)\}/)?.[1] ?? '';

    expect(ghostBlock).toContain('white-space: nowrap');
    expect(ghostBlock).toContain('text-overflow: ellipsis');
    expect(slotBlock).toContain('width: 40px');
    expect(nativeSlotBlock).toContain('width: 30px');
    expect(styles).toContain('--composer-font-size: calc(15px * var(--native-focus-scale');
  });
});

describe('retired Tasks layout migration', () => {
 it('collapses legacy Tasks tiles while preserving other panes', () => {
  const layout = {type:'split',direction:'horizontal',ratio:0.5,a:{type:'tasks',tasksId:'tasks_old',selectedTaskId:null,composing:false},b:{type:'pane',paneId:'p1'}} as const;
  expect(pruneLayout(layout,()=>true)).toEqual({type:'pane',paneId:'p1'});
  expect(pruneLayout(layout.a,()=>true)).toBeNull();
 });
});
