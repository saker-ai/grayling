// Console routing. parseConsoleParams/buildConsoleHash are a branch-for-branch
// port of the legacy consoleHash()/restoreConsoleHash() in web/legacy/app.js —
// the hash format `#/console/{ws}/{env}/{view}/…` is byte-compatible with the
// old console, including `~` empty markers, the `~new` create state and the
// diagnostics → health tab alias.
import { Navigate, createHashRouter, useLocation, useParams } from 'react-router-dom';
import { Suspense, lazy } from 'react';
import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import { Skeleton } from '@saker/grayling-pages/components/Skeleton';
import ShellLayout from './pages/ShellLayout';

export const VIEW_NAMES = [
  'overview', 'flags', 'segments', 'experiments', 'metrics',
  'debug', 'tokens', 'quotas', 'approvals', 'audit', 'ai',
] as const;
export type ViewName = (typeof VIEW_NAMES)[number];
const VIEW_SET: ReadonlySet<string> = new Set(VIEW_NAMES);

// Experiment-detail tabs. 'summary' is the default and carries no hash
// segment; the legacy 'diagnostics' deep link is an alias of 'health' and is
// normalized on restore so old bookmarks keep working.
export const EXP_TABS = ['summary', 'health', 'evidence', 'rollout', 'settings'] as const;
export const EXP_TAB_ALIASES: Record<string, string> = { diagnostics: 'health' };

export interface ConsoleParams {
  ws: string;
  env: string;
  view: ViewName;
  flagKey: string | null;
  segKey: string | null;
  expId: string | null;
  expTab: string;
  expCreate: boolean;
  metricRef: { key: string; version: number } | null;
  approvalID: string | null;
  aiRef: { type: string; logicalId: string } | null;
  auditObject: string;
}

function defaults(ws: string, env: string, view: ViewName): ConsoleParams {
  return {
    ws, env, view,
    flagKey: null, segKey: null, expId: null, expTab: 'summary', expCreate: false,
    metricRef: null, approvalID: null, aiRef: null, auditObject: '',
  };
}

// parseConsoleParams restores one console state from a raw location.hash;
// it returns null for every shape the legacy restore rejected.
export function parseConsoleParams(hash: string): ConsoleParams | null {
  if (!hash.startsWith('#/console/')) return null;
  let parts: string[];
  try {
    parts = hash.slice(2).split('/').map(decodeURIComponent);
  } catch {
    return null;
  }
  if (parts.length < 4 || parts[0] !== 'console' || !VIEW_SET.has(parts[3])) return null;
  const view = parts[3] as ViewName;
  if (view === 'experiments' && (parts.length > 6 ||
    (parts[5] && (parts[4] === '~new' ||
      !(EXP_TABS.includes(parts[5] as (typeof EXP_TABS)[number]) ||
        Object.hasOwn(EXP_TAB_ALIASES, parts[5])))))) return null;
  if (view === 'ai' && parts.length !== 4 && parts.length !== 6) return null;
  if (view === 'audit' && parts.length > 5) return null;
  const params = defaults(
    parts[1] === '~' ? '' : parts[1],
    parts[2] === '~' ? '' : parts[2],
    view,
  );
  if (view === 'audit') params.auditObject = parts[4] || '';
  if (view === 'flags' && parts[4]) params.flagKey = parts[4];
  if (view === 'segments' && parts[4]) params.segKey = parts[4];
  if (view === 'experiments' && parts[4]) {
    if (parts[4] === '~new') params.expCreate = true;
    else {
      params.expId = parts[4];
      if (parts[5]) params.expTab = EXP_TAB_ALIASES[parts[5]] || parts[5];
    }
  }
  if (view === 'metrics' && parts[4] && /^[1-9]\d*$/.test(parts[5] || '')) {
    params.metricRef = { key: parts[4], version: Number(parts[5]) };
  }
  if (view === 'approvals' && parts[4]) params.approvalID = parts[4];
  if (view === 'ai' && parts[4] && parts[5]) params.aiRef = { type: parts[4], logicalId: parts[5] };
  return params;
}

// buildConsoleHash is the inverse of parseConsoleParams (legacy consoleHash).
export function buildConsoleHash(p: ConsoleParams): string {
  const parts = ['console', p.ws || '~', p.env || '~', p.view];
  if (p.view === 'flags' && p.flagKey) parts.push(p.flagKey);
  if (p.view === 'segments' && p.segKey) parts.push(p.segKey);
  if (p.view === 'experiments' && p.expId) {
    parts.push(p.expId);
    if (p.expTab && p.expTab !== 'summary') parts.push(p.expTab);
  }
  if (p.view === 'experiments' && p.expCreate) parts.push('~new');
  if (p.view === 'metrics' && p.metricRef) parts.push(p.metricRef.key, String(p.metricRef.version));
  if (p.view === 'approvals' && p.approvalID) parts.push(p.approvalID);
  if (p.view === 'ai' && p.aiRef) parts.push(p.aiRef.type, p.aiRef.logicalId);
  if (p.view === 'audit' && p.auditObject) parts.push(p.auditObject);
  return '#/' + parts.map(encodeURIComponent).join('/');
}

// consolePath is buildConsoleHash without the '#', the shape navigate() wants.
export function consolePath(p: ConsoleParams): string {
  return buildConsoleHash(p).slice(1);
}

// useConsoleParams parses the current hash route through the same pure
// function the tests pin, so the shell and the parser can never drift apart.
export function useConsoleParams(): ConsoleParams | null {
  const location = useLocation();
  return parseConsoleParams('#' + location.pathname);
}

const VIEW_TITLE_KEYS: Record<ViewName, string> = {
  overview: 'nav.overview',
  flags: 'nav.flags',
  segments: 'nav.segments',
  experiments: 'nav.experiments',
  metrics: 'nav.metrics',
  debug: 'nav.debug',
  tokens: 'nav.tokens',
  quotas: 'nav.quotas',
  approvals: 'nav.approvals',
  audit: 'nav.audit',
  ai: 'nav.ai',
};

// PlaceholderPage stands in for every view until phases 1–3 land the real
// ones: it shows the localized view title and the raw route parameters.
function PlaceholderPage({ view }: { view: ViewName }): ReactElement {
  const { t } = useTranslation();
  const params = useParams();
  return (
    <section className="card">
      <h2>{t(VIEW_TITLE_KEYS[view])}</h2>
      <p className="muted">placeholder</p>
      <pre className="pre small">{JSON.stringify(params, null, 2)}</pre>
    </section>
  );
}

// HashGuard rejects hashes the legacy console rejected (unknown view, bad
// experiment tab, …) by bouncing to the scope's overview.
function HashGuard(): ReactElement | null {
  const location = useLocation();
  const params = useConsoleParams();
  if (location.pathname.startsWith('/console/') && params === null) {
    const parts = location.pathname.split('/');
    const ws = parts[2] || '~';
    const env = parts[3] || '~';
    return <Navigate to={`/console/${ws}/${env}/overview`} replace />;
  }
  return null;
}

// CatchAll handles shapes the route table has no pattern for but the legacy
// console still accepted (extra trailing segments, e.g. flags/{key}/extra):
// render the parsed view when parseConsoleParams accepts the hash, otherwise
// bounce like HashGuard.
function CatchAll(): ReactElement {
  const location = useLocation();
  const params = useConsoleParams();
  if (params) return <PlaceholderPage view={params.view} />;
  const parts = location.pathname.split('/');
  return <Navigate to={`/console/${parts[2] || '~'}/${parts[3] || '~'}/overview`} replace />;
}

function ShellWithGuard(): ReactElement {
  return (
    <ShellLayout guard={<HashGuard />} />
  );
}

// RootRedirect lands `/` on the restored scope (localStorage) or, failing
// that, on an empty scope — the scope selectors fill in from there.
function RootRedirect(): ReactElement {
  let ws = '~';
  let env = '~';
  try {
    ws = localStorage.getItem('grayling_ws') || '~';
    env = localStorage.getItem('grayling_env') || '~';
  } catch {
    // storage unavailable
  }
  return <Navigate to={`/console/${encodeURIComponent(ws)}/${encodeURIComponent(env)}/overview`} replace />;
}

// Real pages come from @saker/grayling-pages via SUBPATH imports: importing
// the barrel here would pull every page into the entry chunk and defeat
// route-level code splitting. Each lazy() is its own Vite chunk; Suspense
// shows the Skeleton fallback (the legacy single-line "Loading…" is gone).
// Pages are named exports in the package; the .then maps them to the default
// export React.lazy wants (same shape the ./pages stubs re-export).
const OverviewPage = lazy(() => import('@saker/grayling-pages/pages/OverviewPage').then(m => ({ default: m.OverviewPage })));
const ApprovalsPage = lazy(() => import('@saker/grayling-pages/pages/ApprovalsPage').then(m => ({ default: m.ApprovalsPage })));
const ApprovalDetailPage = lazy(() => import('@saker/grayling-pages/pages/ApprovalDetailPage').then(m => ({ default: m.ApprovalDetailPage })));
const MetricsPage = lazy(() => import('@saker/grayling-pages/pages/MetricsPage').then(m => ({ default: m.MetricsPage })));
const AiPage = lazy(() => import('@saker/grayling-pages/pages/AiPage').then(m => ({ default: m.AiPage })));
const FlagDetailPage = lazy(() => import('@saker/grayling-pages/pages/FlagDetailPage').then(m => ({ default: m.FlagDetailPage })));
const SegmentDetailPage = lazy(() => import('@saker/grayling-pages/pages/SegmentDetailPage').then(m => ({ default: m.SegmentDetailPage })));
const DebugPage = lazy(() => import('@saker/grayling-pages/pages/DebugPage').then(m => ({ default: m.DebugPage })));
const QuotasPage = lazy(() => import('@saker/grayling-pages/pages/QuotasPage').then(m => ({ default: m.QuotasPage })));
const TokensPage = lazy(() => import('@saker/grayling-pages/pages/TokensPage').then(m => ({ default: m.TokensPage })));
const AuditPage = lazy(() => import('@saker/grayling-pages/pages/AuditPage').then(m => ({ default: m.AuditPage })));
const SegmentsPage = lazy(() => import('@saker/grayling-pages/pages/SegmentsPage').then(m => ({ default: m.SegmentsPage })));
const ExperimentsPage = lazy(() => import('@saker/grayling-pages/pages/ExperimentsPage').then(m => ({ default: m.ExperimentsPage })));
const FlagsPage = lazy(() => import('@saker/grayling-pages/pages/FlagsPage').then(m => ({ default: m.FlagsPage })));
const ExperimentCreatePage = lazy(
  () => import('@saker/grayling-pages/pages/ExperimentCreatePage').then(m => ({ default: m.ExperimentCreatePage })),
);
const ExperimentDetailPage = lazy(
  () => import('@saker/grayling-pages/pages/ExperimentDetailPage').then(m => ({ default: m.ExperimentDetailPage })),
);

const view = (page: ReactElement): ReactElement => <Suspense fallback={<Skeleton lines={4} />}>{page}</Suspense>;

export const router = createHashRouter([
  { path: '/', element: <RootRedirect /> },
  {
    path: '/console/:ws/:env',
    element: <ShellWithGuard />,
    children: [
      { path: 'overview', element: view(<OverviewPage />) },
      { path: 'flags', element: view(<FlagsPage />) },
      { path: 'flags/:flagKey', element: view(<FlagDetailPage />) },
      { path: 'segments', element: view(<SegmentsPage />) },
      { path: 'segments/:segKey', element: view(<SegmentDetailPage />) },
      { path: 'experiments', element: view(<ExperimentsPage />) },
      { path: 'experiments/~new', element: view(<ExperimentCreatePage />) },
      { path: 'experiments/:expId', element: view(<ExperimentDetailPage />) },
      { path: 'experiments/:expId/:tab', element: view(<ExperimentDetailPage />) },
      { path: 'metrics', element: view(<MetricsPage />) },
      { path: 'metrics/:key/:version', element: view(<MetricsPage />) },
      { path: 'approvals', element: view(<ApprovalsPage />) },
      { path: 'approvals/:approvalID', element: view(<ApprovalDetailPage />) },
      { path: 'ai', element: view(<AiPage />) },
      { path: 'ai/:type/:logicalId', element: view(<AiPage />) },
      { path: 'audit', element: view(<AuditPage />) },
      { path: 'audit/:object', element: view(<AuditPage />) },
      { path: 'debug', element: view(<DebugPage />) },
      { path: 'tokens', element: view(<TokensPage />) },
      { path: 'quotas', element: view(<QuotasPage />) },
      { path: '*', element: <CatchAll /> },
    ],
  },
]);
