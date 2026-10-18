import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';
import type { ReactElement, ReactNode } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import type { Environment, Workspace } from '@saker/grayling-client';
import { useSession } from '../session/SessionProvider';
import { consolePath, useConsoleParams } from '../routes';

// ScopeProvider owns the workspace/environment pair. The URL is the source of
// truth for the current scope (`~` encodes "empty"); localStorage
// grayling_ws/grayling_env only seeds the next boot. Lists are fetched from
// the admin API; an unknown slug falls back to the first entry and the URL is
// corrected, exactly like legacy loadWorkspaces/loadEnvs.
interface ScopeContextValue {
  ws: string;
  env: string;
  workspaces: Workspace[];
  envs: Environment[];
  loading: boolean;
  switchScope: (ws: string, env: string) => void;
  reloadWorkspaces: () => Promise<void>;
  reloadEnvs: () => Promise<void>;
}

const ScopeContext = createContext<ScopeContextValue | null>(null);

export function useScope(): ScopeContextValue {
  const ctx = useContext(ScopeContext);
  if (!ctx) throw new Error('useScope outside ScopeProvider');
  return ctx;
}

function persist(key: string, value: string): void {
  try {
    localStorage.setItem(key, value);
  } catch {
    // storage unavailable
  }
}

export function ScopeProvider({ children }: { children: ReactNode }): ReactElement {
  const { client, session } = useSession();
  const routeParams = useParams<{ ws: string; env: string }>();
  const urlWs = !routeParams.ws || routeParams.ws === '~' ? '' : routeParams.ws;
  const urlEnv = !routeParams.env || routeParams.env === '~' ? '' : routeParams.env;
  const navigate = useNavigate();
  const consoleParams = useConsoleParams();

  const [workspaces, setWorkspaces] = useState<Workspace[]>([]);
  const [loaded, setLoaded] = useState(false);
  // The environment list is keyed by the workspace it was fetched for: until
  // the fetch for the CURRENT workspace settles, the URL env slug is trusted
  // as-is. Judging it against a stale or not-yet-arrived list transiently
  // rewrote cold deep links to `#/console/{ws}/~/…` and fired view fetches
  // with an empty env (404), losing the deep link.
  const [envsState, setEnvsState] = useState<{ ws: string; list: Environment[] }>({ ws: '', list: [] });

  // Load workspaces once per session.
  useEffect(() => {
    if (!session) return;
    const controller = new AbortController();
    setLoaded(false);
    client
      .api<Workspace[]>('/api/v1/admin/workspaces', { signal: controller.signal })
      .then(list => {
        if (controller.signal.aborted) return;
        setWorkspaces(list || []);
        setLoaded(true);
      })
      .catch(() => {
        if (!controller.signal.aborted) setLoaded(true);
      });
    return () => controller.abort();
  }, [client, session]);

  // Effective scope: the URL slug when it exists, else the first entry.
  const ws = loaded && !workspaces.some(w => w.slug === urlWs) ? workspaces[0]?.slug ?? '' : urlWs;

  useEffect(() => {
    if (!session || !loaded || !ws) {
      setEnvsState({ ws: '', list: [] });
      return;
    }
    const controller = new AbortController();
    client
      .api<Environment[]>(
        '/api/v1/admin/workspaces/' + encodeURIComponent(ws) + '/environments',
        { signal: controller.signal },
      )
      .then(list => {
        if (!controller.signal.aborted) setEnvsState({ ws, list: list || [] });
      })
      .catch(() => {
        // capability failures surface when the view fetches its own data
        if (!controller.signal.aborted) setEnvsState({ ws, list: [] });
      });
    return () => controller.abort();
  }, [client, session, loaded, ws]);

  const envsLoaded = envsState.ws === ws;
  const envs = envsState.list;
  const env = envsLoaded && !envs.some(e => e.slug === urlEnv) ? envs[0]?.slug ?? '' : urlEnv;

  // Correct the URL when the deep-linked scope does not exist, and mirror the
  // settled scope into localStorage for the next boot. envsLoaded gates this:
  // until the env list for the current workspace has arrived, `env` still
  // trusts the URL and must not trigger a "correction".
  useEffect(() => {
    if (!loaded || !envsLoaded) return;
    if (ws !== urlWs || env !== urlEnv) {
      const view = consoleParams?.view ?? 'overview';
      navigate(consolePath({ ...emptyParams, ws, env, view }), { replace: true });
      return;
    }
    persist('grayling_ws', ws);
    persist('grayling_env', env);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [loaded, envsLoaded, ws, env, urlWs, urlEnv]);

  const switchScope = useCallback(
    (nextWs: string, nextEnv: string) => {
      persist('grayling_ws', nextWs);
      persist('grayling_env', nextEnv);
      const view = consoleParams?.view ?? 'overview';
      navigate(consolePath({ ...emptyParams, ws: nextWs, env: nextEnv, view }));
    },
    [navigate, consoleParams],
  );

  const reloadWorkspaces = useCallback(async () => {
    const list = await client.api<Workspace[]>('/api/v1/admin/workspaces');
    setWorkspaces(list || []);
  }, [client]);

  const reloadEnvs = useCallback(async () => {
    if (!ws) {
      setEnvsState({ ws: '', list: [] });
      return;
    }
    const list = await client.api<Environment[]>(
      '/api/v1/admin/workspaces/' + encodeURIComponent(ws) + '/environments',
    );
    setEnvsState({ ws, list: list || [] });
  }, [client, ws]);

  const value = useMemo<ScopeContextValue>(
    () => ({ ws, env, workspaces, envs, loading: !loaded, switchScope, reloadWorkspaces, reloadEnvs }),
    [ws, env, workspaces, envs, loaded, switchScope, reloadWorkspaces, reloadEnvs],
  );

  return <ScopeContext.Provider value={value}>{children}</ScopeContext.Provider>;
}

const emptyParams = {
  flagKey: null,
  segKey: null,
  expId: null,
  expTab: 'summary',
  expCreate: false,
  metricRef: null,
  approvalID: null,
  aiRef: null,
  auditObject: '',
};
