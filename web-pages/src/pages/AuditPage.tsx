import { useEffect, useState } from 'react';
import type { FormEvent, ReactElement } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { useNavigate, useParams } from 'react-router-dom';
import type { AuditEntry } from '@saker/grayling-client';
import { Banner } from '../components/Banner';
import { Button } from '../components/Button';
import { CursorPager } from '../components/CursorPager';
import { DataTable } from '../components/DataTable';
import type { Column } from '../components/DataTable';
import { ErrorBanner } from '../components/ErrorBanner';
import { Skeleton } from '../components/Skeleton';
import { useHost } from '../host';
import { useApiData } from '../hooks/useApiData';
import { consoleViewPath, diagnosticLabel, fmtTime, wsPath } from '../utils';

// AuditPage ports legacy viewAudit: cursor-stack pagination over
// {ws}/audit (the server is asked for limit+1 rows to detect a next page),
// a q= search, and an exact-match object filter that deep-links as
// #/console/{ws}/{env}/audit/{object}. The legacy dual desktop/mobile markup
// is one DataTable whose mobileCards tree serves narrow screens.
interface Pager {
  cursors: Array<string | 0>;
  page: number;
}

export function AuditPage(): ReactElement {
  const { t, i18n } = useTranslation();
  const { client, ws, env } = useHost();
  const navigate = useNavigate();
  const routeParams = useParams<{ object?: string }>();
  const auditObject = routeParams.object || '';
  const locale = i18n.language === 'zh' ? 'zh-CN' : 'en-US';

  const [limit, setLimit] = useState(50);
  const [query, setQuery] = useState('');
  const [draftQuery, setDraftQuery] = useState('');
  const [pager, setPager] = useState<Pager>({ cursors: [0], page: 0 });

  // Arriving at (or leaving) an object deep link restarts pagination, like
  // the legacy route() re-render.
  useEffect(() => {
    setPager({ cursors: [0], page: 0 });
  }, [auditObject]);

  const cursor = pager.cursors[pager.page] || 0;
  const { data, loading, error } = useApiData(
    signal => {
      const params =
        'limit=' +
        (limit + 1) +
        '&q=' +
        encodeURIComponent(query) +
        (auditObject ? '&object=' + encodeURIComponent(auditObject) : '') +
        (cursor ? '&before=' + encodeURIComponent(cursor) : '');
      return client.api<AuditEntry[]>(wsPath(ws) + '/audit?' + params, { signal });
    },
    [client, ws, limit, query, auditObject, cursor],
  );

  if (!ws) {
    return (
      <Banner kind="warn">
        <Trans i18nKey="banner.noWs" />
      </Banner>
    );
  }

  const page = data ?? [];
  const rows = page.slice(0, limit);
  const hasMore = page.length > limit;

  const search = (ev: FormEvent<HTMLFormElement>) => {
    ev.preventDefault();
    setQuery(draftQuery.trim());
    setPager({ cursors: [0], page: 0 });
  };

  const columns: Array<Column<AuditEntry>> = [
    // Card title: time + actor together identify the entry at a glance.
    { key: 'time', header: t('th.time'), className: 'nowrap', mobilePrimary: true, cell: a => fmtTime(a.time, locale) },
    { key: 'actor', header: t('th.actor'), mobilePrimary: true, cell: a => a.actor || '—' },
    {
      key: 'action',
      header: t('th.action'),
      className: 'mono',
      cell: a => (
        <>
          {diagnosticLabel(a.action, t)}{' '}
          <details className="inline-diagnostic">
            <summary>{t('diagnostic.raw')}</summary>
            <code>{a.action}</code>
          </details>
        </>
      ),
    },
    { key: 'object', header: t('th.object'), className: 'mono', cell: a => a.object || '—' },
    { key: 'env', header: t('th.env'), className: 'mono', cell: a => a.environmentId || '—' },
    {
      key: 'detail',
      header: t('th.detail'),
      className: 'detail',
      cell: a => {
        const detail = typeof a.detail === 'string' ? a.detail : JSON.stringify(a.detail ?? '');
        return <span title={detail}>{detail}</span>;
      },
    },
  ];

  return (
    <>
      <div className="view-head">
        <h2>{t('audit.title')}</h2>
        <label className="inline">
          {t('audit.limit')}{' '}
          <select
            id="a-limit"
            value={limit}
            onChange={ev => {
              setLimit(Number(ev.currentTarget.value));
              setPager({ cursors: [0], page: 0 });
            }}
          >
            {[25, 50, 100].map(n => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </label>
        <form id="a-search-form" className="inline-search" onSubmit={search}>
          <input
            id="a-search"
            type="search"
            maxLength={128}
            value={draftQuery}
            placeholder={t('audit.search')}
            aria-label={t('audit.search')}
            onChange={ev => setDraftQuery(ev.currentTarget.value)}
          />
          <Button size="sm" type="submit">
            {t('audit.searchButton')}
          </Button>
        </form>
      </div>
      {auditObject ? (
        <Banner kind="info">
          {t('audit.objectFilter', { object: auditObject })}{' '}
          <Button size="sm" id="a-clear-object" onClick={() => navigate(consoleViewPath(ws, env, 'audit'))}>
            {t('audit.clearObject')}
          </Button>
        </Banner>
      ) : null}
      {loading ? (
        <Skeleton lines={4} />
      ) : error ? (
        <ErrorBanner error={error} />
      ) : (
        <>
          <DataTable
            columns={columns}
            rows={rows}
            rowKey={a => a.id}
            emptyText={t('audit.empty')}
            ariaLabel={t('audit.title')}
            mobileCards
          />
          <CursorPager
            page={pager.page}
            hasMore={hasMore}
            onPrev={() => setPager(p => (p.page > 0 ? { ...p, page: p.page - 1 } : p))}
            onNext={() =>
              setPager(p => {
                if (!hasMore || !rows.length) return p;
                const cursors = p.cursors.slice();
                cursors[p.page + 1] = rows[rows.length - 1].id;
                return { cursors, page: p.page + 1 };
              })
            }
          />
        </>
      )}
    </>
  );
}
