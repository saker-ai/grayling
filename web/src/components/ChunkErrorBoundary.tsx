import { Component } from 'react';
import type { ReactElement, ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { useLocation } from 'react-router-dom';
import { Banner } from './Banner';
import { Button } from './Button';

// ChunkErrorBoundary keeps a failed lazy route chunk (typically an offline
// first navigation) from tearing down the whole console: the router's default
// boundary would replace the shell itself, leaving no navigation and no
// recovery path. The error card stays inside the outlet, so the sidebar and
// topbar survive. React.lazy caches a rejected import for the page's
// lifetime, so a full reload is the only honest recovery — the button does
// exactly that.
class ChunkErrorBoundaryInner extends Component<{ children: ReactNode }, { failed: boolean }> {
  state = { failed: false };

  static getDerivedStateFromError(): { failed: boolean } {
    return { failed: true };
  }

  componentDidCatch(err: unknown): void {
    console.error('route chunk failed to load', err);
  }

  render(): ReactNode {
    if (this.state.failed) return <ChunkErrorCard />;
    return this.props.children;
  }
}

function ChunkErrorCard(): ReactElement {
  const { t } = useTranslation();
  return (
    <section className="card">
      <Banner kind="err">
        {t('err.chunkLoad')} <Button size="sm" id="btn-reload" onClick={() => window.location.reload()}>{t('btn.reload')}</Button>
      </Banner>
    </section>
  );
}

// RoutedChunkBoundary resets the boundary on every navigation so a later
// route change re-renders cleanly instead of pinning a stale error card.
export function RoutedChunkBoundary({ children }: { children: ReactNode }): ReactElement {
  const location = useLocation();
  return <ChunkErrorBoundaryInner key={location.pathname}>{children}</ChunkErrorBoundaryInner>;
}
