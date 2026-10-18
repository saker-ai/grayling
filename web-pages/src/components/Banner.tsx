import type { ReactElement, ReactNode } from 'react';

// Banner maps to the legacy .banner kinds; error banners assert themselves
// (role=alert), the rest are plain text.
export function Banner({ kind, children }: { kind: 'err' | 'warn' | 'ok' | 'info'; children: ReactNode }): ReactElement {
  return (
    <div className={'banner ' + kind} {...(kind === 'err' ? { role: 'alert' } : {})}>
      {children}
    </div>
  );
}
