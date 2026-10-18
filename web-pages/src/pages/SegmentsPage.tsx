import { useState } from 'react';
import type { ReactElement } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { ApiError } from '@saker/grayling-client';
import type { SegmentDraft } from '@saker/grayling-client';
import { Banner } from '../components/Banner';
import { Button } from '../components/Button';
import { Chip } from '../components/Chip';
import { DataTable } from '../components/DataTable';
import type { Column } from '../components/DataTable';
import { ErrorBanner } from '../components/ErrorBanner';
import { FormDialog } from '../components/FormDialog';
import { ListSearch } from '../components/ListSearch';
import { Skeleton } from '../components/Skeleton';
import { Can } from '../host';
import { useHost } from '../host';
import { useApiData } from '../hooks/useApiData';
import { presetOptions } from '../presets';
import { DRAFT_CHIP, consoleViewPath, fmtTime, resourceContent, resourceKeyError, scopePath } from '../utils';

// SegmentsPage ports the list half of legacy viewSegments: searchable draft
// table, row drill-down into the detail route, and the create entry (a
// FormDialog carrying metadata fields, the key validator and the preset
// picker). The detail editor itself is phase 2; the create dialog hands the
// chosen preset/metadata over via router state, the same contract legacy
// kept in S.segPreset/S.segMetadata.
export interface SegmentCreateState {
  preset: string;
  metadata: { displayName: string; description: string };
}

// segmentSearchText is the set of strings the row renders, concatenated
// exactly like legacy textContent, so a substring query matches the same rows
// bindListSearch would have shown.
function segmentSearchText(d: SegmentDraft, locale: string): string {
  const content = resourceContent(d);
  const name = (content.displayName as string) || d.key;
  const description = (content.description as string) || '';
  return [name, d.key, description, d.state || '', d.risk || '—', d.updatedBy || '—', fmtTime(d.updatedAt, locale)]
    .join('')
    .toLocaleLowerCase();
}

export function SegmentsPage(): ReactElement {
  const { t, i18n } = useTranslation();
  const { client, ws, env } = useHost();
  const navigate = useNavigate();
  const locale = i18n.language === 'zh' ? 'zh-CN' : 'en-US';
  const { data, loading, error } = useApiData(
    signal => (ws && env ? client.api<SegmentDraft[]>(scopePath(ws, env) + '/segments', { signal }) : Promise.resolve(null)),
    [client, ws, env],
  );
  const [query, setQuery] = useState('');
  const [createOpen, setCreateOpen] = useState(false);

  if (!ws || !env) {
    return (
      <Banner kind="warn">
        <Trans i18nKey="banner.noScope" />
      </Banner>
    );
  }
  if (loading) return <Skeleton lines={4} />;
  if (error) return <ErrorBanner error={error} />;

  const drafts = data ?? [];
  const q = query.trim().toLocaleLowerCase();
  const visible = q ? drafts.filter(d => segmentSearchText(d, locale).includes(q)) : drafts;

  const openDetail = (key: string, state?: SegmentCreateState) =>
    navigate(consoleViewPath(ws, env, 'segments', key), state ? { state } : undefined);

  const columns: Array<Column<SegmentDraft>> = [
    {
      key: 'key',
      header: t('th.key'),
      // Card title: name + key (the in-cell description stays table-only via
      // the .dt-card .resource-description rule).
      mobilePrimary: true,
      cell: d => {
        const content = resourceContent(d);
        return (
          <>
            <button className="row-link resource-name" type="button" data-seg={d.key}>
              {(content.displayName as string) || d.key}
            </button>
            <div className="muted mono resource-key">{d.key}</div>
            {content.description ? <p className="muted resource-description">{String(content.description)}</p> : null}
          </>
        );
      },
    },
    { key: 'state', header: t('th.state'), cell: d => <Chip label={d.state} cls={DRAFT_CHIP[d.state || ''] || 'gray'} /> },
    { key: 'risk', header: t('th.risk'), cell: d => d.risk || '—' },
    { key: 'updatedBy', header: t('th.updatedBy'), cell: d => d.updatedBy || '—' },
    { key: 'updated', header: t('th.updated'), className: 'nowrap', cell: d => fmtTime(d.updatedAt, locale) },
  ];

  return (
    <>
      <div className="view-head">
        <h2>{t('seg.title')}</h2>
        <span className="muted">{t('seg.count', { n: drafts.length })}</span>
        <ListSearch value={query} onChange={setQuery} noMatch={!!q && visible.length === 0} />
        <Can permission="flag.write">
          <Button variant="primary" id="seg-new" onClick={() => setCreateOpen(true)}>
            {t('seg.new')}
          </Button>
        </Can>
      </div>
      <DataTable
        columns={columns}
        rows={visible}
        rowKey={d => d.key}
        emptyText={t('seg.empty')}
        mobileCards
        rowProps={d => ({
          'data-list-record': '',
          className: 'clickable',
          onClick: () => openDetail(d.key),
        })}
      />
      {createOpen ? (
        <FormDialog
          title={t('seg.new')}
          fields={[
            {
              name: 'displayName',
              label: t('resource.displayName'),
              hint: t('resource.displayNameHint'),
              validate: value =>
                Array.from(value).length > 128 ? t('editor.displayNameLimit') : '',
            },
            {
              name: 'description',
              label: t('resource.description'),
              type: 'textarea',
              rows: 3,
              validate: value =>
                Array.from(value).length > 2048 ? t('editor.descriptionLimit') : '',
            },
            {
              name: 'key',
              label: t('prompt.newSegKey'),
              required: true,
              placeholder: 'office-network',
              hint: t('key.formatHint'),
              validate: value => resourceKeyError(value, t),
            },
            { name: 'preset', label: t('preset.label'), type: 'select', options: presetOptions(t) },
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
