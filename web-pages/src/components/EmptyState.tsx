import type { ReactElement, ReactNode } from 'react';

// EmptyState is the shared empty-list placeholder (legacy rendered a bare
// <p class="muted"> per view). Used standalone and for the list-search
// no-match hint; data-* attributes pass through so markers like
// data-list-no-match keep working.
export interface EmptyStateProps {
  children: ReactNode;
  hidden?: boolean;
  className?: string;
}

export function EmptyState({
  children,
  hidden,
  className,
  ...rest
}: EmptyStateProps & Record<`data-${string}`, string>): ReactElement {
  return (
    <p className={className ? 'muted ' + className : 'muted'} hidden={hidden} {...rest}>
      {children}
    </p>
  );
}
