import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ApiError } from '@saker/grayling-client';
import type { MetricDefinition } from '@saker/grayling-client';
import { MetricsPage, metricDefinitionFromForm } from '@saker/grayling-pages/pages/MetricsPage';
import en from '../i18n/en';
import { fetchMock, renderPage } from './helpers';

// The unit tests assert the thrown ApiError message equals the en catalog
// string for the key, so this t resolves keys against en.ts directly.
const t = (key: string): string => (en as Record<string, string>)[key] ?? key;

const METRICS: MetricDefinition[] = [
  {
    key: 'activation.complete',
    version: 3,
    displayName: 'Activation completion',
    owner: 'alice',
    status: 'published',
    eventKeys: ['activation_complete'],
    attributionWindow: 7 * 8.64e13,
    hash: 'h1',
    source: 'collect',
    spec: { type: 'proportion', direction: 'increase', key: 'activation_complete' },
  },
  {
    key: 'checkout.complete',
    version: 1,
    displayName: 'Checkout completion',
    owner: 'bob',
    status: 'draft',
    eventKeys: ['checkout_complete'],
    attributionWindow: 7 * 8.64e13,
    hash: 'h2',
    source: 'collect',
    spec: { type: 'proportion', direction: 'increase' },
  },
];

const LIST_ROUTE = { path: '/console/:ws/:env/metrics', route: '/console/ws1/prod/metrics' };

function setup() {
  const calls: Array<{ url: string; init: RequestInit }> = [];
  const mock = vi.fn(
    fetchMock((url, init) => {
      calls.push({ url: url.pathname + url.search, init });
      if (url.pathname.endsWith('/metrics') && init.method === 'POST') {
        return { body: { key: 'checkout.complete', version: 1 } };
      }
      if (url.pathname.endsWith('/metrics')) return { body: { items: METRICS, nextCursor: 'cursor-2' } };
      return { status: 404, body: {} };
    }),
  );
  vi.stubGlobal('fetch', mock);
  return { mock, calls };
}

const DRAFT_DETAIL: MetricDefinition = {
  key: 'checkout.complete',
  version: 1,
  displayName: 'Checkout completion',
  description: 'checkout funnel end',
  owner: 'bob',
  source: 'collect',
  eventKeys: ['checkout_complete'],
  attributionWindow: 7 * 8.64e13,
  hash: 'abc123',
  status: 'draft',
  spec: { type: 'proportion', direction: 'increase', key: 'checkout_complete' },
};

function setupDetail(def: MetricDefinition = DRAFT_DETAIL) {
  const calls: Array<{ url: string; init: RequestInit }> = [];
  const mock = vi.fn(
    fetchMock((url, init) => {
      calls.push({ url: url.pathname + url.search, init });
      if (url.pathname.match(/\/(submit|publish|deprecate)$/) && init.method === 'POST') return { body: {} };
      if (url.pathname.includes('/metrics/')) return { body: def };
      return { status: 404, body: {} };
    }),
  );
  vi.stubGlobal('fetch', mock);
  return { mock, calls };
}

const DETAIL_ROUTE = {
  path: '/console/:ws/:env/metrics/:key/:version',
  route: '/console/ws1/prod/metrics/checkout.complete/1',
};

describe('MetricsPage list', () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => vi.unstubAllGlobals());

  it('renders rows with chips and versions, and navigates on row click', async () => {
    const { calls } = setup();
    renderPage(<MetricsPage />, LIST_ROUTE);
    const link = (await screen.findAllByText('activation.complete'))[0];
    expect(screen.getAllByText('checkout.complete')[0]).toBeTruthy();
    expect(screen.getAllByText('v3')[0]).toBeTruthy();
    expect(screen.getAllByText('v1')[0]).toBeTruthy();
    // The status select also renders draft/published as options, so pick the chip.
    const published = screen.getAllByText('published').find(el => el.className.includes('chip'));
    const draft = screen.getAllByText('draft').find(el => el.className.includes('chip'));
    expect(published?.className).toContain('green');
    expect(draft?.className).toContain('blue');
    expect(calls[0].url).toBe('/api/v1/admin/workspaces/ws1/metrics?limit=50');

    await userEvent.click(link);
    const loc = await screen.findByTestId('location');
    expect(loc.getAttribute('data-pathname')).toBe('/console/ws1/prod/metrics/activation.complete/3');
  });

  it('narrows via q/status params and fetches the cursor page', async () => {
    const { calls } = setup();
    renderPage(<MetricsPage />, LIST_ROUTE);
    await screen.findAllByText('activation.complete');

    await userEvent.type(screen.getByLabelText('Filter this list'), 'checkout');
    await userEvent.selectOptions(screen.getByLabelText('Status'), 'draft');
    await userEvent.click(screen.getByRole('button', { name: 'Search' }));
    await waitFor(() =>
      expect(
        calls.some(c => c.url === '/api/v1/admin/workspaces/ws1/metrics?limit=50&q=checkout&status=draft'),
      ).toBe(true),
    );

    await userEvent.click(await screen.findByRole('button', { name: 'Next' }));
    await waitFor(() =>
      expect(
        calls.some(
          c => c.url === '/api/v1/admin/workspaces/ws1/metrics?limit=50&q=checkout&status=draft&cursor=cursor-2',
        ),
      ).toBe(true),
    );
  });

  it('creates a metric through the dialog and navigates to the new version', async () => {
    const { calls } = setup();
    renderPage(<MetricsPage />, { ...LIST_ROUTE, permissions: ['experiment.write'] });
    await userEvent.click(await screen.findByRole('button', { name: 'New metric' }));
    const dialog = await screen.findByRole('dialog');
    await userEvent.type(within(dialog).getByLabelText('Metric key'), 'checkout.complete');
    await userEvent.type(within(dialog).getByLabelText('Display name'), 'Checkout completion');
    await userEvent.type(within(dialog).getByLabelText('Owner'), 'bob');
    await userEvent.type(within(dialog).getByLabelText('Event keys (comma separated)'), 'checkout_complete');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Create draft' }));

    const post = calls.find(c => c.init.method === 'POST');
    expect(post?.url).toBe('/api/v1/admin/workspaces/ws1/metrics');
    expect(JSON.parse(String(post?.init.body))).toEqual({
      key: 'checkout.complete',
      displayName: 'Checkout completion',
      description: '',
      owner: 'bob',
      source: 'collect',
      dedupUnit: 'user',
      eventKeys: ['checkout_complete'],
      attributionWindow: 7 * 8.64e13,
      spec: { type: 'proportion', direction: 'increase', key: 'checkout_complete' },
    });
    expect(await screen.findByText('Metric draft created.')).toBeTruthy();
    const loc = await screen.findByTestId('location');
    expect(loc.getAttribute('data-pathname')).toBe('/console/ws1/prod/metrics/checkout.complete/1');
  });
});

describe('metricDefinitionFromForm', () => {
  const base: Record<string, string> = {
    key: 'activation.complete',
    displayName: 'Activation completion',
    description: '  ',
    owner: 'alice',
    eventKeys: 'activation_complete',
    windowDays: '7',
    spec: JSON.stringify({ type: 'proportion', direction: 'increase' }),
  };

  function expectFail(values: Record<string, string>, message: string) {
    let thrown: unknown;
    try {
      metricDefinitionFromForm(values, t);
    } catch (e) {
      thrown = e;
    }
    expect(thrown).toBeInstanceOf(ApiError);
    expect((thrown as ApiError).code).toBe('validation_failed');
    expect((thrown as ApiError).message).toBe(message);
  }

  it('builds the create body from a valid minimal form', () => {
    expect(metricDefinitionFromForm(base, t)).toEqual({
      key: 'activation.complete',
      displayName: 'Activation completion',
      description: '',
      owner: 'alice',
      source: 'collect',
      dedupUnit: 'user',
      eventKeys: ['activation_complete'],
      attributionWindow: 7 * 8.64e13,
      spec: { type: 'proportion', direction: 'increase', key: 'activation_complete' },
    });
  });

  it('rejects a bad key', () => {
    expectFail({ ...base, key: 'Bad key' }, en['metrics.validationKey']);
  });

  it('rejects a missing owner', () => {
    expectFail({ ...base, owner: '  ' }, en['metrics.validationIdentity']);
  });

  it('rejects duplicate event keys', () => {
    expectFail({ ...base, eventKeys: 'a_ok, a_ok' }, en['metrics.validationEvents']);
  });

  it('rejects a zero window', () => {
    expectFail({ ...base, windowDays: '0' }, en['metrics.validationWindow']);
  });

  it('rejects a 31-day window', () => {
    expectFail({ ...base, windowDays: '31' }, en['metrics.validationWindow']);
  });

  it('rejects a non-numeric window', () => {
    expectFail({ ...base, windowDays: 'abc' }, en['metrics.validationWindow']);
  });

  it('rejects an invalid JSON spec', () => {
    expectFail({ ...base, spec: 'not json' }, en['metrics.validationSpec']);
  });

  it('rejects a spec key outside the event keys', () => {
    expectFail(
      { ...base, spec: JSON.stringify({ type: 'proportion', direction: 'increase', key: 'other.event' }) },
      en['metrics.validationSpecKey'],
    );
  });

  it('rejects a ratio denominator outside the event keys', () => {
    expectFail(
      { ...base, spec: JSON.stringify({ type: 'ratio', direction: 'increase', denominatorKey: 'other.event' }) },
      en['metrics.validationRatio'],
    );
  });

  it('rejects a quantile level outside (0,1)', () => {
    expectFail(
      { ...base, spec: JSON.stringify({ type: 'quantile', direction: 'increase', quantileLevel: 1.5 }) },
      en['metrics.validationQuantile'],
    );
  });

  it('rejects floor above ceil', () => {
    expectFail(
      { ...base, spec: JSON.stringify({ type: 'mean', direction: 'increase', floor: 5, ceil: 1 }) },
      en['metrics.validationClamp'],
    );
  });

  it('rejects a funnel with fewer than two steps', () => {
    expectFail(
      {
        ...base,
        spec: JSON.stringify({
          type: 'funnel',
          direction: 'increase',
          funnel: { steps: ['activation_complete'], dedupUnit: 'user', stepTimeout: 3600 },
        }),
      },
      en['metrics.validationJourney'],
    );
  });
});

describe('MetricsPage detail', () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => vi.unstubAllGlobals());

  it('renders the definition fields and spec, and submits with a reason', async () => {
    const { calls } = setupDetail();
    renderPage(<MetricsPage />, { ...DETAIL_ROUTE, permissions: ['experiment.write', 'flag.publish'] });
    expect(await screen.findByText('Checkout completion')).toBeTruthy();
    expect(screen.getByText('checkout.complete').className).toContain('mono');
    expect(screen.getByText('v1')).toBeTruthy();
    expect(screen.getByText('bob')).toBeTruthy();
    expect(screen.getByText('checkout funnel end')).toBeTruthy();
    expect(screen.getByText('collect')).toBeTruthy();
    expect(screen.getByText('checkout_complete').className).toContain('mono');
    expect(screen.getByText('7')).toBeTruthy();
    expect(screen.getByText('abc123').className).toContain('mono');
    expect(screen.getByText(/"type": "proportion"/).tagName).toBe('PRE');
    expect(screen.getByText('draft').className).toContain('chip blue');
    // Draft actions: submit (experiment.write) and publish (flag.publish).
    expect(screen.getByRole('button', { name: 'Submit for review' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Publish approved version' })).toBeTruthy();

    await userEvent.click(screen.getByRole('button', { name: 'Submit for review' }));
    const dialog = await screen.findByRole('dialog');
    expect(dialog.textContent).toContain('Checkout completion v1');
    // Reason is required: confirming with an empty reason posts nothing.
    await userEvent.click(within(dialog).getByRole('button', { name: 'Submit for review' }));
    expect(calls.some(c => c.url.endsWith('/submit'))).toBe(false);
    await userEvent.type(within(dialog).getByRole('textbox'), 'ready for review');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Submit for review' }));
    await waitFor(() => expect(calls.some(c => c.url.endsWith('/submit'))).toBe(true));
    const post = calls.find(c => c.url.endsWith('/submit'));
    expect(post?.init.method).toBe('POST');
    expect(JSON.parse(String(post?.init.body))).toEqual({ reason: 'ready for review' });
    expect(await screen.findByText('Metric submitted for review.')).toBeTruthy();
  });

  it('publishes with an approval id through the form dialog', async () => {
    const { calls } = setupDetail();
    renderPage(<MetricsPage />, { ...DETAIL_ROUTE, permissions: ['flag.publish'] });
    await screen.findByText('Checkout completion');
    await userEvent.click(screen.getByRole('button', { name: 'Publish approved version' }));
    const dialog = await screen.findByRole('dialog');
    await userEvent.type(within(dialog).getByLabelText('Approval ID'), 'appr-42');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Publish approved version' }));
    await waitFor(() => expect(calls.some(c => c.url.endsWith('/publish'))).toBe(true));
    const post = calls.find(c => c.url.endsWith('/publish'));
    expect(JSON.parse(String(post?.init.body))).toEqual({ approvalId: 'appr-42' });
    expect(await screen.findByText('Metric version published.')).toBeTruthy();
  });

  it('shows deprecate for a published version and posts the reason', async () => {
    const { calls } = setupDetail({ ...DRAFT_DETAIL, status: 'published' });
    renderPage(<MetricsPage />, { ...DETAIL_ROUTE, permissions: ['flag.publish'] });
    await screen.findByText('Checkout completion');
    expect(screen.queryByRole('button', { name: 'Submit for review' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Publish approved version' })).toBeNull();

    await userEvent.click(screen.getByRole('button', { name: 'Deprecate version' }));
    const dialog = await screen.findByRole('dialog');
    await userEvent.type(within(dialog).getByRole('textbox'), 'superseded by v2');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Deprecate version' }));
    await waitFor(() => expect(calls.some(c => c.url.endsWith('/deprecate'))).toBe(true));
    const post = calls.find(c => c.url.endsWith('/deprecate'));
    expect(JSON.parse(String(post?.init.body))).toEqual({ reason: 'superseded by v2' });
    expect(await screen.findByText('Metric version deprecated.')).toBeTruthy();
  });
});
