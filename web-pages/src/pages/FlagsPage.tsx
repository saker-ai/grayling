import { useMemo, useState } from 'react';
import type { ReactElement } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { ApiError } from '@saker/grayling-client';
import type { FlagListItem, Translate } from '@saker/grayling-client';
import { Banner } from '../components/Banner';
import { Button } from '../components/Button';
import { Chip } from '../components/Chip';
import { DataTable } from '../components/DataTable';
import type { Column } from '../components/DataTable';
import { ErrorBanner } from '../components/ErrorBanner';
import { FormDialog } from '../components/FormDialog';
import { ListSearch } from '../components/ListSearch';
import { Pagination } from '../components/Pagination';
import { Skeleton } from '../components/Skeleton';
import { Can } from '../host';
import { useHost } from '../host';
import { useApiData } from '../hooks/useApiData';
import { flagPresetOptions } from '../presets';
import { DRAFT_CHIP, consoleViewPath, fmtTime, resourceContent, resourceKeyError, scopePath } from '../utils';

// FlagsPage ports the list half of legacy viewFlags (web/legacy/app.js
// 1288-1367): status-category filter, substring search over the legacy
// resourceSearchText, the legacy sort (running-change first, then
// submitted/approved/editing/published, newest first), client-side
// pagination at 25 rows per page, row drill-down into the detail route and
// the create dialog (metadata + key + preset) that hands the chosen preset
// to the detail editor via router state.

// FlagCreateState is the router-state contract between this page's create
// dialog and FlagDetailPage's new-flag seeding.
export interface FlagCreateState {
  preset: string;
  metadata: { displayName: string; description: string };
}

export type FlagListCategory = 'submitted' | 'approved' | 'editing' | 'published';

// flagListCategory is the legacy category: the draft state when a draft
// exists, else published/editing by whether a published revision exists.
export function flagListCategory(flag: FlagListItem): string {
  return flag.draft?.state || (flag.current ? 'published' : 'editing');
}

const CATEGORY_LABEL_KEYS: Record<string, string> = {
  submitted: 'flags.statusSubmitted',
  approved: 'flags.statusApproved',
  editing: 'flags.statusEditing',
  published: 'chip.published',
};

// flagListStatusLabel is the legacy flagListStatusLabel: the localized label
// for the category, with the raw category as fallback for unknown states.
export function flagListStatusLabel(flag: FlagListItem, t: Translate): string {
  const category = flagListCategory(flag);
  const key = CATEGORY_LABEL_KEYS[category];
  return key ? t(key) : category;
}

// flagRunningChange is the legacy flagRunningChange: a high-risk draft on a
// flag whose published content carries a running experiment.
export function flagRunningChange(flag: FlagListItem): boolean {
  const experiment = flag.current?.content?.experiment as { state?: string } | undefined;
  return experiment?.state === 'running' && flag.draft?.risk === 'high';
}

// flagSearchText is the legacy resourceSearchText for flag rows: key,
// displayName and description joined with spaces, lowercased.
export function flagSearchText(flag: FlagListItem): string {
  const content = resourceContent(flag);
  return [flag.key, content.displayName, content.description]
    .filter(Boolean)
    .join(' ')
    .toLocaleLowerCase();
}

// sortFlagsForList is the legacy sort: running-change flags first, then by
// category rank (submitted, approved, editing, published), newest update
// first, key as the final tiebreak.
export function sortFlagsForList(flags: FlagListItem[]): FlagListItem[] {
  const rank = (flag: FlagListItem): number => {
    if (flagRunningChange(flag)) return 0;
    const ranks: Record<string, number> = { submitted: 1, approved: 2, editing: 3, published: 4 };
    return ranks[flagListCategory(flag)] ?? 5;
  };
  const updated = (flag: FlagListItem): number =>
    Date.parse(flag.draft?.updatedAt || flag.current?.publishedAt || '') || 0;
  return [...flags].sort(
    (a, b) => rank(a) - rank(b) || updated(b) - updated(a) || String(a.key).localeCompare(String(b.key)),
  );
}

export interface FlagListPageResult {
  records: FlagListItem[];
  page: number;
  pages: number;
  total: number;
}

// flagListPage is the legacy filter+paginate helper: category filter and
// substring query first, then a clamped 25-per-page slice.
export function flagListPage(
  flags: FlagListItem[],
  { query = '', category = 'all', page = 0, pageSize = 25 }: { query?: string; category?: string; page?: number; pageSize?: number } = {},
): FlagListPageResult {
  const filtered = flags.filter(
    flag =>
      (category === 'all' || flagListCategory(flag) === category) &&
      (!query || flagSearchText(flag).includes(query.toLocaleLowerCase())),
  );
  const pages = Math.max(1, Math.ceil(filtered.length / pageSize));
  const clamped = Math.max(0, Math.min(page, pages - 1));
  return {
    records: filtered.slice(clamped * pageSize, (clamped + 1) * pageSize),
    page: clamped,
    pages,
    total: filtered.length,
  };
}

const PAGE_SIZE = 25;

export function FlagsPage(): ReactElement {
  const { t, i18n } = useTranslation();
  const { client, ws, env } = useHost();
  const navigate = useNavigate();
  const locale = i18n.language === 'zh' ? 'zh-CN' : 'en-US';
  const { data, loading, error } = useApiData(
    signal => (ws && env ? client.api<FlagListItem[]>(scopePath(ws, env) + '/flags', { signal }) : Promise.resolve(null)),
    [client, ws, env],
  );
  const [query, setQuery] = useState('');
  const [category, setCategory] = useState('all');
  const [page, setPage] = useState(0);
  const [createOpen, setCreateOpen] = useState(false);

  const flags = useMemo(() => sortFlagsForList(data ?? []), [data]);
  const result = useMemo(() => flagListPage(flags, { query, category, page, pageSize: PAGE_SIZE }), [flags, query, category, page]);

  if (!ws || !env) {
    return (
      <Banner kind="warn">
        <Trans i18nKey="banner.noScope" />
      </Banner>
    );
  }
  if (loading) return <Skeleton lines={4} />;
  if (error) return <ErrorBanner error={error} />;

  const openDetail = (key: string, state?: FlagCreateState) =>
    navigate(consoleViewPath(ws, env, 'flags', key), state ? { state } : undefined);

  const columns: Array<Column<FlagListItem>> = [
    {
      key: 'key',
      header: t('th.key'),
      // Card title: name + key (the in-cell description stays table-only via
      // the .dt-card .resource-description rule).
      mobilePrimary: true,
      cell: f => {
        const content = resourceContent(f);
        return (
          <>
            <button className="row-link resource-name" type="button" data-flag={f.key}>
              {(content.displayName as string) || f.key}
            </button>
            <div className="muted mono resource-key">{f.key}</div>
            {content.description ? <p className="muted resource-description">{String(content.description)}</p> : null}
          </>
        );
      },
    },
    {
      key: 'type',
      header: t('th.type'),
      cell: f => ((f.draft?.content?.type as string) || (f.current?.content?.type as string) || '—'),
    },
    {
      key: 'draftState',
      header: t('th.draftState'),
      cell: f => {
        const d = f.draft;
        const c = f.current;
        return (
          <>
            {d ? (
              <>
                <Chip label={flagListStatusLabel(f, t)} cls={DRAFT_CHIP[d.state || ''] || 'gray'} />
                {d.deleted ? <> <Chip label={t('chip.del')} cls="red" /></> : null}
              </>
            ) : c ? (
              <Chip label={t('chip.published')} cls="green" />
            ) : (
              <Chip label={t('chip.none')} cls="gray" />
            )}
            {flagRunningChange(f) ? <> <Chip label={t('flags.runningChange')} cls="red" /></> : null}
          </>
        );
      },
    },
    { key: 'revision', header: t('th.revision'), cell: f => String(f.currentRevision || 0) },
    {
      key: 'updated',
      header: t('th.updated'),
      className: 'nowrap',
      cell: f => fmtTime(f.draft?.updatedAt || f.current?.publishedAt || undefined, locale),
    },
  ];

  return (
    <>
      <div className="view-head">
        <h2>{t('flags.title')}</h2>
        <span className="muted">{t('flags.count', { n: flags.length })}</span>
        <ListSearch value={query} onChange={value => { setQuery(value); setPage(0); }} noMatch={false} />
        <Can permission="flag.write">
          <Button variant="primary" id="flag-new" onClick={() => setCreateOpen(true)}>
            {t('flags.new')}
          </Button>
        </Can>
      </div>
      <div className="row">
        <label className="inline">
          {t('flags.statusFilter')}{' '}
          <select
            id="flag-status-filter"
            value={category}
            onChange={ev => { setCategory(ev.currentTarget.value); setPage(0); }}
          >
            <option value="all">{t('flags.statusAll')}</option>
            <option value="submitted">{t('flags.statusSubmitted')}</option>
            <option value="approved">{t('flags.statusApproved')}</option>
            <option value="editing">{t('flags.statusEditing')}</option>
            <option value="published">{t('flags.statusPublished')}</option>
          </select>
        </label>
        <span className="muted">{t('flags.sortNote')}</span>
      </div>
      <DataTable
        columns={columns}
        rows={result.records}
        rowKey={f => f.key}
        emptyText={t('flags.empty')}
        mobileCards
        rowProps={f => ({
          'data-flag-record': '',
          'data-key': f.key,
          'data-category': flagListCategory(f),
          className: 'clickable',
          onClick: () => openDetail(f.key),
        })}
      />
      {flags.length > 0 ? (
        <Pagination page={result.page} pageSize={PAGE_SIZE} total={result.total} onPage={setPage} />
      ) : null}
      <p className="muted" id="flag-filter-empty" hidden={result.total > 0 || flags.length === 0}>
        {t('flags.noMatch')}
      </p>
      {createOpen ? (
        <FormDialog
          title={t('flags.new')}
          fields={[
            {
              name: 'displayName',
              label: t('resource.displayName'),
              hint: t('resource.displayNameHint'),
              validate: value => (Array.from(value).length > 128 ? t('editor.displayNameLimit') : ''),
            },
            {
              name: 'description',
              label: t('resource.description'),
              type: 'textarea',
              rows: 3,
              validate: value => (Array.from(value).length > 2048 ? t('editor.descriptionLimit') : ''),
            },
            {
              name: 'key',
              label: t('prompt.newFlagKey'),
              required: true,
              placeholder: 'office-network',
              hint: t('key.formatHint'),
              validate: value => resourceKeyError(value, t),
            },
            { name: 'preset', label: t('preset.label'), type: 'select', options: flagPresetOptions(t) },
          ]}
          submitLabel={t('btn.create')}
          onClose={() => setCreateOpen(false)}
          onSubmit={values => {
            const key = values.key.trim();
            const keyIssue = resourceKeyError(key, t);
            if (keyIssue) throw new ApiError(0, 'validation_failed', keyIssue);
            setCreateOpen(false);
            openDetail(key, {
              preset: values.preset || '',
              metadata: { displayName: (values.displayName || '').trim(), description: (values.description || '').trim() },
            });
          }}
        />
      ) : null}
    </>
  );
}
