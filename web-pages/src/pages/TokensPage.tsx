import { useState } from 'react';
import type { ReactElement } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { ApiError, errMsg } from '@saker/grayling-client';
import type { CreatedToken, TokenInfo } from '@saker/grayling-client';
import { Banner } from '../components/Banner';
import { Button } from '../components/Button';
import { Chip } from '../components/Chip';
import { useConfirm } from '../components/ConfirmDialog';
import { DataTable } from '../components/DataTable';
import type { Column } from '../components/DataTable';
import { ErrorBanner } from '../components/ErrorBanner';
import { Skeleton } from '../components/Skeleton';
import { useToast } from '../components/Toast';
import { useHost } from '../host';
import { useApiData } from '../hooks/useApiData';
import { useMutation } from '../hooks/useMutation';
import { fmtTime, wsPath } from '../utils';

// TokensPage ports legacy viewTokens: the token list plus the create form.
// The created secret is shown exactly once in a warn banner (the server never
// returns it again); revocation goes through the shared ConfirmDialog with a
// required reason, replacing the legacy actionDialog.
export function TokensPage(): ReactElement {
  const { t, i18n } = useTranslation();
  const { client, ws, envs, can } = useHost();
  const { toast } = useToast();
  const confirm = useConfirm();
  const mutate = useMutation();
  const locale = i18n.language === 'zh' ? 'zh-CN' : 'en-US';
  const { data, loading, error, reload } = useApiData(
    signal =>
      ws && can('token.manage')
        ? client.api<TokenInfo[]>(wsPath(ws) + '/tokens', { signal })
        : Promise.resolve(null),
    [client, ws, can],
  );
  const [secret, setSecret] = useState('');
  const [creating, setCreating] = useState(false);

  if (!ws) {
    return (
      <Banner kind="warn">
        <Trans i18nKey="banner.noWs" />
      </Banner>
    );
  }
  if (!can('token.manage')) {
    return <ErrorBanner error={new ApiError(403, 'forbidden', '')} />;
  }
  if (loading) return <Skeleton lines={4} />;
  if (error) return <ErrorBanner error={error} />;
  const tokens = data ?? [];

  const create = async () => {
    if (creating) return;
    const name = (document.getElementById('t-name') as HTMLInputElement).value.trim();
    const kind = (document.getElementById('t-kind') as HTMLSelectElement).value;
    const role = (document.getElementById('t-role') as HTMLSelectElement).value;
    const environment = (document.getElementById('t-env') as HTMLSelectElement).value;
    if (!name) {
      toast(t('toast.nameRequired'), false);
      return;
    }
    // Legacy validateTokenScope: sdk/collect tokens must name an environment.
    if (['sdk', 'collect'].includes(kind) && !environment) {
      toast(errMsg(new ApiError(0, 'validation_failed', t('validation.tokenEnvironment')), t), false);
      return;
    }
    setCreating(true);
    try {
      const res = await client.api<CreatedToken>(wsPath(ws) + '/tokens', {
        method: 'POST',
        body: { name, kind, role, environment },
      });
      setSecret(res.Secret || '');
      toast(t('toast.tokenCreated'));
      reload();
    } catch (e) {
      toast(errMsg(e as ApiError, t), false);
    } finally {
      setCreating(false);
    }
  };

  const copySecret = () => {
    const inp = document.getElementById('secret-val') as HTMLInputElement;
    inp.select();
    if (navigator.clipboard) {
      void navigator.clipboard.writeText(inp.value).then(() => toast(t('toast.secretCopied')));
    } else {
      document.execCommand('copy');
    }
  };

  const revoke = async (tk: TokenInfo) => {
    const approval = await confirm({
      title: t('btn.revoke'),
      summary: t('confirm.revokeToken', { name: tk.name }),
      reasonLabel: t('prompt.tokenRevokeReason', { name: tk.name }),
      confirmLabel: t('btn.revoke'),
    });
    if (!approval) return;
    await mutate(
      () =>
        client.api('/api/v1/admin/tokens/' + encodeURIComponent(tk.id), {
          method: 'DELETE',
          body: { reason: approval.reason },
        }),
      t('toast.tokenRevoked'),
      () => reload(),
    );
  };

  const columns: Array<Column<TokenInfo>> = [
    { key: 'name', header: t('label.name'), cell: tk => tk.name },
    {
      key: 'kind',
      header: t('label.kind'),
      cell: tk => <Chip label={tk.kind} cls={tk.kind === 'admin' ? 'teal' : tk.kind === 'sdk' ? 'blue' : 'gray'} />,
    },
    { key: 'role', header: t('label.role'), cell: tk => tk.role || '—' },
    { key: 'prefix', header: t('th.prefix'), className: 'mono', cell: tk => tk.prefix || '—' },
    { key: 'env', header: t('th.env'), className: 'mono', cell: tk => tk.environmentId || '—' },
    { key: 'created', header: t('th.created'), className: 'nowrap', cell: tk => fmtTime(tk.createdAt, locale) },
    {
      key: 'status',
      header: t('th.status'),
      cell: tk =>
        tk.revoked ? <Chip label={t('chip.revoked')} cls="red" /> : <Chip label={t('chip.active')} cls="green" />,
    },
    {
      key: 'actions',
      header: '',
      cell: tk =>
        tk.revoked ? null : (
          <Button size="sm" variant="danger" data-rev={tk.id} onClick={() => void revoke(tk)}>
            {t('btn.revoke')}
          </Button>
        ),
    },
  ];

  return (
    <>
      <div className="view-head">
        <h2>{t('tokens.title')}</h2>
        <span className="muted">{t('tokens.count', { n: tokens.length })}</span>
      </div>
      <div id="tok-secret">
        {secret ? (
          <Banner kind="warn">
            <b>{t('tokens.secretWarn')}</b>
            <div className="row">
              <input className="mono grow" id="secret-val" readOnly value={secret} />
              <Button size="sm" id="secret-copy" onClick={copySecret}>
                {t('btn.copy')}
              </Button>
            </div>
          </Banner>
        ) : null}
      </div>
      <section className="card">
        <h3>{t('tokens.create')}</h3>
        <div className="form-grid">
          <label className="field">
            {t('label.name')}
            <input id="t-name" />
          </label>
          <label className="field">
            {t('label.kind')}
            <select id="t-kind">
              <option>admin</option>
              <option>sdk</option>
              <option>collect</option>
            </select>
          </label>
          <label className="field">
            {t('label.role')}
            <select id="t-role">
              <option>owner</option>
              <option>publisher</option>
              <option>analyst</option>
              <option>viewer</option>
            </select>
          </label>
          <label className="field">
            {t('tokens.env')}
            <select id="t-env">
              <option value="">{t('tokens.envNone')}</option>
              {envs.map(v => (
                <option key={v.slug} value={v.slug}>
                  {v.name || v.slug}
                </option>
              ))}
            </select>
          </label>
        </div>
        <div className="row">
          <Button variant="primary" id="t-create" disabled={creating} onClick={() => void create()}>
            {t('btn.create')}
          </Button>
        </div>
      </section>
      <DataTable columns={columns} rows={tokens} rowKey={tk => tk.id} emptyText={t('tokens.empty')} />
    </>
  );
}
