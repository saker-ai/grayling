import { useEffect, useState } from 'react';
import type { FormEvent, ReactElement } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { errMsg } from '@saker/grayling-client';
import type { ApiError, WorkspaceQuotas } from '@saker/grayling-client';
import { Banner } from '../components/Banner';
import { Button } from '../components/Button';
import { useConfirm } from '../components/ConfirmDialog';
import { ErrorBanner } from '../components/ErrorBanner';
import { Skeleton } from '../components/Skeleton';
import { useHost } from '../host';
import { useApiData } from '../hooks/useApiData';
import { wsPath } from '../utils';

// QuotasPage ports legacy viewQuotas (owner-only workspace limits). The
// legacy native confirm() before save is replaced by the shared
// ConfirmDialog; on a 403 the capabilities are refreshed (the nav entry may
// vanish). All seven fields are validated as positive safe integers and sent
// together.
const QUOTA_FIELDS: Array<[keyof WorkspaceQuotas, string]> = [
  ['maxFlags', 'quotas.maxFlags'],
  ['maxFlagBytes', 'quotas.maxFlagBytes'],
  ['maxSegments', 'quotas.maxSegments'],
  ['maxExperiments', 'quotas.maxExperiments'],
  ['maxEventBytes', 'quotas.maxEventBytes'],
  ['maxEventsPerBatch', 'quotas.maxEventsPerBatch'],
  ['eventRetention', 'quotas.eventRetention'],
];

export function QuotasPage(): ReactElement {
  const { t } = useTranslation();
  const { client, ws, refreshCapabilities } = useHost();
  const confirm = useConfirm();
  const { data, loading, error, reload } = useApiData(
    signal => (ws ? client.api<WorkspaceQuotas>(wsPath(ws) + '/quotas', { signal }) : Promise.resolve(null)),
    [client, ws],
  );
  const [values, setValues] = useState<Record<keyof WorkspaceQuotas, string> | null>(null);
  const [feedback, setFeedback] = useState('');
  const [saving, setSaving] = useState(false);

  // Seed the controlled inputs when a fresh quota document arrives (initial
  // load, refresh, and after a successful save all take this path).
  useEffect(() => {
    if (!data) return;
    const next = {} as Record<keyof WorkspaceQuotas, string>;
    for (const [key] of QUOTA_FIELDS) next[key] = String(data[key]);
    setValues(next);
  }, [data]);

  if (!ws) {
    return (
      <Banner kind="warn">
        <Trans i18nKey="banner.noWs" />
      </Banner>
    );
  }
  if (loading) return <Skeleton lines={4} />;
  if (error || !data) return <ErrorBanner error={error} />;

  const submit = async (ev: FormEvent<HTMLFormElement>) => {
    ev.preventDefault();
    if (!values) return;
    const body = {} as WorkspaceQuotas;
    for (const [key] of QUOTA_FIELDS) {
      const raw = (values[key] || '').trim();
      const n = Number(raw);
      if (!raw || !Number.isSafeInteger(n) || n <= 0) {
        setFeedback(t('quotas.invalid'));
        return;
      }
      body[key] = n;
    }
    setFeedback('');
    const approval = await confirm({
      title: t('quotas.save'),
      summary: t('quotas.confirm'),
      confirmLabel: t('quotas.save'),
    });
    if (!approval) return;
    setSaving(true);
    try {
      const saved = await client.api<WorkspaceQuotas>(wsPath(ws) + '/quotas', { method: 'PUT', body });
      const next = {} as Record<keyof WorkspaceQuotas, string>;
      for (const [key] of QUOTA_FIELDS) next[key] = String(saved[key]);
      setValues(next);
      setFeedback(t('quotas.saved'));
    } catch (e) {
      setFeedback(errMsg(e as ApiError, t));
      if ((e as ApiError).status === 403) void refreshCapabilities();
    } finally {
      setSaving(false);
    }
  };

  return (
    <>
      <div className="view-head">
        <h2>{t('quotas.title')}</h2>
      </div>
      <section className="card">
        <p className="muted">{t('quotas.hint')}</p>
        <form id="quota-form" onSubmit={ev => void submit(ev)}>
          <div className="form-grid">
            {QUOTA_FIELDS.map(([key, labelKey]) => (
              <label className="field" key={key}>
                {t(labelKey)}
                <input
                  id={'quota-' + key}
                  name={key}
                  type="number"
                  min={1}
                  step={1}
                  required
                  value={values?.[key] ?? ''}
                  // Capture the value now: currentTarget is null by the time
                  // React runs the state updater closure.
                  onChange={ev => {
                    const next = ev.currentTarget.value;
                    setValues(v => (v ? { ...v, [key]: next } : v));
                  }}
                />
              </label>
            ))}
          </div>
          <div id="quota-feedback" role="alert">
            {feedback}
          </div>
          <Button variant="primary" type="submit" id="quota-save" disabled={saving || !values}>
            {t('quotas.save')}
          </Button>{' '}
          <Button id="quota-refresh" onClick={reload}>
            {t('btn.refresh')}
          </Button>
        </form>
      </section>
    </>
  );
}
