import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from './Button';

// CursorPager is the audit log's cursor pager (legacy viewAudit): a cursor
// stack with page-local prev/next. Previous is disabled on page 0, next when
// the server returned no further rows (hasMore). The parent owns the cursor
// stack; this component is the pure control.
export interface CursorPagerProps {
  page: number;
  hasMore: boolean;
  onPrev: () => void;
  onNext: () => void;
}

export function CursorPager({ page, hasMore, onPrev, onNext }: CursorPagerProps): ReactElement {
  const { t } = useTranslation();
  return (
    <div className="pager">
      <Button size="sm" id="a-prev" disabled={page === 0} onClick={onPrev}>
        {t('audit.previous')}
      </Button>
      <span>{t('audit.page', { n: page + 1 })}</span>
      <Button size="sm" id="a-next" disabled={!hasMore} onClick={onNext}>
        {t('audit.next')}
      </Button>
    </div>
  );
}
