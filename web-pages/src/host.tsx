// Host bridge for @saker/grayling-pages. The pages package is host-agnostic:
// it cannot import the shell (web/src) without a circular dependency, so the
// shell injects everything a view needs — API client, current scope, the
// capability predicate and a re-auth escape hatch — through this context.
// web/src/pages/ShellLayout.tsx installs the provider with values adapted
// from its Session/Scope/Capability providers.
import { createContext, useContext } from 'react';
import type { ReactElement, ReactNode } from 'react';
import type { Environment, GraylingClient } from '@saker/grayling-client';

export interface HostServices {
  client: GraylingClient;
  ws: string;
  env: string;
  envs: Environment[];
  // actor is the signed-in operator's session name (legacy S.actorName); the
  // approvals views use it for the self-review/break-glass rules.
  actor: string;
  can: (permission: string) => boolean;
  refreshCapabilities: () => Promise<void>;
  // reauth drops the session and returns to the login screen (the legacy
  // showAuth() escape hatch on 403 error banners).
  reauth: () => void;
}

const HostContext = createContext<HostServices | null>(null);

export const HostProvider = HostContext.Provider;

export function useHost(): HostServices {
  const ctx = useContext(HostContext);
  if (!ctx) throw new Error('useHost outside HostProvider');
  return ctx;
}

// useCan/Can are the page-level capability gate, mirroring the shell's
// Can/useCan but sourced from the host bridge (legacy gateActions hid gated
// controls; the server still checks every request).
export function useCan(): (permission: string) => boolean {
  return useHost().can;
}

export function Can({ permission, children }: { permission: string; children: ReactNode }): ReactElement | null {
  const { can } = useHost();
  return can(permission) ? <>{children}</> : null;
}
