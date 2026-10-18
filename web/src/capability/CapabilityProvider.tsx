import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';
import type { ReactElement, ReactNode } from 'react';
import type { GlobalCapabilities, ScopedCapabilities } from '@saker/grayling-client';
import { ApiError } from '@saker/grayling-client';
import { useSession } from '../session/SessionProvider';
import { useScope } from '../scope/ScopeProvider';

// CapabilityProvider ports legacy refreshCapabilities/can: the global map
// (/api/v1/admin/capabilities) answers workspace.admin; the scoped map
// ({ws}/capabilities) answers everything else. 403/404 on either fetch is a
// soft "no capabilities", anything else is logged but non-fatal — the server
// still checks every request.
interface CapabilityContextValue {
  can: (permission: string) => boolean;
  refresh: () => Promise<void>;
}

const CapabilityContext = createContext<CapabilityContextValue>({
  can: () => false,
  refresh: () => Promise.resolve(),
});

export function useCapabilities(): CapabilityContextValue {
  return useContext(CapabilityContext);
}

export function useCan(): (permission: string) => boolean {
  return useCapabilities().can;
}

export function Can({ permission, children }: { permission: string; children: ReactNode }): ReactElement | null {
  const { can } = useCapabilities();
  return can(permission) ? <>{children}</> : null;
}

export function CapabilityProvider({ children }: { children: ReactNode }): ReactElement {
  const { client, session } = useSession();
  const { ws } = useScope();
  const [global, setGlobal] = useState<GlobalCapabilities | null>(null);
  const [scoped, setScoped] = useState<ScopedCapabilities | null>(null);

  const load = useCallback(
    async (signal?: AbortSignal) => {
      setGlobal(null);
      setScoped(null);
      try {
        const g = await client.api<GlobalCapabilities>('/api/v1/admin/capabilities', { signal });
        if (!signal?.aborted) setGlobal(g);
      } catch (e) {
        const status = (e as ApiError).status;
        if (status !== 403 && status !== 404) console.warn('capabilities: global fetch failed', e);
      }
      if (signal?.aborted || !ws) return;
      try {
        const s = await client.api<ScopedCapabilities>(
          '/api/v1/admin/workspaces/' + encodeURIComponent(ws) + '/capabilities',
          { signal },
        );
        if (!signal?.aborted) setScoped(s);
      } catch (e) {
        const status = (e as ApiError).status;
        if (status !== 403 && status !== 404) console.warn('capabilities: scoped fetch failed', e);
      }
    },
    [client, ws],
  );

  useEffect(() => {
    if (!session) return;
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [session, load]);

  const refresh = useCallback(() => load(), [load]);

  const can = useCallback(
    (permission: string) =>
      permission === 'workspace.admin'
        ? !!(global && global[permission])
        : !!(scoped && scoped.permissions && scoped.permissions[permission]),
    [global, scoped],
  );

  const value = useMemo(() => ({ can, refresh }), [can, refresh]);
  return <CapabilityContext.Provider value={value}>{children}</CapabilityContext.Provider>;
}
