import { useState } from 'react';
import type { FormEvent, ReactElement } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { useNavigate, useParams } from 'react-router-dom';
import { ApiError } from '@saker/grayling-client';
import type { MetricDefinition, MetricPage, Translate } from '@saker/grayling-client';
import { Banner } from '../components/Banner';
import { Button } from '../components/Button';
import { Chip } from '../components/Chip';
import { useConfirm } from '../components/ConfirmDialog';
import { DataTable } from '../components/DataTable';
import type { Column } from '../components/DataTable';
import { ErrorBanner } from '../components/ErrorBanner';
import { FormDialog } from '../components/FormDialog';
import { Skeleton } from '../components/Skeleton';
import { useToast } from '../components/Toast';
import { Can } from '../host';
import { useHost } from '../host';
import { useApiData } from '../hooks/useApiData';
import { useMutation } from '../hooks/useMutation';
import { consoleViewPath, wsPath } from '../utils';

// MetricsPage ports legacy viewMetrics (web/legacy/app.js 3043-3132): the
// catalog list with q/status search and cursor paging, the create FormDialog
// (validated through metricDefinitionFromForm, a branch-for-branch port of
// the legacy validator), and the version detail with submit/publish/deprecate
// actions. Legacy kept the query in S.metricQuery/S.metricStatus/S.metricCursor
// and the detail selection in S.metricRef; here the query state is local and
// the detail is the route /console/{ws}/{env}/metrics/{key}/{version}.
// METRIC_CHIP and NS_DAY mirror legacy app.js lines 109 and 2038.
const METRIC_CHIP: Record<string, string> = { draft: 'blue', published: 'green', deprecated: 'gray' };
const NS_DAY = 8.64e13;

// MetricDefinitionBody is the POST {ws}/metrics payload built by
// metricDefinitionFromForm.
export interface MetricDefinitionBody {
  key: string;
  displayName: string;
  description: string;
  owner: string;
  source: string;
  dedupUnit: string;
  eventKeys: string[];
  attributionWindow: number;
  spec: Record<string, unknown>;
}

// metricDefinitionFromForm is the legacy validator/builder (app.js
// 2997-3041) ported branch-for-branch: every check throws an ApiError
// carrying the localized message for the same key legacy used, in the same
// order. On success it returns the create body.
export function metricDefinitionFromForm(values: Record<string, string>, t: Translate): MetricDefinitionBody {
  const fail = (key: string): never => {
    throw new ApiError(0, 'validation_failed', t(key));
  };
  const keyPattern = /^[a-z][a-z0-9_.-]{0,127}$/;
  const key = (values.key || '').trim();
  const displayName = (values.displayName || '').trim();
  const owner = (values.owner || '').trim();
  if (!keyPattern.test(key)) fail('metrics.validationKey');
  if (!displayName || !owner) fail('metrics.validationIdentity');
  const eventKeys = (values.eventKeys || '')
    .split(',')
    .map(value => value.trim());
  if (
    !eventKeys.length ||
    eventKeys.length > 32 ||
    new Set(eventKeys).size !== eventKeys.length ||
    eventKeys.some(value => !keyPattern.test(value))
  ) {
    fail('metrics.validationEvents');
  }
  const days = Number(values.windowDays);
  if (!Number.isFinite(days) || days <= 0 || days > 30 || Math.round(days * NS_DAY) <= 0) {
    fail('metrics.validationWindow');
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(values.spec || '');
  } catch {
    fail('metrics.validationSpec');
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) fail('metrics.validationSpec');
  const spec = parsed as Record<string, unknown>;
  if (
    !['proportion', 'mean', 'ratio', 'quantile', 'funnel', 'retention'].includes(spec.type as string) ||
    !['increase', 'decrease'].includes(spec.direction as string)
  ) {
    fail('metrics.validationSpec');
  }
  for (const field of ['key', 'unit', 'numeratorKey', 'denominatorKey', 'covariateKey']) {
    if (spec[field] !== undefined && spec[field] !== null && typeof spec[field] !== 'string') {
      fail('metrics.validationSpec');
    }
  }
  if (
    spec.quantileLevel !== undefined &&
    spec.quantileLevel !== null &&
    (typeof spec.quantileLevel !== 'number' || !Number.isFinite(spec.quantileLevel))
  ) {
    fail('metrics.validationSpec');
  }
  if (spec.key === undefined || spec.key === '') spec.key = eventKeys[0];
  if (!eventKeys.includes(spec.key as string)) fail('metrics.validationSpecKey');
  if (
    spec.type === 'ratio' &&
    (!eventKeys.includes(spec.denominatorKey as string) ||
      (spec.numeratorKey !== undefined &&
        spec.numeratorKey !== '' &&
        !eventKeys.includes(spec.numeratorKey as string)))
  ) {
    fail('metrics.validationRatio');
  }
  if (
    spec.type === 'quantile' &&
    (typeof spec.quantileLevel !== 'number' ||
      !Number.isFinite(spec.quantileLevel) ||
      spec.quantileLevel <= 0 ||
      spec.quantileLevel >= 1)
  ) {
    fail('metrics.validationQuantile');
  }
  for (const field of ['floor', 'ceil']) {
    if (
      spec[field] !== undefined &&
      spec[field] !== null &&
      (typeof spec[field] !== 'number' || !Number.isFinite(spec[field]))
    ) {
      fail('metrics.validationClamp');
    }
  }
  if (spec.floor != null && spec.ceil != null && (spec.floor as number) > (spec.ceil as number)) {
    fail('metrics.validationClamp');
  }
  if (spec.type === 'funnel') {
    const journey = spec.funnel as Record<string, unknown> | null | undefined;
    if (
      !journey ||
      typeof journey !== 'object' ||
      Array.isArray(journey) ||
      spec.retention != null ||
      !Array.isArray(journey.steps) ||
      journey.steps.length < 2 ||
      journey.steps.length > 32 ||
      new Set(journey.steps).size !== journey.steps.length ||
      journey.steps.some(step => !eventKeys.includes(step as string)) ||
      !['user', 'session'].includes(journey.dedupUnit as string) ||
      typeof journey.stepTimeout !== 'number' ||
      !Number.isInteger(journey.stepTimeout) ||
      journey.stepTimeout <= 0 ||
      journey.stepTimeout > 9223372036854775807
    ) {
      fail('metrics.validationJourney');
    }
  } else if (spec.type === 'retention') {
    const journey = spec.retention as Record<string, unknown> | null | undefined;
    if (
      !journey ||
      typeof journey !== 'object' ||
      Array.isArray(journey) ||
      spec.funnel != null ||
      (journey.cohortKey != null && typeof journey.cohortKey !== 'string') ||
      !eventKeys.includes(journey.returnKey as string) ||
      ![1, 7].includes(journey.day as number) ||
      typeof journey.timezone !== 'string' ||
      !journey.timezone.trim()
    ) {
      fail('metrics.validationJourney');
    }
  } else if (spec.funnel != null || spec.retention != null) {
    fail('metrics.validationJourney');
  }
  const hasNonfinite = (value: unknown): boolean =>
    typeof value === 'number'
      ? !Number.isFinite(value)
      : value && typeof value === 'object'
        ? Object.values(value).some(hasNonfinite)
        : false;
  if (hasNonfinite(spec)) fail('metrics.validationSpec');
  return {
    key,
    displayName,
    description: (values.description || '').trim(),
    owner,
    source: 'collect',
    dedupUnit: 'user',
    eventKeys,
    attributionWindow: Math.round(days * NS_DAY),
    spec,
  };
}

export function MetricsPage(): ReactElement {
  const { ws } = useHost();
  const params = useParams<{ key?: string; version?: string }>();
  if (!ws) {
    return (
      <Banner kind="warn">
        <Trans i18nKey="banner.noWs" />
      </Banner>
    );
  }
  if (params.key && params.version) return <MetricVersionDetail metricKey={params.key} version={params.version} />;
  return <MetricCatalog />;
}

// MetricCatalog is the legacy list half: server-side q/status/cursor query
// (limit 50), a row-link drill-down into the version route, and the create
// dialog.
function MetricCatalog(): ReactElement {
  const { t } = useTranslation();
  const { client, ws, env } = useHost();
  const navigate = useNavigate();
  const { toast } = useToast();
  const [q, setQ] = useState('');
  const [status, setStatus] = useState('');
  const [cursor, setCursor] = useState('');
  const [createOpen, setCreateOpen] = useState(false);
  const { data, loading, error } = useApiData(
    signal => {
      const query = new URLSearchParams({ limit: '50' });
      if (q) query.set('q', q);
      if (status) query.set('status', status);
      if (cursor) query.set('cursor', cursor);
      return client.api<MetricPage>(wsPath(ws) + '/metrics?' + query, { signal });
    },
    [client, ws, q, status, cursor],
  );

  if (loading) return <Skeleton lines={4} />;
  if (error) return <ErrorBanner error={error} />;
  const page: MetricPage = data ?? {};
  const rows = page.items || [];

  // The legacy form re-read its inputs on submit and cleared the cursor;
  // here the submit handler copies the uncontrolled inputs into state.
  const search = (ev: FormEvent<HTMLFormElement>) => {
    ev.preventDefault();
    const form = ev.currentTarget;
    const qInput = form.querySelector<HTMLInputElement>('#metric-q');
    const statusInput = form.querySelector<HTMLSelectElement>('#metric-status');
    setQ((qInput?.value || '').trim());
    setStatus(statusInput?.value || '');
    setCursor('');
  };

  const create = async (values: Record<string, string>) => {
    const body = metricDefinitionFromForm(values, t);
    const created = await client.api<MetricDefinition>(wsPath(ws) + '/metrics', { method: 'POST', body });
    toast(t('metrics.created'));
    navigate(consoleViewPath(ws, env, 'metrics', created.key, String(created.version)));
  };

  const columns: Array<Column<MetricDefinition>> = [
    {
      key: 'key',
      header: t('metrics.key'),
      mobilePrimary: true, // card title: the metric key drill-down
      cell: d => (
        <button
          className="row-link mono"
          type="button"
          data-metric-key={d.key}
          data-metric-version={d.version}
          onClick={() => navigate(consoleViewPath(ws, env, 'metrics', d.key, String(d.version)))}
        >
          {d.key}
        </button>
      ),
    },
    { key: 'version', header: t('metrics.version'), cell: d => 'v' + d.version },
    { key: 'displayName', header: t('label.displayName'), cell: d => d.displayName },
    {
      key: 'status',
      header: t('metrics.status'),
      cell: d => <Chip label={d.status} cls={METRIC_CHIP[d.status || ''] || 'gray'} />,
    },
    { key: 'owner', header: t('metrics.owner'), cell: d => d.owner },
  ];

  return (
    <>
      <div className="view-head">
        <h2>{t('metrics.title')}</h2>
        <form id="metric-search" className="inline-search" onSubmit={search}>
          <input
            id="metric-q"
            type="search"
            defaultValue={q}
            placeholder={t('list.search')}
            aria-label={t('list.search')}
          />
          <select id="metric-status" aria-label={t('metrics.status')} defaultValue={status}>
            {['', 'draft', 'published', 'deprecated'].map(value => (
              <option key={value} value={value}>
                {value || t('metrics.all')}
              </option>
            ))}
          </select>
          <Button size="sm" type="submit">
            {t('audit.searchButton')}
          </Button>
        </form>
        <Can permission="experiment.write">
          <Button variant="primary" size="sm" id="metric-new" onClick={() => setCreateOpen(true)}>
            {t('metrics.new')}
          </Button>
        </Can>
      </div>
      <DataTable
        columns={columns}
        rows={rows}
        rowKey={d => d.key + '@' + d.version}
        emptyText={t('metrics.empty')}
        ariaLabel={t('metrics.title')}
        mobileCards
      />
      {page.nextCursor ? (
        <Button size="sm" id="metric-next" onClick={() => setCursor(page.nextCursor || '')}>
          {t('audit.next')}
        </Button>
      ) : null}
      {createOpen ? (
        <FormDialog
          title={t('metrics.new')}
          fields={[
            { name: 'key', label: t('metrics.key'), placeholder: 'activation.complete', required: true },
            { name: 'displayName', label: t('label.displayName'), required: true },
            { name: 'description', label: t('metrics.description'), type: 'textarea', rows: 2 },
            { name: 'owner', label: t('metrics.owner'), required: true },
            { name: 'eventKeys', label: t('metrics.eventKeys'), placeholder: 'activation_complete', required: true },
            { name: 'windowDays', label: t('metrics.windowDays'), type: 'number', value: '7' },
            {
              name: 'spec',
              label: t('metrics.spec'),
              type: 'textarea',
              rows: 5,
              value: JSON.stringify({ type: 'proportion', direction: 'increase' }, null, 2),
            },
          ]}
          submitLabel={t('metrics.create')}
          onClose={() => setCreateOpen(false)}
          onSubmit={create}
        />
      ) : null}
    </>
  );
}

// MetricVersionDetail is the legacy detail half: the definition as a
// detail-grid plus the spec JSON, with submit/publish for drafts and
// deprecate for published versions (gated exactly like legacy gateActions:
// experiment.write for submit, flag.publish for publish/deprecate).
function MetricVersionDetail({ metricKey, version }: { metricKey: string; version: string }): ReactElement {
  const { t } = useTranslation();
  const { client, ws, env } = useHost();
  const navigate = useNavigate();
  const { toast } = useToast();
  const confirm = useConfirm();
  const mutate = useMutation();
  const [publishOpen, setPublishOpen] = useState(false);
  const path = wsPath(ws) + '/metrics/' + encodeURIComponent(metricKey) + '/versions/' + encodeURIComponent(version);
  const { data, loading, error, reload } = useApiData(signal => client.api<MetricDefinition>(path, { signal }), [
    client,
    path,
  ]);

  if (loading) return <Skeleton lines={4} />;
  if (error || !data) return <ErrorBanner error={error} />;
  const def = data;

  const submit = async () => {
    const choice = await confirm({
      title: t('metrics.submit'),
      summary: def.displayName + ' v' + def.version,
      reasonLabel: t('approval.reason'),
      confirmLabel: t('metrics.submit'),
    });
    if (!choice) return;
    await mutate(
      () => client.api(path + '/submit', { method: 'POST', body: { reason: choice.reason } }),
      t('metrics.submitted'),
      () => reload(),
    );
  };

  const deprecate = async () => {
    const choice = await confirm({
      title: t('metrics.deprecate'),
      summary: def.displayName + ' v' + def.version,
      reasonLabel: t('approval.reason'),
      confirmLabel: t('metrics.deprecate'),
    });
    if (!choice) return;
    await mutate(
      () => client.api(path + '/deprecate', { method: 'POST', body: { reason: choice.reason } }),
      t('metrics.deprecated'),
      () => reload(),
    );
  };

  const publish = async (values: Record<string, string>) => {
    const approvalId = (values.approvalId || '').trim();
    if (!approvalId) throw new ApiError(0, 'validation_failed', t('metrics.approvalId'));
    await client.api(path + '/publish', { method: 'POST', body: { approvalId } });
    toast(t('metrics.published'));
    reload();
  };

  return (
    <>
      <div className="view-head">
        <Button size="sm" id="metric-back" onClick={() => navigate(consoleViewPath(ws, env, 'metrics'))}>
          {t('exp.back')}
        </Button>
        <h2>{def.displayName}</h2>
        <Chip label={def.status} cls={METRIC_CHIP[def.status || ''] || 'gray'} />
      </div>
      <section className="card">
        <dl className="detail-grid">
          <dt>{t('metrics.key')}</dt>
          <dd className="mono">{def.key}</dd>
          <dt>{t('metrics.version')}</dt>
          <dd>{'v' + def.version}</dd>
          <dt>{t('metrics.owner')}</dt>
          <dd>{def.owner}</dd>
          <dt>{t('metrics.description')}</dt>
          <dd>{def.description || '—'}</dd>
          <dt>{t('metrics.source')}</dt>
          <dd>{def.source}</dd>
          <dt>{t('metrics.eventKeys')}</dt>
          <dd className="mono">{(def.eventKeys || []).join(', ')}</dd>
          <dt>{t('metrics.windowDays')}</dt>
          <dd>{Number(def.attributionWindow) / NS_DAY}</dd>
          <dt>{t('metrics.hash')}</dt>
          <dd className="mono">{def.hash}</dd>
        </dl>
        <h3>{t('metrics.spec')}</h3>
        <pre className="mono">{JSON.stringify(def.spec, null, 2)}</pre>
        <div className="action-row">
          {def.status === 'draft' ? (
            <>
              <Can permission="experiment.write">
                <Button size="sm" id="metric-submit" onClick={() => void submit()}>
                  {t('metrics.submit')}
                </Button>
              </Can>
              <Can permission="flag.publish">
                <Button variant="primary" size="sm" id="metric-publish" onClick={() => setPublishOpen(true)}>
                  {t('metrics.publish')}
                </Button>
              </Can>
            </>
          ) : null}
          {def.status === 'published' ? (
            <Can permission="flag.publish">
              <Button size="sm" id="metric-deprecate" onClick={() => void deprecate()}>
                {t('metrics.deprecate')}
              </Button>
            </Can>
          ) : null}
        </div>
      </section>
      {publishOpen ? (
        <FormDialog
          title={t('metrics.publish')}
          fields={[{ name: 'approvalId', label: t('metrics.approvalId') }]}
          submitLabel={t('metrics.publish')}
          onClose={() => setPublishOpen(false)}
          onSubmit={publish}
        />
      ) : null}
    </>
  );
}
