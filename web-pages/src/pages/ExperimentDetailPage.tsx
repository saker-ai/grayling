import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate, useParams } from 'react-router-dom';
import { ApiError, errMsg } from '@saker/grayling-client';
import type {
  AuditEntry,
  BindingReadiness,
  ExperimentDetail,
  ExperimentDiagnostics,
  ExperimentReport,
} from '@saker/grayling-client';
import { Button } from '../components/Button';
import { Chip } from '../components/Chip';
import { useConfirm } from '../components/ConfirmDialog';
import { ErrorBanner } from '../components/ErrorBanner';
import { useToast } from '../components/Toast';
import { useApiData } from '../hooks/useApiData';
import { usePolling } from '../hooks/usePolling';
import { Can, useHost } from '../host';
import { consoleViewPath, fmtDurNs, wsPath, EXP_CHIP } from '../utils';
import { BindConflict, BindReadinessView } from '../workbench/BindReadiness';
import { classifyExpStatus, EXP_PRELAUNCH_STATES } from '../workbench/classify';
import type { LatestTransition } from '../workbench/classify';
import { DecisionHero } from '../workbench/DecisionHero';
import { HealthZone } from '../workbench/HealthZone';
import { ReportSummary, ReportView } from '../workbench/ReportView';
import { RolloutBlock } from '../workbench/RolloutBlock';
import type { RolloutReportStatus } from '../workbench/RolloutBlock';
import { StatusBand } from '../workbench/StatusBand';
import { ZoneNav } from '../workbench/ZoneNav';
import type { ZoneTab } from '../workbench/ZoneNav';

// ExperimentDetailPage ports legacy viewExpDetail (web/legacy/exp_workbench.js
// 26-483): the four-zone experiment workbench. Persistent chrome (status
// band, auto-refresh switch, zone navigation) stays mounted across tabs; the
// zone body swaps per route. Report state follows the legacy rules — a stale
// report is never presented as the current conclusion (snapshot clears on
// error), the rollout gate degrades to 'unavailable', and the evidence zone
// shows the stale copy behind an explicit warning. The hero lazily pulls
// diagnostics (no_samples sub-branch) and the latest experiment.transition
// audit entry (ended copy), each guarded so a slow response can never
// overwrite a newer experiment (the keyed remount + request counters of
// legacy, here via an epoch ref).
const EXP_TAB_ALIASES: Record<string, ZoneTab> = { diagnostics: 'health' };

export function ExperimentDetailPage(): ReactElement {
  const params = useParams<{ expId?: string; tab?: string }>();
  const expId = params.expId || '';
  const rawTab = params.tab || 'summary';
  const tab: ZoneTab = EXP_TAB_ALIASES[rawTab] || (rawTab as ZoneTab);
  // The keyed remount is the legacy route() on experiment switch: all report,
  // diagnostics and rollout state resets instead of risking cross-experiment
  // bleed.
  return <ExperimentDetailBody key={expId} expId={expId} tab={tab} />;
}

function ExperimentDetailBody({ expId, tab }: { expId: string; tab: ZoneTab }): ReactElement {
  const { t, i18n } = useTranslation();
  const { client, ws, env, can } = useHost();
  const navigate = useNavigate();
  const { toast } = useToast();
  const locale = i18n.language === 'zh' ? 'zh-CN' : 'en-US';
  const base = '/api/v1/admin/experiments/' + encodeURIComponent(expId);
  const { data: experiment, loading, error, reload } = useApiData<ExperimentDetail>(
    signal => client.api<ExperimentDetail>(base, { signal }),
    [client, base],
  );

  // Report state (legacy lastReport/lastReportError/reportDetailOpen).
  const [lastReport, setLastReport] = useState<ExperimentReport | null>(null);
  const [lastReportError, setLastReportError] = useState<unknown>(null);
  const [reportDetailOpen, setReportDetailOpen] = useState(false);
  const [rolloutStatus, setRolloutStatus] = useState<RolloutReportStatus | null>(null);
  // Hero extras: undefined = not fetched, null = fetch failed / not found.
  const [heroDiagnostics, setHeroDiagnostics] = useState<ExperimentDiagnostics | null | undefined>(undefined);
  const [latestTransition, setLatestTransition] = useState<LatestTransition | null | undefined>(undefined);
  const [autoRefresh, setAutoRefresh] = useState(false);
  const requestEpoch = useRef(0);

  const canRead = can('report.read');
  const canPublish = can('flag.publish');
  const preLaunch = EXP_PRELAUNCH_STATES.includes(experiment?.state || '');
  const reportStatus: RolloutReportStatus =
    rolloutStatus ?? (preLaunch ? 'not_required' : canRead ? 'pending' : 'restricted');

  // loadReport keeps the legacy return-a-boolean contract (the polling
  // backoff counts failures on it) and the legacy update semantics.
  const loadReport = useCallback(
    async (showDetail = false): Promise<boolean> => {
      if (showDetail) setReportDetailOpen(true);
      const epoch = ++requestEpoch.current;
      try {
        const report = await client.api<ExperimentReport>(base + '/report');
        if (epoch !== requestEpoch.current) return false;
        setLastReport(report);
        setLastReportError(null);
        setRolloutStatus('ready');
        return true;
      } catch (err) {
        if (epoch !== requestEpoch.current) return false;
        setLastReportError(err);
        // The stale report stays for the evidence zone's explicit stale
        // warning; the snapshot hides because lastReportError is set.
        setRolloutStatus('unavailable');
        return false;
      }
    },
    [client, base],
  );

  const fetchHeroDiagnostics = useCallback(async () => {
    const epoch = ++requestEpoch.current;
    try {
      const d = await client.api<ExperimentDiagnostics>(base + '/diagnostics');
      if (epoch !== requestEpoch.current) return;
      setHeroDiagnostics(d);
    } catch {
      if (epoch !== requestEpoch.current) return;
      setHeroDiagnostics(null); // generic waiting copy + retry, no guessed sub-branch
    }
  }, [client, base]);

  const fetchLatestTransition = useCallback(async () => {
    const epoch = ++requestEpoch.current;
    try {
      const page =
        (await client.api<AuditEntry[]>(wsPath(ws) + '/audit?object=' + encodeURIComponent(expId) + '&limit=20')) || [];
      if (epoch !== requestEpoch.current) return;
      const entry = page.find(a => a && a.action === 'experiment.transition');
      setLatestTransition(entry ? parseTransition(entry) : null);
    } catch {
      if (epoch !== requestEpoch.current) return;
      setLatestTransition(null); // degraded endedUnknown copy, never a guess
    }
  }, [client, ws, expId]);

  const cls = useMemo(
    () =>
      experiment
        ? classifyExpStatus(
            experiment,
            lastReport,
            heroDiagnostics,
            latestTransition,
            { error: lastReportError, canRead, diagFailed: heroDiagnostics === null },
            canPublish,
            t,
          )
        : null,
    [experiment, lastReport, heroDiagnostics, latestTransition, lastReportError, canRead, canPublish, t],
  );

  // ensureHeroData: the lazily-fetched hero sources, once per need.
  useEffect(() => {
    if (!cls) return;
    if (
      cls.kind === 'no_samples' &&
      cls.params.subBranch === 'unknown' &&
      heroDiagnostics === undefined &&
      !(lastReport && lastReport.diagnostics)
    ) {
      void fetchHeroDiagnostics();
    }
    if (cls.kind === 'ended' && latestTransition === undefined) void fetchLatestTransition();
  }, [cls, heroDiagnostics, latestTransition, lastReport, fetchHeroDiagnostics, fetchLatestTransition]);

  // Mount report load (legacy viewExpDetail tail): the evidence tab awaits a
  // detailed load, every other tab a silent one.
  useEffect(() => {
    if (!experiment) return;
    if (canRead && !EXP_PRELAUNCH_STATES.includes(experiment.state || '')) {
      void loadReport(tab === 'evidence');
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [experiment?.id]);

  // #exp-auto: the 10s→60s exponential-backoff polling of legacy poll().
  const pollTask = useCallback(() => loadReport(), [loadReport]);
  usePolling({ enabled: autoRefresh, task: pollTask });

  const gotoZone = useCallback(
    (next: ZoneTab) => {
      navigate(
        next === 'summary'
          ? consoleViewPath(ws, env, 'experiments', expId)
          : consoleViewPath(ws, env, 'experiments', expId, next),
      );
    },
    [navigate, ws, env, expId],
  );
  const openAudit = useCallback(() => navigate(consoleViewPath(ws, env, 'audit', expId)), [navigate, ws, env, expId]);
  const heroGoto = useCallback(
    (target: string) => {
      if (target === 'audit') openAudit();
      else if (target === 'retry-diagnostics') setHeroDiagnostics(undefined);
      else gotoZone(target as ZoneTab);
    },
    [openAudit, gotoZone],
  );

  const download = useCallback(
    async (format: string) => {
      try {
        await client.downloadReport(base + '/report/export', format);
      } catch (err) {
        toast(errMsg(err as Parameters<typeof errMsg>[0], t), false);
      }
    },
    [client, base, toast, t],
  );

  if (loading && !experiment) return <p className="muted">{t('misc.loading')}</p>;
  if (error) return <ErrorBanner error={error} />;
  if (!experiment) return <p className="muted">{t('misc.loading')}</p>;
  const e = experiment;

  return (
    <>
      <div className="view-head">
        <Button size="sm" id="exp-back" onClick={() => navigate(consoleViewPath(ws, env, 'experiments'))}>
          {t('exp.back')}
        </Button>
        <h2 className="mono">{e.id}</h2>
        <Chip label={e.state} cls={EXP_CHIP[e.state || ''] || 'gray'} />
        <span className="muted">
          {t('exp.meta', { flag: e.flagKey, rev: e.flagRevision || 0, epoch: e.epoch || 0, by: e.createdBy || '—' })}
        </span>
      </div>
      <StatusBand experiment={e} report={lastReport} locale={locale} />
      <div className="row" id="exp-report-controls">
        <label className="inline">
          <input
            type="checkbox"
            id="exp-auto"
            checked={autoRefresh}
            onChange={ev => setAutoRefresh(ev.currentTarget.checked)}
          />{' '}
          {t('exp.autoRefresh')}
        </label>
      </div>
      <ZoneNav active={tab} onSelect={gotoZone} />
      {/* #exp-zone stays the layout hook (global.css min-width); the tabpanel
          semantics live on an inner wrapper because the APG tab pairing needs
          a per-tab id (exp-zonepanel-{tab}) that aria-controls can target. */}
      <div id="exp-zone">
        <div role="tabpanel" id={'exp-zonepanel-' + tab} aria-labelledby={'exp-tab-' + tab}>
        {tab === 'health' ? (
          <HealthZoneTab expId={expId} experiment={e} report={lastReport} locale={locale} />
        ) : tab === 'evidence' ? (
          <section className="card">
            <h3>{t('exp.report')}</h3>
            <div className="row">
              <Button id="exp-report" onClick={() => void loadReport(true)}>
                {t('btn.loadReport')}
              </Button>
              {canRead ? (
                <>
                  <Button id="exp-export-csv" onClick={() => void download('csv')}>
                    {t('report.exportCsv')}
                  </Button>
                  <Button id="exp-export-json" onClick={() => void download('json')}>
                    {t('report.exportJson')}
                  </Button>
                </>
              ) : null}
            </div>
            <div id="report-box">
              {!reportDetailOpen ? (
                <p className="muted">{t('exp.notLoaded')}</p>
              ) : lastReportError ? (
                <>
                  {lastReport ? (
                    <div className="banner warn" role="status">
                      {t('report.stale')}
                    </div>
                  ) : null}
                  <ErrorBanner error={lastReportError} />
                  {lastReport ? <ReportView report={lastReport} locale={locale} /> : null}
                </>
              ) : lastReport ? (
                <ReportView report={lastReport} locale={locale} />
              ) : (
                <p className="muted">{t('exp.notLoaded')}</p>
              )}
            </div>
          </section>
        ) : tab === 'rollout' ? (
          <section className="card">
            <h3>
              {t('ro.title')} <span className="muted">{t('ro.subtitle')}</span>
            </h3>
            <div id="rollout-box">
              <RolloutBlock
                experiment={e}
                env={env}
                reportStatus={reportStatus}
                canPublish={canPublish}
                locale={locale}
                onReportStatus={setRolloutStatus}
                onChanged={reload}
                onOpenAudit={openAudit}
              />
            </div>
          </section>
        ) : tab === 'settings' ? (
          <SettingsZone experiment={e} base={base} onChanged={reload} onOpenAudit={openAudit} />
        ) : (
          <>
            {cls ? <DecisionHero cls={cls} report={lastReport} locale={locale} onGoto={heroGoto} /> : null}
            <div id="exp-summary-snapshot">
              {lastReport && !lastReportError ? <ReportSummary report={lastReport} locale={locale} /> : null}
            </div>
            <div className="row actions" id="exp-zone-actions">
              <Button id="exp-goto-health" onClick={() => gotoZone('health')}>
                {t('exp.zone.health')}
              </Button>
              <Button id="exp-goto-evidence" onClick={() => gotoZone('evidence')}>
                {t('exp.zone.evidence')}
              </Button>
              <Button id="exp-goto-rollout" onClick={() => gotoZone('rollout')}>
                {t('exp.zone.rollout')}
              </Button>
              <Button id="exp-goto-settings" onClick={() => gotoZone('settings')}>
                {t('exp.tab.settings')}
              </Button>
            </div>
            <p>
              <button className="row-link" id="exp-view-prereg" type="button" onClick={() => gotoZone('settings')}>
                {t('exp.viewPrereg')}
              </button>
            </p>
          </>
        )}
        </div>
      </div>
    </>
  );
}

// parseTransition reads the real experiment.transition audit entry: actor and
// time come from the audit record, to/reason from its JSON detail.
function parseTransition(entry: AuditEntry): LatestTransition {
  let detail: { to?: string; reason?: string } = {};
  try {
    detail =
      typeof entry.detail === 'string' && entry.detail
        ? (JSON.parse(entry.detail) as { to?: string; reason?: string })
        : ((entry.detail || {}) as { to?: string; reason?: string });
  } catch {
    detail = {};
  }
  return { actor: entry.actor || '', time: entry.time || '', to: detail.to || '', reason: detail.reason || '' };
}

// HealthZoneTab owns the per-zone diagnostics fetch (legacy loadZoneHealth):
// the payload loads on entering the zone, errors render the errBanner + a
// retry button, and #diag-refresh re-fetches in place.
function HealthZoneTab({
  expId,
  experiment,
  report,
  locale,
}: {
  expId: string;
  experiment: ExperimentDetail;
  report: ExperimentReport | null;
  locale: string;
}): ReactElement {
  const { t } = useTranslation();
  const { client } = useHost();
  const base = '/api/v1/admin/experiments/' + encodeURIComponent(expId);
  const { data, loading, error, reload } = useApiData<ExperimentDiagnostics>(
    signal => client.api<ExperimentDiagnostics>(base + '/diagnostics', { signal }),
    [client, base],
  );
  if (loading && !data) return <p className="muted">{t('misc.loading')}</p>;
  if (error || !data) {
    return (
      <>
        <ErrorBanner error={error} />
        <div className="row">
          <Button id="diag-refresh" onClick={reload}>
            {t('btn.refresh')}
          </Button>
        </div>
      </>
    );
  }
  return <HealthZone experiment={experiment} diagnostics={data} report={report} locale={locale} onRefresh={reload} />;
}

// SettingsZone ports loadZoneSettings (legacy): the lifecycle card (bind
// readiness + review/open-flag/recheck/bind + the transition row) and the
// pre-registration card, plus the open-audit card.
function SettingsZone({
  experiment,
  base,
  onChanged,
  onOpenAudit,
}: {
  experiment: ExperimentDetail;
  base: string;
  onChanged: () => void;
  onOpenAudit: () => void;
}): ReactElement {
  const { t } = useTranslation();
  const { client, ws, env, can, refreshCapabilities } = useHost();
  const navigate = useNavigate();
  const { toast } = useToast();
  const confirm = useConfirm();
  const e = experiment;
  const canFlagRead = can('flag.read');

  const [bindReadiness, setBindReadiness] = useState<BindingReadiness | null>(null);
  const [bindCheckError, setBindCheckError] = useState<unknown>(null);
  const [bindChecking, setBindChecking] = useState(false);
  const [bindConflict, setBindConflict] = useState<{
    previous: BindingReadiness | null;
    latest: BindingReadiness | null;
  } | null>(null);
  const [transitionTo, setTransitionTo] = useState('running');
  const [transitionReason, setTransitionReason] = useState('');

  const loadBindCheck = useCallback(async (): Promise<BindingReadiness | null> => {
    setBindChecking(true);
    setBindConflict(null);
    try {
      const readiness = await client.api<BindingReadiness>(base + '/bind-check');
      setBindReadiness(readiness);
      setBindCheckError(null);
      return readiness;
    } catch (err) {
      setBindReadiness(null);
      setBindCheckError(err);
      return null;
    } finally {
      setBindChecking(false);
    }
  }, [client, base]);

  useEffect(() => {
    if (canFlagRead) void loadBindCheck();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [canFlagRead, e.id]);

  // Review: legacy prompt() + unconditional POST; the dialog's cancel now
  // genuinely aborts (legacy cancel still posted an empty note).
  const onReview = async () => {
    const accepted = await confirm({
      title: t('btn.review'),
      reasonLabel: t('prompt.reviewNote'),
      reasonRequired: false,
      onConfirm: async ({ reason }) => {
        try {
          await client.api(base + '/review', { method: 'POST', body: { note: reason } });
        } catch (err) {
          if ((err as ApiError).status === 403) await refreshCapabilities();
          throw err;
        }
      },
    });
    if (accepted) {
      toast(t('toast.reviewRecorded'));
      onChanged();
    }
  };

  // Bind: re-check readiness first, POST /bind with the legacy empty body,
  // and on 409 re-check + render the conflict comparison.
  const onBind = async () => {
    const checked = await loadBindCheck();
    if (!checked?.ready) {
      toast(t('exp.bindNotReady'), false);
      return;
    }
    try {
      await client.api(base + '/bind', { method: 'POST', body: {} });
      toast(t('toast.expBound'));
      onChanged();
    } catch (err) {
      toast(errMsg(err as Parameters<typeof errMsg>[0], t), false);
      const latest = await loadBindCheck();
      if ((err as ApiError).status === 409) setBindConflict({ previous: checked, latest });
    }
  };

  const onTransition = async () => {
    const reason = transitionReason.trim();
    if (!reason) {
      toast(t('toast.transitionReason'), false);
      return;
    }
    const accepted = await confirm({
      title: t('btn.transition'),
      summary: t('confirm.transition', { to: transitionTo }),
      onConfirm: async () => {
        try {
          await client.api(base + '/transition', { method: 'POST', body: { to: transitionTo, reason } });
        } catch (err) {
          if ((err as ApiError).status === 403) await refreshCapabilities();
          throw err;
        }
      },
    });
    if (accepted) {
      toast(t('toast.transitioned', { to: transitionTo }));
      onChanged();
    }
  };

  return (
    <>
      <div className="cols exp-detail-cols">
        <section className="card">
          <h3>{t('exp.lifecycle')}</h3>
          <div className="bind-readiness" id="exp-bind-check" role="status">
            {bindConflict ? <BindConflict previous={bindConflict.previous} latest={bindConflict.latest} /> : null}
            {!canFlagRead ? (
              t('exp.bindCheckPermission')
            ) : bindChecking && !bindReadiness ? (
              t('exp.bindCheckLoading')
            ) : bindCheckError ? (
              <ErrorBanner error={bindCheckError} />
            ) : bindReadiness ? (
              <BindReadinessView readiness={bindReadiness} />
            ) : (
              t('exp.bindCheckLoading')
            )}
          </div>
          <div className="row actions" style={{ border: 0, padding: 0, margin: 0 }}>
            <Can permission="experiment.write">
              <Button id="exp-review" onClick={() => void onReview()}>
                {t('btn.review')}
              </Button>
            </Can>
            <Button id="exp-open-flag" onClick={() => navigate(consoleViewPath(ws, env, 'flags', e.flagKey))}>
              {t('exp.openFlag')}
            </Button>
            <Button id="exp-recheck" disabled={!canFlagRead} onClick={() => void loadBindCheck()}>
              {t('exp.bindRecheck')}
            </Button>
            <Can permission="flag.write">
              <Button id="exp-bind" disabled={!bindReadiness?.ready} onClick={() => void onBind()}>
                {t('btn.bind')}
              </Button>
            </Can>
          </div>
          <p className="muted">{t('exp.bindHint')}</p>
          <div className="row">
            <select
              id="exp-to"
              aria-label={t('exp.transitionTo')}
              value={transitionTo}
              onChange={ev => setTransitionTo(ev.currentTarget.value)}
            >
              <option value="running">running</option>
              <option value="paused">paused</option>
              <option value="stopped">stopped</option>
              <option value="analyzed">analyzed</option>
              <option value="archived">archived</option>
            </select>
            <input
              id="exp-reason"
              className="grow"
              placeholder={t('ph.reasonRequired')}
              value={transitionReason}
              onChange={ev => setTransitionReason(ev.currentTarget.value)}
            />
            <Can permission="flag.publish">
              <Button variant="primary" id="exp-transition" onClick={() => void onTransition()}>
                {t('btn.transition')}
              </Button>
            </Can>
          </div>
        </section>
        <PreRegistration experiment={e} />
      </div>
      <section className="card">
        <div className="row">
          <Button id="exp-open-audit" onClick={onOpenAudit}>
            {t('exp.openAuditObject')}
          </Button>
        </div>
      </section>
    </>
  );
}

// PreRegistration ports the prereg card of loadZoneSettings: every field the
// operator registered at creation time, verbatim, with the guardrail table
// and the Holm family when present.
function PreRegistration({ experiment }: { experiment: ExperimentDetail }): ReactElement {
  const { t } = useTranslation();
  const e = experiment;
  return (
    <section className="card">
      <h3>{t('exp.prereg')}</h3>
      <table className="kv">
        <tbody>
          <tr>
            <th scope="row">{t('th.unit')}</th>
            <td className="mono">{e.unit}</td>
          </tr>
          <tr>
            <th scope="row">{t('exp.layerShort')}</th>
            <td>
              {e.layer || '—'}
              {e.layerSlot ? (
                <span className="muted">{' ' + t('exp.slot', { json: JSON.stringify(e.layerSlot) })}</span>
              ) : null}
            </td>
          </tr>
          <tr>
            <th scope="row">{t('exp.eligShort')}</th>
            <td>{e.eligibility ? e.eligibility + '%' : '100%'}</td>
          </tr>
          <tr>
            <th scope="row">{t('th.variants')}</th>
            <td>
              {(e.variants || []).map((v, i) => (
                <span key={v}>
                  {i ? ', ' : ''}
                  <span className="mono">{v}</span>
                  {' (' + String((e.proportions || {})[v] ?? '?') + ')'}
                </span>
              ))}
            </td>
          </tr>
          <tr>
            <th scope="row">{t('exp.primaryShort')}</th>
            <td className="mono">{e.primaryMetric?.key}</td>
          </tr>
          <tr>
            <th scope="row"></th>
            <td>{t('exp.betterEq', { type: e.primaryMetric?.type, dir: e.primaryMetric?.direction })}</td>
          </tr>
          <tr>
            <th scope="row">α / power / MDE</th>
            <td>{String(e.alpha) + ' / ' + String(e.power) + ' / ' + String(e.mde)}</td>
          </tr>
          <tr>
            <th scope="row">{t('exp.minSampleArm')}</th>
            <td>{e.minSamplePerArm}</td>
          </tr>
          <tr>
            <th scope="row">{t('exp.minRuntime')}</th>
            <td>{fmtDurNs(e.minRuntime, t)}</td>
          </tr>
          <tr>
            <th scope="row">{t('exp.window')}</th>
            <td>{fmtDurNs(e.analysisWindow, t)}</td>
          </tr>
          <tr>
            <th scope="row">{t('exp.exposureMode')}</th>
            <td>{t(e.exposureMode === 'onRender' ? 'exp.onRender' : 'exp.onDecision')}</td>
          </tr>
          {e.exposureMode === 'onRender' ? (
            <>
              <tr>
                <th scope="row">{t('exp.exposureTrigger')}</th>
                <td>{e.exposureTrigger || '—'}</td>
              </tr>
              <tr>
                <th scope="row">{t('exp.integrationOwner')}</th>
                <td>{e.integrationOwner || '—'}</td>
              </tr>
            </>
          ) : null}
          <tr>
            <th scope="row">{t('exp.method')}</th>
            <td>{e.analysisMethod || '—'}</td>
          </tr>
          <tr>
            <th scope="row">{t('exp.allocationMode')}</th>
            <td>
              {(e.allocationMode || 'fixed-split') + (e.bandit ? ' · ' + e.bandit.policyVersion + ' · ' + e.bandit.strategy : '')}
            </td>
          </tr>
        </tbody>
      </table>
      <div className="muted">{t('exp.hypothesisLine', { h: e.hypothesis || '—' })}</div>
      {(e.guardrails || []).length ? (
        <>
          <h4>{t('exp.guardrails')}</h4>
          <div className="table-scroll">
            <table className="grid">
            <thead>
              <tr>
                <th>{t('th.metric')}</th>
                <th>{t('th.type')}</th>
                <th>{t('th.goodDir')}</th>
                <th>{t('th.maxReg')}</th>
                <th>{t('th.minSample')}</th>
              </tr>
            </thead>
            <tbody>
              {(e.guardrails || []).map((g, i) => (
                <tr key={i}>
                  <td className="mono">{g.metricKey}</td>
                  <td>{g.type}</td>
                  <td>{g.direction || 'increase'}</td>
                  <td>{g.maxRelativeRegression}</td>
                  <td>{g.minSample}</td>
                </tr>
              ))}
            </tbody>
            </table>
          </div>
        </>
      ) : null}
      {e.holmFamily ? (
        <>
          <h4>{t('exp.holmFamily')}</h4>
          <p className="muted">{t('exp.holmHelp') + ' · ' + (e.registrationVersion || '—')}</p>
          <ul className="plain">
            {(e.holmFamily.hypotheses || []).map(h => (
              <li key={h.id}>
                {h.id + ': ' + h.metric.key + ' (' + h.control + ' → ' + h.treatment + ')'}
              </li>
            ))}
          </ul>
        </>
      ) : null}
    </section>
  );
}
