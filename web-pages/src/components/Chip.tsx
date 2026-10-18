import type { ReactElement, ReactNode } from 'react';

// Chip is the legacy chip() helper as a component: a .chip span with a color
// class (gray default), label rendered as text (never markup).
export function Chip({ label, cls }: { label: ReactNode; cls?: string }): ReactElement {
  return <span className={'chip ' + (cls || 'gray')}>{label}</span>;
}
