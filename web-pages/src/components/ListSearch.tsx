import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import { EmptyState } from './EmptyState';

// ListSearch is the legacy bindListSearch contract as a controlled
// component: the input filters the parent's list (the page owns the filter
// predicate over the same text the rows render), and the no-match hint
// (data-list-no-match) appears exactly when a nonempty query leaves zero
// visible rows.
export interface ListSearchProps {
  value: string;
  onChange: (value: string) => void;
  noMatch: boolean;
}

export function ListSearch({ value, onChange, noMatch }: ListSearchProps): ReactElement {
  const { t } = useTranslation();
  return (
    <>
      <input
        className="list-search"
        type="search"
        value={value}
        placeholder={t('list.search')}
        aria-label={t('list.search')}
        onChange={ev => onChange(ev.currentTarget.value)}
      />
      <EmptyState data-list-no-match="" hidden={!noMatch}>
        {t('list.noMatch')}
      </EmptyState>
    </>
  );
}
