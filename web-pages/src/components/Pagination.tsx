import { useEffect, useRef } from 'react';
import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from './Button';

// Pagination is the legacy flags-list pager (.list-pagination): previous/next
// buttons around a role=status "Page x of y · n matches" label. Disabled
// states match legacy: previous off on the first page, next off when the
// current page already covers the total. Focus placement is ported too —
// after a page turn the clicked button keeps focus, unless it became
// disabled, in which case focus lands on the opposite button.
export interface PaginationProps {
  page: number;
  pageSize: number;
  total: number;
  onPage: (page: number) => void;
}

export function Pagination({ page, pageSize, total, onPage }: PaginationProps): ReactElement {
  const { t } = useTranslation();
  const pages = Math.max(1, Math.ceil(total / pageSize));
  const prevRef = useRef<HTMLButtonElement>(null);
  const nextRef = useRef<HTMLButtonElement>(null);
  const lastAction = useRef<'prev' | 'next' | null>(null);

  useEffect(() => {
    if (lastAction.current === 'prev') {
      (prevRef.current?.disabled ? nextRef.current : prevRef.current)?.focus();
    } else if (lastAction.current === 'next') {
      (nextRef.current?.disabled ? prevRef.current : nextRef.current)?.focus();
    }
    lastAction.current = null;
  }, [page, total, pageSize]);

  return (
    <div className="row list-pagination">
      <Button
        size="sm"
        ref={prevRef}
        disabled={page === 0}
        onClick={() => {
          lastAction.current = 'prev';
          onPage(page - 1);
        }}
      >
        {t('list.previous')}
      </Button>
      <span role="status">{t('list.page', { page: page + 1, pages, n: total })}</span>
      <Button
        size="sm"
        ref={nextRef}
        disabled={(page + 1) * pageSize >= total}
        onClick={() => {
          lastAction.current = 'next';
          onPage(page + 1);
        }}
      >
        {t('list.next')}
      </Button>
    </div>
  );
}
