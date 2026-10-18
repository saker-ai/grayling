import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react';
import type { ReactElement, ReactNode } from 'react';
import { baseUrl, sameOriginConsole } from '@saker/grayling-client';
import { client } from '../api';
import { LoginPage } from '../pages/LoginPage';

// SessionProvider restores the console session on mount (GET
// /api/v1/admin/session) and gates the app on it: unauthenticated renders
// LoginPage, whatever the route says. A 401 anywhere clears credentials and
// drops back here via client.onUnauthorized. The re-auth draft (editor
// content captured before a forced re-login) is keyed by principalId — a
// different principal discards it.
export interface SessionState {
  name: string;
  principalId: string;
}

interface SessionContextValue {
  client: typeof client;
  session: SessionState | null;
  status: 'restoring' | 'ready';
  // authNotice is an i18n key the login page shows (e.g. after a 401).
  authNotice: string | null;
  setAuthNotice: (key: string | null) => void;
  login: (url: string, token: string) => Promise<void>;
  logout: () => Promise<void>;
  reauthDraft: unknown;
  setReauthDraft: (draft: unknown) => void;
}

const SessionContext = createContext<SessionContextValue | null>(null);

export function useSession(): SessionContextValue {
  const ctx = useContext(SessionContext);
  if (!ctx) throw new Error('useSession outside SessionProvider');
  return ctx;
}

export function SessionProvider({ children }: { children: ReactNode }): ReactElement {
  const [session, setSession] = useState<SessionState | null>(null);
  const [status, setStatus] = useState<'restoring' | 'ready'>('restoring');
  const [authNotice, setAuthNotice] = useState<string | null>(null);
  const [reauthDraft, setReauthDraft] = useState<unknown>(null);
  const principalRef = useRef('');

  const establish = useCallback((s: SessionState) => {
    // A different principal must never see the previous one's draft.
    if (principalRef.current && principalRef.current !== s.principalId) setReauthDraft(null);
    principalRef.current = s.principalId;
    setSession(s);
    setStatus('ready');
  }, []);

  useEffect(() => {
    client.onUnauthorized = () => {
      setSession(null);
      setAuthNotice('err.tokenRejected401');
      setStatus('ready');
    };
    // Session restore: same-origin consoles probe the cookie session; a
    // cross-origin console has nothing to restore (the memory token is gone
    // after reload by design).
    if (sameOriginConsole()) {
      client.cookieSession = true;
      client
        .fetchSession()
        .then(s => establish(s))
        .catch(() => {
          client.resetCredentials();
          setAuthNotice(null);
          setSession(null);
          setStatus('ready');
        });
    } else {
      setStatus('ready');
    }
    return () => {
      client.onUnauthorized = undefined;
    };
  }, [establish]);

  const login = useCallback(
    async (url: string, token: string) => {
      try {
        localStorage.setItem('grayling_url', url.trim());
      } catch {
        // storage unavailable
      }
      await client.login(token);
      const s = await client.fetchSession();
      setAuthNotice(null);
      establish(s);
    },
    [establish],
  );

  const logout = useCallback(async () => {
    try {
      await client.logout();
    } catch {
      // non-401 logout failures must not trap the operator on the shell
    }
    setReauthDraft(null);
    setAuthNotice(null);
    principalRef.current = '';
    setSession(null);
  }, []);

  const value: SessionContextValue = {
    client,
    session,
    status,
    authNotice,
    setAuthNotice,
    login,
    logout,
    reauthDraft,
    setReauthDraft,
  };

  if (status === 'restoring') {
    return <div className="auth-wrap" aria-busy="true" />;
  }
  return (
    <SessionContext.Provider value={value}>
      {session ? children : <LoginPage />}
    </SessionContext.Provider>
  );
}

// connInfo is the topbar "server · auth mode" readout.
export function connInfo(sessionMode: string): string {
  return (baseUrl() || location.origin) + ' · ' + sessionMode;
}
