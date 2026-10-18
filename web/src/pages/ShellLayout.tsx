import { useEffect, useMemo, useState } from 'react';
import type { ReactElement, ReactNode } from 'react';
import { Outlet, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { ApiError, baseUrl } from '@saker/grayling-client';
import { HostProvider } from '@saker/grayling-pages/host';
import type { HostServices } from '@saker/grayling-pages/host';
import { setLang } from '../i18n';
import { useSession } from '../session/SessionProvider';
import { ScopeProvider, useScope } from '../scope/ScopeProvider';
import { CapabilityProvider, useCan } from '../capability/CapabilityProvider';
import { useCapabilities } from '../capability/CapabilityProvider';
import { Button } from '../components/Button';
import { FormDialog } from '../components/FormDialog';
import { useToast } from '../components/Toast';
import { useTheme } from '../hooks/useTheme';
import { consolePath, useConsoleParams } from '../routes';
import type { ViewName } from '../routes';

// ShellLayout is the legacy #app chrome: sidebar nav (groups and labels match
// legacy index.html), topbar scope selectors, theme tri-state, zh/EN switch,
// mobile hamburger and the skip-link. Views render into <Outlet/>.
export default function ShellLayout({ guard }: { guard?: ReactNode }): ReactElement {
  return (
    <ScopeProvider>
      <CapabilityProvider>
        <ShellBody guard={guard} />
      </CapabilityProvider>
    </ScopeProvider>
  );
}

interface NavItem {
  view: ViewName;
  labelKey: string;
  permission?: string;
  id?: string;
}

const NAV_GROUPS: Array<{ labelKey: string | null; items: NavItem[] }> = [
  { labelKey: null, items: [{ view: 'overview', labelKey: 'nav.overview' }] },
  {
    labelKey: 'nav.groupRelease',
    items: [
      { view: 'flags', labelKey: 'nav.flags' },
      { view: 'segments', labelKey: 'nav.segments' },
    ],
  },
  {
    labelKey: 'nav.groupExperiment',
    items: [
      { view: 'metrics', labelKey: 'nav.metrics' },
      { view: 'experiments', labelKey: 'nav.experiments' },
    ],
  },
  {
    labelKey: 'nav.groupAI',
    items: [{ view: 'ai', labelKey: 'nav.ai', id: 'nav-ai' }],
  },
  {
    labelKey: 'nav.groupOps',
    items: [
      { view: 'approvals', labelKey: 'nav.approvals' },
      { view: 'debug', labelKey: 'nav.debug' },
      { view: 'tokens', labelKey: 'nav.tokens', permission: 'token.manage' },
      { view: 'quotas', labelKey: 'nav.quotas', permission: 'quota.manage', id: 'nav-quotas' },
      { view: 'audit', labelKey: 'nav.audit' },
    ],
  },
];

function ShellBody({ guard }: { guard?: ReactNode }): ReactElement {
  const { t, i18n } = useTranslation();
  const { session, logout, client } = useSession();
  const { ws, env, workspaces, envs, switchScope, reloadWorkspaces, reloadEnvs } = useScope();
  const can = useCan();
  const { refresh: refreshCapabilities } = useCapabilities();
  const { toast } = useToast();
  const navigate = useNavigate();
  const consoleParams = useConsoleParams();
  const { preference, setPreference } = useTheme();
  const [menuOpen, setMenuOpen] = useState(false);
  const [newWsOpen, setNewWsOpen] = useState(false);
  const [newEnvOpen, setNewEnvOpen] = useState(false);
  const [aiAvailable, setAiAvailable] = useState(true);

  // The host bridge hands every page (living in @saker/grayling-pages) the
  // shell services it may need; pages never import the shell back.
  const host = useMemo<HostServices>(
    () => ({
      client,
      ws,
      env,
      envs,
      actor: session?.name || '',
      can,
      refreshCapabilities,
      reauth: () => {
        void logout();
      },
    }),
    [client, ws, env, envs, session, can, refreshCapabilities, logout],
  );

  // probeAI: the AI view hides itself when the server answers 501.
  useEffect(() => {
    if (!ws || !env) return;
    const controller = new AbortController();
    client
      .api(
        '/api/v1/admin/workspaces/' +
          encodeURIComponent(ws) +
          '/environments/' +
          encodeURIComponent(env) +
          '/ai/resources',
        { signal: controller.signal },
      )
      .then(() => {
        if (!controller.signal.aborted) setAiAvailable(true);
      })
      .catch((e: unknown) => {
        if (!controller.signal.aborted && (e as ApiError).status === 501) setAiAvailable(false);
      });
    return () => controller.abort();
  }, [client, ws, env]);

  const gotoView = (view: ViewName) => {
    setMenuOpen(false);
    navigate(consolePath({ ...emptySub, ws, env, view }));
  };

  const slugField = (placeholder: string) => ({
    name: 'slug',
    label: t('label.slug'),
    placeholder,
    required: true,
    validate: (value: string) => (/^[a-z][a-z0-9-]{0,63}$/.test(value) ? '' : t('validation.slugInvalid')),
  });

  const nameField = (placeholder: string) => ({
    name: 'name',
    label: t('label.displayName'),
    placeholder,
  });

  const conn = (baseUrl() || location.origin) + ' · ' + t(client.cookieSession ? 'auth.session' : 'auth.temporary');

  return (
    <div className="app">
      <a className="skip-link" href="#main">
        Skip to main content
      </a>
      <aside className="sidebar">
        <div className="brand-row">
          <div className="brand">
            Grayling <span>{t('brand.sub')}</span>
          </div>
          <Button
            size="sm"
            className="menu-toggle"
            id="btn-menu"
            aria-controls="nav"
            aria-expanded={menuOpen}
            onClick={() => setMenuOpen(open => !open)}
          >
            {t('nav.menu')}
          </Button>
        </div>
        <nav id="nav" aria-label={t('nav.primary')} className={menuOpen ? 'mobile-open' : ''}>
          {NAV_GROUPS.map((group, gi) => (
            <div className="nav-group" key={group.labelKey ?? gi}>
              {group.labelKey ? <span className="nav-group-label">{t(group.labelKey)}</span> : null}
              {group.items
                .filter(item => !item.permission || can(item.permission))
                .filter(item => item.view !== 'ai' || aiAvailable)
                .map(item => (
                  <button
                    key={item.view}
                    type="button"
                    id={item.id}
                    className={consoleParams?.view === item.view ? 'active' : ''}
                    onClick={() => gotoView(item.view)}
                  >
                    {t(item.labelKey)}
                  </button>
                ))}
            </div>
          ))}
        </nav>
        <div className="side-foot">
          <Button size="sm" grow id="btn-reauth" onClick={() => void logout()}>
            {t('nav.reauth')}
          </Button>
        </div>
      </aside>
      <div className="maincol">
        <header className="topbar">
          <label className="inline">
            <span>{t('top.workspace')}</span>{' '}
            <select
              id="sel-ws"
              value={ws}
              onChange={ev => switchScope(ev.currentTarget.value, '')}
            >
              {workspaces.length === 0 ? <option value="">{t('select.noneCreate')}</option> : null}
              {workspaces.map(w => (
                <option key={w.slug} value={w.slug}>
                  {(w.name || w.slug) + ' (' + w.slug + ')'}
                </option>
              ))}
            </select>
          </label>
          {can('workspace.admin') ? (
            <Button size="sm" id="btn-new-ws" title={t('top.newWs')} onClick={() => setNewWsOpen(true)}>
              +
            </Button>
          ) : null}
          <label className="inline">
            <span>{t('top.environment')}</span>{' '}
            <select
              id="sel-env"
              value={env}
              onChange={ev => switchScope(ws, ev.currentTarget.value)}
            >
              {envs.length === 0 ? <option value="">{t('select.noneCreate')}</option> : null}
              {envs.map(v => (
                <option key={v.slug} value={v.slug}>
                  {(v.name || v.slug) + ' (' + v.slug + ')'}
                </option>
              ))}
            </select>
          </label>
          {ws && can('workspace.admin') ? (
            <Button size="sm" id="btn-new-env" title={t('top.newEnv')} onClick={() => setNewEnvOpen(true)}>
              +
            </Button>
          ) : null}
          <span id="conn-info" className="conn muted">
            {conn}
            {session?.name ? ' · ' + session.name : ''}
          </span>
          <label className="inline">
            <span>{t('top.theme')}</span>{' '}
            <select
              id="sel-theme"
              aria-label={t('top.theme')}
              value={preference}
              onChange={ev => setPreference(ev.currentTarget.value as 'system' | 'light' | 'dark')}
            >
              <option value="system">{t('theme.system')}</option>
              <option value="light">{t('theme.light')}</option>
              <option value="dark">{t('theme.dark')}</option>
            </select>
          </label>
          <span className="lang-switch" id="lang-switch">
            <button
              className={'btn sm' + (i18n.language === 'zh' ? ' active' : '')}
              id="lang-zh"
              type="button"
              aria-pressed={i18n.language === 'zh'}
              onClick={() => setLang('zh')}
            >
              {t('lang.zh')}
            </button>
            <button
              className={'btn sm' + (i18n.language === 'en' ? ' active' : '')}
              id="lang-en"
              type="button"
              aria-pressed={i18n.language === 'en'}
              onClick={() => setLang('en')}
            >
              {t('lang.en')}
            </button>
          </span>
        </header>
        <main id="main">
          <HostProvider value={host}>
            {guard}
            <Outlet />
          </HostProvider>
        </main>
      </div>
      {newWsOpen ? (
        <FormDialog
          title={t('ws.newTitle')}
          fields={[slugField('acme'), nameField('Acme Inc')]}
          submitLabel={t('btn.create')}
          onClose={() => setNewWsOpen(false)}
          onSubmit={async v => {
            const slug = v.slug.trim();
            await client.api('/api/v1/admin/workspaces', {
              method: 'POST',
              body: { slug, name: v.name.trim() || slug },
            });
            toast(t('toast.wsCreated'));
            await reloadWorkspaces();
            switchScope(slug, '');
          }}
        />
      ) : null}
      {newEnvOpen ? (
        <FormDialog
          title={t('env.newTitle', { ws })}
          fields={[slugField('production'), nameField('Production')]}
          submitLabel={t('btn.create')}
          onClose={() => setNewEnvOpen(false)}
          onSubmit={async v => {
            const slug = v.slug.trim();
            await client.api(
              '/api/v1/admin/workspaces/' + encodeURIComponent(ws) + '/environments',
              { method: 'POST', body: { slug, name: v.name.trim() || slug } },
            );
            toast(t('toast.envCreated'));
            await reloadEnvs();
            switchScope(ws, slug);
          }}
        />
      ) : null}
    </div>
  );
}

const emptySub = {
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
