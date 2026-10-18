import { useEffect, useState } from 'react';
import type { ReactElement } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { useNavigate, useParams } from 'react-router-dom';
import { ApiError, errMsg } from '@saker/grayling-client';
import type {
  AIResource,
  AIResourceDetail,
  AIResourcesResponse,
  AllowlistEntry,
  Translate,
} from '@saker/grayling-client';
import { Banner } from '../components/Banner';
import { Button } from '../components/Button';
import { Chip } from '../components/Chip';
import { useConfirm } from '../components/ConfirmDialog';
import { DataTable } from '../components/DataTable';
import type { Column } from '../components/DataTable';
import { ErrorBanner } from '../components/ErrorBanner';
import { FormDialog } from '../components/FormDialog';
import { ListSearch } from '../components/ListSearch';
import { Skeleton } from '../components/Skeleton';
import { useToast } from '../components/Toast';
import { Can } from '../host';
import { useHost } from '../host';
import { useApiData } from '../hooks/useApiData';
import { useMutation } from '../hooks/useMutation';
import { consoleViewPath, fmtTime, scopePath, wsPath } from '../utils';

// AiPage ports legacy viewAI (web/legacy/app.js 2837-2988) and
// viewAIResourceDetail/renderAIResourceDetail (2822-2836, 2769-2798): the
// resource catalog with register + ListSearch + revocation table, the resolve
// viewer, the workspace model allowlist editor, and the per-resource detail
// route /console/{ws}/{env}/ai/{type}/{logicalId}. The legacy dual
// desktop/mobile markup is merged into one scrolling DataTable; the legacy
// native prompt() in the pin flow is folded into the pin FormDialog because
// window.prompt is banned. Capability gates mirror legacy gateActions:
// ai.write for register/pin/revoke/reinstate, ai.content.read for
// resolve/allowlist.
const AI_TYPES = ['prompt', 'skill', 'agentspec'];

interface AllowlistRow {
  provider: string;
  model: string;
  enabled: boolean;
}

interface Revocation {
  type: string;
  logicalId: string;
  reason: string;
}

// aiResourcePath is the legacy per-resource API path under the scope.
function aiResourcePath(ws: string, env: string, type: string, logicalId: string): string {
  return scopePath(ws, env) + '/ai/resources/' + encodeURIComponent(type) + '/' + encodeURIComponent(logicalId);
}

// sourceModeLabel is the explicit legacy sourceMode → label map (app.js
// 2772-2776); unknown values fall back to "unavailable".
function sourceModeLabel(mode: string | undefined, t: Translate): string {
  const labels: Record<string, string> = {
    'embedded-gonacos': t('ai.sourceEmbedded'),
    'external-config': t('ai.sourceExternal'),
    unavailable: t('ai.sourceUnavailable'),
  };
  return (mode && labels[mode]) || t('ai.sourceUnavailable');
}

// splitRevocations decodes the legacy revocations map whose keys are
// "type<NUL>logicalId", split at the first NUL (app.js 2854-2857).
function splitRevocations(map: Record<string, string> | undefined): Revocation[] {
  return Object.entries(map || {}).map(([k, reason]) => {
    const i = k.indexOf('\u0000');
    return { type: i < 0 ? k : k.slice(0, i), logicalId: i < 0 ? '' : k.slice(i + 1), reason };
  });
}

// collectAllowlistEntries ports legacy app.js 1990-1997: a fully blank,
// still-enabled row is skipped; a half-filled row fails naming its 1-based
// position; the rest pass through as {provider, model, enabled}.
function collectAllowlistEntries(rows: AllowlistRow[], t: Translate): AllowlistEntry[] {
  return rows.flatMap((row, index) => {
    const provider = row.provider.trim();
    const model = row.model.trim();
    if (!provider && !model && row.enabled !== false) return [];
    if (!provider || !model) {
      throw new ApiError(0, 'validation_failed', t('validation.allowlistRow', { n: index + 1 }));
    }
    return [{ provider, model, enabled: row.enabled }];
  });
}

interface AIActions {
  openPin: (type: string, logicalId: string) => void;
  revoke: (type: string, logicalId: string) => void;
  reinstate: (type: string, logicalId: string) => void;
  dialogs: ReactElement | null;
}

// useAIActions ports handleAIResourceAction (app.js 2800-2820) shared by the
// list rows, the revocation table and the detail view: pin files an ai.pin
// approval request through one FormDialog carrying content + reason;
// revoke/reinstate go through the shared ConfirmDialog with a required
// reason. after() runs after a successful revoke/reinstate (the caller's
// reload); a pin is only an approval request, so legacy did not reload and
// neither do we.
function useAIActions(after: () => void): AIActions {
  const { t } = useTranslation();
  const { client, ws, env } = useHost();
  const { toast } = useToast();
  const confirm = useConfirm();
  const mutate = useMutation();
  const [pinTarget, setPinTarget] = useState<{ type: string; logicalId: string } | null>(null);

  const revoke = async (type: string, logicalId: string) => {
    const approval = await confirm({
      title: t('btn.revoke'),
      summary: t('confirm.revokeResource'),
      reasonLabel: t('prompt.revokeReason', { type, id: logicalId }),
      confirmLabel: t('btn.revoke'),
    });
    if (!approval) return;
    await mutate(
      () =>
        client.api(aiResourcePath(ws, env, type, logicalId) + '/revoke', {
          method: 'POST',
          body: { reason: approval.reason },
        }),
      t('toast.resourceRevoked'),
      after,
    );
  };

  const reinstate = async (type: string, logicalId: string) => {
    const approval = await confirm({
      title: t('btn.reinstate'),
      summary: t('confirm.reinstateResource', { type, id: logicalId }),
      reasonLabel: t('prompt.reinstateReason', { type, id: logicalId }),
      confirmLabel: t('btn.reinstate'),
    });
    if (!approval) return;
    await mutate(
      () =>
        client.api(aiResourcePath(ws, env, type, logicalId) + '/reinstate', {
          method: 'POST',
          body: { reason: approval.reason },
        }),
      t('toast.resourceReinstated'),
      after,
    );
  };

  const pin = async (values: Record<string, string>) => {
    if (!pinTarget) return;
    if (!values.content) throw new ApiError(0, 'validation_failed', t('err.contentRequired'));
    if (!(values.reason || '').trim()) throw new ApiError(0, 'validation_failed', t('toast.reasonRequired'));
    const res = await client.api<{ id?: string }>(scopePath(ws, env) + '/approvals', {
      method: 'POST',
      body: {
        action: 'ai.pin',
        object: pinTarget.type + '/' + pinTarget.logicalId,
        payload: { content: values.content },
        reason: (values.reason || '').trim(),
      },
    });
    toast(t('approval.requested', { id: res.id }));
  };

  return {
    openPin: (type, logicalId) => setPinTarget({ type, logicalId }),
    revoke: (type, logicalId) => void revoke(type, logicalId),
    reinstate: (type, logicalId) => void reinstate(type, logicalId),
    dialogs: pinTarget ? (
      <FormDialog
        title={t('ai.pinTitle', { type: pinTarget.type, id: pinTarget.logicalId })}
        fields={[
          { name: 'content', label: t('ai.content'), type: 'textarea', rows: 10, placeholder: t('ai.contentPh') },
          { name: 'reason', label: t('approval.reason'), required: true },
        ]}
        submitLabel={t('btn.pin')}
        onClose={() => setPinTarget(null)}
        onSubmit={pin}
      />
    ) : null,
  };
}

export function AiPage(): ReactElement {
  const { ws, env } = useHost();
  const params = useParams<{ type?: string; logicalId?: string }>();
  if (!ws || !env) {
    return (
      <Banner kind="warn">
        <Trans i18nKey="banner.noScope" />
      </Banner>
    );
  }
  if (params.type && params.logicalId) {
    return <AIResourceDetailView resourceType={params.type} logicalId={params.logicalId} />;
  }
  return <AIResourceCatalog />;
}

// AIResourceCatalog is the legacy viewAI list: register card, searchable
// resources table plus the revocations table, resolve viewer and the
// workspace model allowlist. A 501 from the resources endpoint collapses the
// whole view to the not-enabled banner, exactly like legacy.
function AIResourceCatalog(): ReactElement {
  const { t, i18n } = useTranslation();
  const { client, ws, env } = useHost();
  const navigate = useNavigate();
  const { toast } = useToast();
  const mutate = useMutation();
  const locale = i18n.language === 'zh' ? 'zh-CN' : 'en-US';
  const { data, loading, error, reload } = useApiData(
    signal => client.api<AIResourcesResponse>(scopePath(ws, env) + '/ai/resources', { signal }),
    [client, ws, env],
  );
  // The allowlist is workspace-level and optional: failures degrade to [].
  const allowlist = useApiData(
    signal =>
      client.api<AllowlistEntry[]>(wsPath(ws) + '/model-allowlist', { signal }).catch(() => [] as AllowlistEntry[]),
    [client, ws],
  );
  const [allowRows, setAllowRows] = useState<AllowlistRow[]>([]);
  useEffect(() => {
    if (allowlist.data) {
      setAllowRows(
        allowlist.data.map(entry => ({
          provider: entry.provider || '',
          model: entry.model || '',
          enabled: entry.enabled !== false,
        })),
      );
    }
  }, [allowlist.data]);
  const [query, setQuery] = useState('');
  const [resolveResult, setResolveResult] = useState<Record<string, unknown> | null>(null);
  const [resolveError, setResolveError] = useState<unknown>(null);
  const actions = useAIActions(reload);

  if (loading) return <Skeleton lines={4} />;
  if (error) {
    if ((error as ApiError).status === 501) return <Banner kind="warn">{t('ai.notEnabled')}</Banner>;
    return <ErrorBanner error={error} />;
  }

  const resources = data?.resources || [];
  const revocations = splitRevocations(data?.revocations);
  const revocationReasons = new Map(revocations.map(item => [item.type + '\u0000' + item.logicalId, item.reason]));
  const q = query.trim().toLocaleLowerCase();
  // searchText mirrors the text the row renders, like legacy textContent.
  const searchText = (r: AIResource): string =>
    [r.type, r.logicalId, r.physicalId || '—', r.owner || '—', fmtTime(r.createdAt, locale)]
      .join(' ')
      .toLocaleLowerCase();
  const visible = q ? resources.filter(r => searchText(r).includes(q)) : resources;

  const register = async () => {
    const type = (document.getElementById('ai-type') as HTMLSelectElement).value;
    const logicalId = (document.getElementById('ai-logical') as HTMLInputElement).value.trim();
    const name = (document.getElementById('ai-name') as HTMLInputElement).value.trim();
    if (!logicalId || !name) {
      toast(t('toast.aiFieldsRequired'), false);
      return;
    }
    await mutate(
      () => client.api(scopePath(ws, env) + '/ai/resources', { method: 'POST', body: { type, logicalId, name } }),
      t('toast.resourceRegistered'),
      reload,
    );
  };

  const resolve = async () => {
    const params = new URLSearchParams({
      type: (document.getElementById('ai-r-type') as HTMLSelectElement).value,
      logicalId: (document.getElementById('ai-r-logical') as HTMLInputElement).value.trim(),
    });
    const ver = (document.getElementById('ai-r-ver') as HTMLInputElement).value.trim();
    if (ver) params.set('version', ver);
    try {
      const res = await client.api<Record<string, unknown>>(scopePath(ws, env) + '/ai/resolve?' + params);
      setResolveResult(res);
      setResolveError(null);
    } catch (e) {
      setResolveResult(null);
      setResolveError(e);
    }
  };

  const saveAllowlist = async () => {
    let entries: AllowlistEntry[];
    try {
      entries = collectAllowlistEntries(allowRows, t);
    } catch (e) {
      toast(errMsg(e as ApiError, t), false);
      return;
    }
    await mutate(
      () => client.api(wsPath(ws) + '/model-allowlist', { method: 'PUT', body: { entries } }),
      t('toast.allowSaved'),
      () => allowlist.reload(),
    );
  };

  const setRow = (index: number, patch: Partial<AllowlistRow>) =>
    setAllowRows(rows => rows.map((row, i) => (i === index ? { ...row, ...patch } : row)));

  const columns: Array<Column<AIResource>> = [
    { key: 'type', header: t('th.type'), cell: r => <Chip label={r.type} cls="teal" /> },
    {
      key: 'logicalId',
      header: t('ai.logicalId'),
      className: 'mono',
      cell: r => (
        <button
          className="row-link"
          type="button"
          data-ai-detail={r.type + '|' + r.logicalId}
          onClick={() => navigate(consoleViewPath(ws, env, 'ai', r.type, r.logicalId))}
        >
          {r.logicalId}
        </button>
      ),
    },
    { key: 'physicalId', header: t('ai.physicalId'), className: 'mono', cell: r => r.physicalId || '—' },
    { key: 'owner', header: t('th.owner'), cell: r => r.owner || '—' },
    { key: 'created', header: t('th.created'), className: 'nowrap', cell: r => fmtTime(r.createdAt, locale) },
    {
      key: 'actions',
      header: t('th.actions'),
      className: 'nowrap',
      cell: r => (
        <Can permission="ai.write">
          <Button size="sm" data-pin={r.type + '|' + r.logicalId} onClick={() => actions.openPin(r.type, r.logicalId)}>
            {t('btn.pinVersion')}
          </Button>{' '}
          {revocationReasons.has(r.type + '\u0000' + r.logicalId) ? (
            <Button
              size="sm"
              data-reinstate={r.type + '|' + r.logicalId}
              onClick={() => actions.reinstate(r.type, r.logicalId)}
            >
              {t('btn.reinstate')}
            </Button>
          ) : (
            <Button
              size="sm"
              variant="danger"
              data-revoke={r.type + '|' + r.logicalId}
              onClick={() => actions.revoke(r.type, r.logicalId)}
            >
              {t('btn.revoke')}
            </Button>
          )}
        </Can>
      ),
    },
  ];

  const revocationColumns: Array<Column<Revocation>> = [
    { key: 'type', header: t('th.type'), cell: rv => <Chip label={rv.type} cls="red" /> },
    { key: 'logicalId', header: t('ai.logicalId'), className: 'mono', cell: rv => rv.logicalId },
    { key: 'reason', header: t('th.reason'), cell: rv => rv.reason || '—' },
    {
      key: 'actions',
      header: '',
      cell: rv => (
        <Can permission="ai.write">
          <Button size="sm" data-reinstate={rv.type + '|' + rv.logicalId} onClick={() => actions.reinstate(rv.type, rv.logicalId)}>
            {t('btn.reinstate')}
          </Button>
        </Can>
      ),
    },
  ];

  return (
    <>
      <div className="view-head">
        <h2>{t('ai.title')}</h2>
      </div>
      <section className="card">
        <h3>{t('ai.register')}</h3>
        <div className="form-grid">
          <label className="field">
            {t('th.type')}
            <select id="ai-type">
              {AI_TYPES.map(type => (
                <option key={type}>{type}</option>
              ))}
            </select>
          </label>
          <label className="field">
            {t('ai.logicalId')}
            <input id="ai-logical" placeholder="checkout-helper" />
          </label>
          <label className="field">
            {t('label.name')}
            <input id="ai-name" />
          </label>
        </div>
        <div className="row">
          <Can permission="ai.write">
            <Button variant="primary" id="ai-reg" onClick={() => void register()}>
              {t('btn.register')}
            </Button>
          </Can>
        </div>
      </section>
      <section className="card">
        <h3>{t('ai.resources', { n: resources.length })}</h3>
        <ListSearch value={query} onChange={setQuery} noMatch={!!q && visible.length === 0} />
        <DataTable
          columns={columns}
          rows={visible}
          rowKey={r => r.type + '\u0000' + r.logicalId}
          emptyText={t('ai.noneRegistered')}
          ariaLabel={t('ai.title')}
          rowProps={() => ({ 'data-list-record': '' })}
        />
        {revocations.length ? (
          <div className="ai-revocations-desktop">
            <h4>{t('ai.revocations')}</h4>
            <DataTable
              columns={revocationColumns}
              rows={revocations}
              rowKey={rv => rv.type + '\u0000' + rv.logicalId}
              emptyText={t('ai.noneRegistered')}
              ariaLabel={t('ai.revocations')}
            />
          </div>
        ) : null}
      </section>
      <section className="card">
        <h3>{t('ai.resolveViewer')}</h3>
        <div className="row">
          <select id="ai-r-type">
            {AI_TYPES.map(type => (
              <option key={type}>{type}</option>
            ))}
          </select>
          <input id="ai-r-logical" placeholder="logicalId" />
          <input id="ai-r-ver" placeholder={t('ai.versionPh')} />
          <Can permission="ai.content.read">
            <Button id="ai-resolve" onClick={() => void resolve()}>
              {t('btn.resolve')}
            </Button>
          </Can>
        </div>
        <div id="ai-r-out">
          {resolveResult ? (
            <>
              <div className="row">
                <Chip label={t('ai.versionChip', { v: (resolveResult.version as string) || '—' })} cls="blue" />{' '}
                <Chip label={t('ai.digestChip', { v: (resolveResult.digest as string) || '—' })} cls="gray" />
              </div>
              <pre className="pre">
                {resolveResult.content !== undefined ? String(resolveResult.content) : JSON.stringify(resolveResult, null, 2)}
              </pre>
            </>
          ) : null}
          {resolveError ? <ErrorBanner error={resolveError} /> : null}
        </div>
      </section>
      <section className="card">
        <h3>{t('ai.allowlist')}</h3>
        <div id="ai-allow">
          {allowRows.length === 0 ? (
            <p className="muted">{t('ai.allowEmpty')}</p>
          ) : (
            allowRows.map((row, index) => (
              <div className="vrow" key={index}>
                <input
                  className="al-provider"
                  placeholder={t('ph.provider')}
                  value={row.provider}
                  onChange={ev => setRow(index, { provider: ev.currentTarget.value })}
                />
                <input
                  className="al-model"
                  placeholder={t('ph.model')}
                  value={row.model}
                  onChange={ev => setRow(index, { model: ev.currentTarget.value })}
                />
                <label className="inline">
                  <input
                    type="checkbox"
                    className="al-enabled"
                    checked={row.enabled}
                    onChange={ev => setRow(index, { enabled: ev.currentTarget.checked })}
                  />{' '}
                  {t('ai.enabled')}
                </label>
                <Button size="sm" aria-label={t('btn.remove')} onClick={() => setAllowRows(rows => rows.filter((_, i) => i !== index))}>
                  {t('btn.remove')}
                </Button>
              </div>
            ))
          )}
        </div>
        <div className="row">
          <Can permission="ai.content.read">
            <Button size="sm" id="ai-allow-add" onClick={() => setAllowRows(rows => [...rows, { provider: '', model: '', enabled: true }])}>
              {t('ai.addEntry')}
            </Button>
            <Button variant="primary" id="ai-allow-save" onClick={() => void saveAllowlist()}>
              {t('ai.saveAllowlist')}
            </Button>
          </Can>
        </div>
      </section>
      {actions.dialogs}
    </>
  );
}

// AIResourceDetailView ports renderAIResourceDetail (app.js 2769-2798): the
// status card (source mode, ids, owner, latest pin, revocation banner), the
// pin/revoke/reinstate actions, and the pinned history card with the
// dependencies disclaimer.
function AIResourceDetailView({ resourceType, logicalId }: { resourceType: string; logicalId: string }): ReactElement {
  const { t, i18n } = useTranslation();
  const { client, ws, env } = useHost();
  const navigate = useNavigate();
  const locale = i18n.language === 'zh' ? 'zh-CN' : 'en-US';
  const { data, loading, error, reload } = useApiData(
    signal => client.api<AIResourceDetail>(aiResourcePath(ws, env, resourceType, logicalId), { signal }),
    [client, ws, env, resourceType, logicalId],
  );
  const actions = useAIActions(reload);

  if (loading) return <Skeleton lines={4} />;
  if (error || !data) return <ErrorBanner error={error} />;
  const detail = data;
  const resource = detail.resource;
  const pins = detail.pins || [];

  return (
    <>
      <div className="view-head">
        <Button size="sm" id="ai-back" onClick={() => navigate(consoleViewPath(ws, env, 'ai'))}>
          {t('ai.back')}
        </Button>
        <h2 className="wrap-anywhere">
          {resource.type} / {resource.logicalId}
        </h2>
        <Chip label={detail.revoked ? t('chip.revoked') : t('chip.active')} cls={detail.revoked ? 'red' : 'green'} />
      </div>
      <section className="card">
        <h3>{t('ai.resourceStatus')}</h3>
        <dl className="detail-grid">
          <dt>{t('ai.source')}</dt>
          <dd>{sourceModeLabel(detail.sourceMode, t)}</dd>
          <dt>{t('ai.physicalId')}</dt>
          <dd className="mono wrap-anywhere">{resource.physicalId}</dd>
          <dt>{t('th.owner')}</dt>
          <dd>{resource.owner}</dd>
          <dt>{t('th.created')}</dt>
          <dd>{fmtTime(resource.createdAt, locale)}</dd>
          <dt>{t('ai.latestPin')}</dt>
          <dd>{pins[0]?.version || '—'}</dd>
        </dl>
        {detail.revoked ? (
          <Banner kind="warn">{t('ai.revocationDetail', { reason: detail.revocationReason || '—' })}</Banner>
        ) : null}
        <p className="muted">{t('ai.sourceNoHealth')}</p>
        <div className="row actions">
          <Can permission="ai.write">
            <Button data-pin="1" onClick={() => actions.openPin(resource.type, resource.logicalId)}>
              {t('btn.pinVersion')}
            </Button>{' '}
            {detail.revoked ? (
              <Button data-reinstate="1" onClick={() => actions.reinstate(resource.type, resource.logicalId)}>
                {t('btn.reinstate')}
              </Button>
            ) : (
              <Button variant="danger" data-revoke="1" onClick={() => actions.revoke(resource.type, resource.logicalId)}>
                {t('btn.revoke')}
              </Button>
            )}
          </Can>
        </div>
      </section>
      <section className="card">
        <h3>{t('ai.pinnedHistory')}</h3>
        {pins.length ? (
          <ol className="ai-pin-history">
            {pins.map(pin => (
              <li key={pin.version}>
                <strong className="mono">{pin.version}</strong>
                <span>{fmtTime(pin.pinnedAt, locale) + ' · ' + pin.pinnedBy}</span>
                <span className="mono wrap-anywhere">{pin.digest}</span>
              </li>
            ))}
          </ol>
        ) : (
          <p className="muted">{t('ai.noPins')}</p>
        )}
        <Banner kind="info">{t('ai.dependenciesUnknown')}</Banner>
      </section>
      {actions.dialogs}
    </>
  );
}
