// zoneNav.test.tsx pins the APG tabs contract on the experiment workbench
// (web-pages/src/workbench/ZoneNav.tsx + the ExperimentDetailPage tabpanel):
// aria-selected and the roving tabindex follow the active zone, the arrow
// keys move FOCUS only (manual activation — selection fires on Enter/click,
// never on arrows, because a zone switch triggers a fetch + hash change), and
// each tab's aria-controls resolves to the rendered tabpanel whose
// aria-labelledby points back at that tab.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ExperimentDetailPage } from '@saker/grayling-pages/pages/ExperimentDetailPage';
import type { ExperimentDetail } from '@saker/grayling-client';
import { fetchMock, renderPage } from './helpers';

const EXP: ExperimentDetail = {
  id: 'exp-1',
  flagKey: 'checkout',
  state: 'running',
  epoch: 3,
  flagRevision: 7,
  environmentId: 'prod',
  createdBy: 'alice',
  unit: 'user_id',
  variants: ['control', 'treatment'],
  proportions: { control: 50, treatment: 50 },
  primaryMetric: { key: 'checkout_success', type: 'proportion', direction: 'increase' },
  exposureMode: 'onDecision',
  releasePlan: null,
};

function setup(tab?: string) {
  vi.stubGlobal(
    'fetch',
    vi.fn(
      fetchMock(url => {
        if (url.pathname === '/api/v1/admin/experiments/exp-1') return { body: EXP };
        return { status: 404, body: {} };
      }),
    ),
  );
  return renderPage(<ExperimentDetailPage />, {
    path: '/console/:ws/:env/experiments/:expId/:tab?',
    route: '/console/ws1/prod/experiments/exp-1' + (tab ? '/' + tab : ''),
  });
}

function tab(name: string): HTMLElement {
  return document.getElementById('exp-tab-' + name) as HTMLElement;
}

describe('ZoneNav APG tabs', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('aria-selected and the roving tabindex follow the active zone', async () => {
    setup();
    await screen.findByRole('tablist');
    expect(tab('summary').getAttribute('aria-selected')).toBe('true');
    expect(tab('summary').tabIndex).toBe(0);
    for (const name of ['health', 'evidence', 'rollout', 'settings']) {
      expect(tab(name).getAttribute('aria-selected')).toBe('false');
      expect(tab(name).tabIndex).toBe(-1);
    }
  });

  it('a deep-linked zone activates its tab', async () => {
    setup('rollout');
    await screen.findByRole('tablist');
    expect(tab('rollout').getAttribute('aria-selected')).toBe('true');
    expect(tab('rollout').tabIndex).toBe(0);
    expect(tab('summary').getAttribute('aria-selected')).toBe('false');
  });

  it('arrow keys and Home/End move focus without selecting (manual activation)', async () => {
    setup();
    await screen.findByRole('tablist');
    tab('summary').focus();
    await userEvent.keyboard('{ArrowRight}');
    expect(document.activeElement).toBe(tab('health'));
    // Manual activation: focus moved, the active zone did not.
    expect(tab('summary').getAttribute('aria-selected')).toBe('true');
    expect(tab('health').getAttribute('aria-selected')).toBe('false');
    await userEvent.keyboard('{ArrowRight}');
    expect(document.activeElement).toBe(tab('evidence'));
    await userEvent.keyboard('{ArrowLeft}');
    expect(document.activeElement).toBe(tab('health'));
    await userEvent.keyboard('{End}');
    expect(document.activeElement).toBe(tab('settings'));
    await userEvent.keyboard('{Home}');
    expect(document.activeElement).toBe(tab('summary'));
    // Wrap-around at both ends.
    await userEvent.keyboard('{ArrowLeft}');
    expect(document.activeElement).toBe(tab('settings'));
    await userEvent.keyboard('{ArrowRight}');
    expect(document.activeElement).toBe(tab('summary'));
    expect(tab('summary').getAttribute('aria-selected')).toBe('true');
  });

  it('Enter on the focused tab selects it', async () => {
    setup();
    await screen.findByRole('tablist');
    tab('summary').focus();
    await userEvent.keyboard('{ArrowRight}');
    await userEvent.keyboard('{Enter}');
    expect(await screen.findByRole('tab', { name: 'Data health' })).toBeTruthy();
    expect(tab('health').getAttribute('aria-selected')).toBe('true');
    expect(tab('health').tabIndex).toBe(0);
    expect(tab('summary').getAttribute('aria-selected')).toBe('false');
    expect(tab('summary').tabIndex).toBe(-1);
  });

  it('aria-controls of the active tab resolves to the rendered tabpanel', async () => {
    setup('evidence');
    await screen.findByRole('tablist');
    const activeTab = tab('evidence');
    const panel = document.getElementById(activeTab.getAttribute('aria-controls') || '');
    expect(panel).toBeTruthy();
    expect(panel?.getAttribute('role')).toBe('tabpanel');
    expect(panel?.getAttribute('aria-labelledby')).toBe(activeTab.id);
    // The layout hook stays verbatim for the global.css contract.
    expect(document.getElementById('exp-zone')).toBeTruthy();
    expect(document.getElementById('exp-zone')?.contains(panel ?? null)).toBe(true);
  });
});
