import type { ReactElement } from 'react';

// Skeleton placeholder block shown while a view loads (replaces the legacy
// "Loading…" text). Pure decoration: hidden from assistive tech, the real
// content replaces it wholesale.
export function Skeleton({ lines = 3 }: { lines?: number }): ReactElement {
  return (
    <div aria-hidden="true">
      {Array.from({ length: lines }, (_, i) => (
        <div key={i} className="skeleton" />
      ))}
    </div>
  );
}
