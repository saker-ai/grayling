import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';

// Skeleton placeholder block shown while a view loads (replaces the legacy
// "Loading…" text). The bars stay pure decoration (aria-hidden); a
// visually-hidden role=status line sits OUTSIDE that hidden subtree so screen
// readers announce the loading state sighted users only see.
export function Skeleton({ lines = 3 }: { lines?: number }): ReactElement {
  const { t } = useTranslation();
  return (
    <>
      <span className="visually-hidden" role="status">
        {t('a11y.loading')}
      </span>
      <div aria-hidden="true">
        {Array.from({ length: lines }, (_, i) => (
          <div key={i} className="skeleton" />
        ))}
      </div>
    </>
  );
}
