import type { HTMLAttributes, ReactElement } from 'react';

// StickyActionBar keeps an editor's lifecycle row reachable on narrow
// screens: legacy rendered `.row.actions` at the bottom of a very tall card
// (a 24-row JSON source plus the structured form), so on phones the save
// button scrolled out of view. The .sticky-actions rule in global.css is
// mobile-only; on desktop the bar is the plain `.row.actions` it always was.
export function StickyActionBar({ className = '', ...rest }: HTMLAttributes<HTMLDivElement>): ReactElement {
  return <div {...rest} className={'row actions sticky-actions' + (className ? ' ' + className : '')} />;
}
