import { useState } from 'react';
import type { ReactElement } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { ApiError, errMsg } from '@saker/grayling-client';
import type { ExperimentDetail, ExperimentReport, PlanEvent, ReleasePlan, ReleaseStep } from '@saker/grayling-client';
import { Banner } from '../components/Banner';
import { Button } from '../components/Button';
import { Chip } from '../components/Chip';
import { useConfirm } from '../components/ConfirmDialog';
import { DataTable } from '../components/DataTable';
import type { Column } from '../components/DataTable';
import { useToast } from '../components/Toast';
import { useHost } from '../host';
import { fmtDurNs, fmtTime, scopePath } from '../utils';
import { NS_HOUR, PLAN_ACTION_CHIP, PLAN_BLOCK_RE, PLAN_CHIP, planNextGate, validateReleaseSteps } from './classify';
import { ReportSummary } from './ReportView';

// RolloutBlock ports renderRollout + bindRollout (exp_workbench.js 962-1289):
// the staged-rollout zone. All plan states render (no-plan create editor,
// armed, running, manual takeover, paused — manual and SRM/guardrail-blocked,
// rolled_back, completed), every mutation keeps the legacy two-step
// confirmation via ConfirmDialog (danger button + reason), the stale-plan
// re-fetch guard, the report-evidence gate (advance/resume/takeover-off
// carry expectedReportVersion and re-check evidence on confirm) and the
// byte-exact request bodies of the legacy contract.
export type RolloutReportStatus = 'pending' | 'ready' | 'restricted' | 'unavailable' | 'not_required';

export interface RolloutBlockProps {
  experiment: ExperimentDetail;
  env: string;
  reportStatus: RolloutReportStatus;
  canPublish: boolean;
  locale: string;
  onReportStatus: (status: RolloutReportStatus) => void;
  // onChanged reloads the experiment (legacy route()).
  onChanged: () => void;
  onOpenAudit: () => void;
}

interface StepRow {
  name: string;
  ratio: string;
  min: string;
  obs: string;
  auto: boolean;
}

export function RolloutBlock({
  experiment,
  env,
  reportStatus,
  canPublish,
  locale,
  onReportStatus,
  onChanged,
  onOpenAudit,
}: RolloutBlockProps): ReactElement {
  const { t } = useTranslation();
  const { client } = useHost();
  const { toast } = useToast();
  const confirm = useConfirm();
  const expId = experiment.id;
  const plan = experiment.releasePlan || null;
  const expBase = '/api/v1/admin/experiments/' + encodeURIComponent(expId);
  const base = expBase + '/release-plan';

  // checkReport is the legacy report gate probe: a usable report needs an
  // evidenceVersion; 403 maps to restricted, everything else unavailable.
  const checkReport = async (notify: boolean): Promise<ExperimentReport | null> => {
    try {
      const report = await client.api<ExperimentReport>(expBase + '/report');
      if (!report?.evidenceVersion) throw new Error(t('ro.reportVersionMissing'));
      onReportStatus('ready');
      return report;
    } catch (err) {
      onReportStatus((err as ApiError).status === 403 ? 'restricted' : 'unavailable');
      if (notify) toast(errMsg(err as Parameters<typeof errMsg>[0], t), false);
      return null;
    }
  };

  // decide ports the legacy decide() flow: re-fetch the experiment, refuse
  // the dialog when the plan moved under the operator, then confirm with the
  // action scope summary plus (for report-gated actions) the evidence
  // preview; the POST carries the byte-exact legacy body.
  const decide = async (opts: {
    title: string;
    summary: string;
    reasonLabel?: string;
    reasonRequired?: boolean;
    confirmLabel?: string;
    path: string;
    body?: (reason: string) => Record<string, unknown>;
    success: string;
    requireReport?: boolean;
  }): Promise<void> => {
    const reportRequired = !!opts.requireReport && !['draft', 'reviewed', 'scheduled'].includes(experiment.state || '');
    let openedReport = reportRequired ? await checkReport(true) : null;
    if (reportRequired && !openedReport) return;
    let latest: ExperimentDetail;
    try {
      latest = await client.api<ExperimentDetail>(expBase);
    } catch (err) {
      toast(errMsg(err as Parameters<typeof errMsg>[0], t), false);
      return;
    }
    const current = latest?.releasePlan;
    if (
      !current ||
      (plan &&
        (current.state !== plan.state ||
          current.currentStep !== plan.currentStep ||
          current.manualOverride !== plan.manualOverride ||
          current.stepStartedAt !== plan.stepStartedAt))
    ) {
      toast(t('ro.planChanged'), false);
      onChanged();
      return;
    }
    const step = current.steps?.[current.currentStep];
    const scope = t('ro.actionScope', { id: expId, env: env || '—', step: step?.name || '—', ratio: step?.ratio ?? '—' });
    const accepted = await confirm({
      title: opts.title,
      summary: scope + ' ' + opts.summary,
      preview: openedReport ? <ReportEvidence report={openedReport} locale={locale} /> : undefined,
      reasonLabel: opts.reasonLabel || undefined,
      reasonRequired: opts.reasonRequired ?? true,
      confirmLabel: opts.confirmLabel,
      onConfirm: async ({ reason }) => {
        if (reportRequired) {
          const currentReport = await checkReport(false);
          if (!currentReport) throw new ApiError(0, 'report_unavailable', t('ro.reportUnavailable'));
          if (currentReport.evidenceVersion !== openedReport!.evidenceVersion) {
            openedReport = currentReport;
            throw new ApiError(0, 'report_changed', t('ro.reportChanged'));
          }
        }
        let requestBody: Record<string, unknown> | undefined;
        if (opts.body || reportRequired) {
          requestBody = opts.body ? opts.body(reason) : {};
          if (reportRequired) requestBody.expectedReportVersion = openedReport!.evidenceVersion;
        }
        try {
          await client.api(base + opts.path, { method: 'POST', body: requestBody });
        } catch (err) {
          if ((err as ApiError).status === 409 && reportRequired) await checkReport(false);
          throw err;
        }
      },
    });
    if (accepted) {
      toast(t(opts.success));
      onChanged();
    }
  };

  if (!plan) {
    return <RolloutCreate expId={expId} canPublish={canPublish} onChanged={onChanged} />;
  }

  const steps = plan.steps || [];
  const cur = steps[plan.currentStep];
  const active = plan.state === 'armed' || plan.state === 'running' || plan.state === 'paused';
  const next = planNextGate(plan);
  const done = plan.state === 'completed' ? steps.length : Math.min(plan.currentStep, steps.length);
  const pct = steps.length ? Math.round((100 * done) / steps.length) : 0;
  const gateBlocked = !['ready', 'not_required'].includes(reportStatus) || !canPublish;

  const blocked = plan.state === 'paused' && PLAN_BLOCK_RE.test(plan.pausedReason || '');

  return (
    <>
      {plan.state === 'rolled_back' ? (
        <Banner kind="err">
          <span id="ro-banner">
            <b>{t('ro.rolledBack')}</b>
            <div>{plan.pausedReason || t('ro.rolledBackDefault')}</div>
          </span>
        </Banner>
      ) : plan.state === 'paused' ? (
        <div className={'banner ' + (blocked ? 'err' : 'warn')} id="ro-banner">
          <b>{t(blocked ? 'ro.blocked' : 'ro.paused')}</b>
          <div>{plan.pausedReason || t('ro.noReason')}</div>
          {blocked ? <div className="muted">{t('ro.blockedNote')}</div> : null}
        </div>
      ) : plan.state === 'completed' ? (
        <div className="banner ok" id="ro-banner">
          {t('ro.completed', { ratio: cur ? cur.ratio : '—' })}
        </div>
      ) : plan.state === 'armed' ? (
        <div className="banner" id="ro-banner">
          <Trans i18nKey="ro.armed" components={{ b: <b /> }} />
        </div>
      ) : null}
      {plan.manualOverride ? (
        <Banner kind="warn">
          <span id="ro-takeover-banner">
            <b>{t('ro.takeoverActive')}</b>
            {t('ro.takeoverNote')}
          </span>
        </Banner>
      ) : null}
      <div className="row" id="ro-head">
        <Chip label={t('ro.planState', { state: plan.state })} cls={PLAN_CHIP[plan.state] || 'gray'} />{' '}
        {plan.manualOverride ? (
          <Chip label={t('chip.manualOverride')} cls="red" />
        ) : (
          <Chip label={t('chip.engineAuto')} cls="green" />
        )}{' '}
        <span id="ro-progress-txt">
          {t('ro.stepProgress', { cur: steps.length ? plan.currentStep + 1 : 0, total: steps.length })}
          {cur ? (
            <Trans
              i18nKey="ro.currentStepMeta"
              values={{ name: cur.name, ratio: cur.ratio }}
              components={{ span: <span className="mono" /> }}
            />
          ) : null}
        </span>
      </div>
      <div
        className="bar"
        id="ro-bar"
        role="progressbar"
        aria-valuenow={pct}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label={t('ro.progress')}
      >
        <i style={{ width: pct + '%' }}></i>
      </div>
      <div className="row">
        <span className="muted">{t('ro.nextGate')}</span>{' '}
        <b id="ro-next-eval">{next ? fmtTime(next.toISOString(), locale) : '—'}</b>
        {next ? (
          <span className="muted">{' ' + t('ro.gateStartedNote', { time: fmtTime(plan.stepStartedAt, locale) })}</span>
        ) : plan.state === 'running' ? (
          <span className="muted">{' ' + t('ro.waitClock')}</span>
        ) : plan.state === 'paused' ? (
          <span className="muted">{' ' + t('ro.heldNote')}</span>
        ) : plan.state === 'armed' ? (
          <span className="muted">{' ' + t('ro.armedNote')}</span>
        ) : null}
      </div>
      <h4>{t('ro.planSteps')}</h4>
      <RolloutSteps plan={plan} />
      <h4>
        {t('ro.actionHistory')} <span className="muted">{t('ro.historyNote')}</span>
      </h4>
      <RolloutHistory plan={plan} locale={locale} />
      <div className="row">
        <Button size="sm" id="ro-open-audit" onClick={onOpenAudit}>
          {t('ro.openAudit')}
        </Button>
      </div>
      <div id="ro-report-gate" role="status">
        {reportStatus === 'ready' || reportStatus === 'not_required' ? null : (
          <Banner kind="warn">
            {t(
              reportStatus === 'restricted'
                ? 'ro.reportRestricted'
                : reportStatus === 'unavailable'
                  ? 'ro.reportUnavailable'
                  : 'ro.reportPending',
            )}
          </Banner>
        )}
      </div>
      <div className="row actions">
        <Button size="sm" id="ro-refresh" onClick={onChanged}>
          {t('btn.refresh')}
        </Button>
        {canPublish && (plan.state === 'armed' || plan.state === 'running') ? (
          <Button
            variant="danger"
            id="ro-pause"
            onClick={() =>
              void decide({
                title: t('btn.pause'),
                summary: t('confirm.pause'),
                reasonLabel: t('prompt.pauseReason'),
                confirmLabel: t('btn.pause'),
                path: '/pause',
                body: reason => ({ reason }),
                success: 'toast.planPaused',
              })
            }
          >
            {t('btn.pause')}
          </Button>
        ) : null}
        {canPublish && plan.state === 'paused' ? (
          <Button
            variant="primary"
            id="ro-resume"
            disabled={gateBlocked}
            onClick={() =>
              void decide({
                title: t('btn.resume'),
                summary: t('confirm.resume'),
                confirmLabel: t('btn.resume'),
                path: '/resume',
                success: 'toast.planResumed',
                requireReport: true,
              })
            }
          >
            {t('btn.resume')}
          </Button>
        ) : null}
        {canPublish && plan.state === 'running' ? (
          <Button
            variant="danger"
            id="ro-advance"
            disabled={gateBlocked}
            onClick={() =>
              void decide({
                title: t('btn.advance'),
                summary: t('confirm.advance'),
                reasonLabel: t('prompt.advanceReason'),
                reasonRequired: false,
                confirmLabel: t('btn.advance'),
                path: '/advance',
                body: reason => ({ reason }),
                success: 'toast.planAdvanced',
                requireReport: true,
              })
            }
          >
            {t('btn.advance')}
          </Button>
        ) : null}
        {canPublish && active ? (
          plan.manualOverride ? (
            <Button
              id="ro-takeover-off"
              disabled={gateBlocked}
              onClick={() =>
                void decide({
                  title: t('btn.takeoverOff'),
                  summary: t('confirm.takeoverOff'),
                  reasonLabel: t('prompt.reasonOptional'),
                  reasonRequired: false,
                  confirmLabel: t('btn.takeoverOff'),
                  path: '/takeover',
                  body: reason => ({ enabled: false, reason }),
                  success: 'toast.takeoverOff',
                  requireReport: true,
                })
              }
            >
              {t('btn.takeoverOff')}
            </Button>
          ) : (
            <Button
              variant="danger"
              id="ro-takeover-on"
              onClick={() =>
                void decide({
                  title: t('btn.takeoverOn'),
                  summary: t('confirm.takeoverOn'),
                  reasonLabel: t('prompt.takeoverReason'),
                  confirmLabel: t('btn.takeoverOn'),
                  path: '/takeover',
                  body: reason => ({ enabled: true, reason }),
                  success: 'toast.takeoverOn',
                })
              }
            >
              {t('btn.takeoverOn')}
            </Button>
          )
        ) : null}
      </div>
      <div className="muted">
        {t('ro.registeredBy', { by: plan.createdBy || '—', time: fmtTime(plan.updatedAt, locale) })}
      </div>
    </>
  );
}

// ReportEvidence is the confirm-dialog preview for report-gated rollout
// actions: the decision snapshot plus the evidence version the POST will
// pin.
function ReportEvidence({ report, locale }: { report: ExperimentReport; locale: string }): ReactElement {
  const { t } = useTranslation();
  return (
    <div id="ro-report-evidence">
      <ReportSummary report={report} locale={locale} />
      <p className="muted">
        <strong>{t('ro.reportEvidenceVersion')}:</strong> <span className="mono">{report.evidenceVersion}</span>
      </p>
    </div>
  );
}

// RolloutSteps ports renderRolloutSteps: status chips per step
// (done/stoppedHere/heldHere/current/pending), advance chips (auto/manual
// hold), the ro-current row marker and the gate note.
function RolloutSteps({ plan }: { plan: ReleasePlan }): ReactElement {
  const { t } = useTranslation();
  const steps = plan.steps || [];
  type Row = { step: ReleaseStep; index: number };
  const rows: Row[] = steps.map((step, index) => ({ step, index }));
  const columns: Array<Column<Row>> = [
    { key: 'idx', header: '#', cell: r => String(r.index) },
    { key: 'name', header: t('th.step'), mobilePrimary: true, cell: r => <span className="mono">{r.step.name}</span> },
    { key: 'ratio', header: t('exp.eligShort'), cell: r => r.step.ratio + '%' },
    { key: 'min', header: t('exp.minRuntime'), cell: r => fmtDurNs(r.step.minRuntime, t) },
    { key: 'obs', header: t('ro.obsWindow'), cell: r => fmtDurNs(r.step.observationWindow || 0, t) },
    {
      key: 'advance',
      header: t('ro.advance'),
      cell: r => (
        <Chip
          label={r.step.autoAdvance ? t('chip.auto') : t('chip.manualHold')}
          cls={r.step.autoAdvance ? 'green' : 'amber'}
        />
      ),
    },
    {
      key: 'status',
      header: t('th.status'),
      cell: r => {
        if (plan.state === 'completed' || r.index < plan.currentStep) return <Chip label={t('chip.done')} cls="green" />;
        if (r.index === plan.currentStep) {
          if (plan.state === 'rolled_back') return <Chip label={t('chip.stoppedHere')} cls="red" />;
          return plan.state === 'paused' ? (
            <Chip label={t('chip.heldHere')} cls="amber" />
          ) : (
            <Chip label={t('chip.current')} cls="blue" />
          );
        }
        return <Chip label={t('chip.pending')} cls="gray" />;
      },
    },
  ];
  return (
    <>
      <DataTable
        id="ro-steps-tbl"
        columns={columns}
        rows={rows}
        rowKey={r => String(r.index)}
        emptyText={t('ro.noSteps')}
        rowProps={r => (r.index === plan.currentStep ? { className: 'ro-current' } : {})}
        ariaLabel={t('ro.planSteps')}
        mobileCards
      />
      <div className="muted">{t('ro.gateNote')}</div>
    </>
  );
}

// RolloutHistory ports renderRolloutHistory: newest first, actor/action/step
// and the skip/block detail, engine vs operator distinguished by the actor
// column and the action chip colors.
function RolloutHistory({ plan, locale }: { plan: ReleasePlan; locale: string }): ReactElement {
  const { t } = useTranslation();
  const history = (plan.history || []).slice().reverse(); // newest first
  const columns: Array<Column<PlanEvent>> = [
    // Card title: time + actor together identify the event.
    { key: 'time', header: t('th.time'), className: 'nowrap', mobilePrimary: true, cell: ev => fmtTime(ev.at, locale) },
    { key: 'actor', header: t('th.actor'), mobilePrimary: true, cell: ev => ev.actor || '—' },
    {
      key: 'action',
      header: t('th.action'),
      cell: ev => <Chip label={ev.action} cls={PLAN_ACTION_CHIP[ev.action] || 'gray'} />,
    },
    { key: 'step', header: t('th.step'), cell: ev => String(ev.step ?? '—') },
    { key: 'detail', header: t('th.detailSkip'), cell: ev => ev.detail || '—' },
  ];
  return (
    <DataTable
      id="ro-history-tbl"
      columns={columns}
      rows={history}
      rowKey={ev => ev.at + '|' + ev.action + '|' + ev.step}
      emptyText={t('ro.noActions')}
      ariaLabel={t('ro.actionHistory')}
      mobileCards
    />
  );
}

// RolloutCreate ports renderRolloutCreate + the create click handler: the
// step editor rows (name/ratio/min-runtime/observation-window/auto-advance),
// client-side validation (validateReleaseSteps) and the approval-wrapped
// release_plan.create submission.
export function RolloutCreate({
  expId,
  canPublish,
  onChanged,
}: {
  expId: string;
  canPublish: boolean;
  onChanged: () => void;
}): ReactElement {
  const { t } = useTranslation();
  const { client, ws, env, refreshCapabilities } = useHost();
  const { toast } = useToast();
  const confirm = useConfirm();
  const [rows, setRows] = useState<StepRow[]>([{ name: '', ratio: '', min: '', obs: '', auto: false }]);

  const update = (index: number, patch: Partial<StepRow>) =>
    setRows(current => current.map((row, i) => (i === index ? { ...row, ...patch } : row)));

  const create = async (): Promise<void> => {
    const steps: ReleaseStep[] = rows.map(row => ({
      name: row.name.trim(),
      ratio: Number(row.ratio),
      minRuntime: Math.round(Number(row.min || 0) * NS_HOUR),
      observationWindow: Math.round(Number(row.obs || 0) * NS_HOUR),
      autoAdvance: row.auto,
    }));
    const issue = validateReleaseSteps(steps, t);
    if (issue) {
      toast(issue, false);
      return;
    }
    // requestChange('release_plan.create', …): the reason comes from the
    // confirm dialog (legacy prompt(approval.reason)); empty cancels with
    // the reasonRequired toast.
    const accepted = await confirm({
      title: t('ro.register'),
      reasonLabel: t('approval.reason'),
      onConfirm: async ({ reason }) => {
        try {
          const a = await client.api<{ id: string }>(scopePath(ws, env) + '/approvals', {
            method: 'POST',
            body: { action: 'release_plan.create', object: expId, payload: { steps }, reason },
          });
          toast(t('approval.requested', { id: a.id }));
        } catch (err) {
          if ((err as ApiError).status === 403) await refreshCapabilities();
          throw err;
        }
      },
    });
    if (accepted) {
      // The plan now sits in the approval queue; reload so the card shows it.
      onChanged();
    }
  };

  return (
    <>
      <p className="muted" id="ro-none">
        {t('ro.none')}
      </p>
      <div id="ro-steps-editor">
        {rows.map((row, index) => (
          <div className="vrow" key={index}>
            {/* Wrapping labels give each field a real accessible name (the
                placeholders stay as sighted hints only). */}
            <label>
              <span className="visually-hidden">{t('ro.stepName')}</span>
              <input
                className="rs-name"
                value={row.name}
                placeholder={t('ph.stepName')}
                onChange={ev => update(index, { name: ev.currentTarget.value })}
              />
            </label>
            <label>
              <span className="visually-hidden">{t('ro.stepRatio')}</span>
              <input
                className="rs-ratio"
                type="number"
                step="0.01"
                min="0"
                max="100"
                value={row.ratio}
                placeholder={t('ph.eligPct')}
                onChange={ev => update(index, { ratio: ev.currentTarget.value })}
              />
            </label>
            <label>
              <span className="visually-hidden">{t('ro.stepMinRuntime')}</span>
              <input
                className="rs-min"
                type="number"
                step="0.5"
                min="0"
                value={row.min}
                placeholder={t('ph.minRuntimeH')}
                onChange={ev => update(index, { min: ev.currentTarget.value })}
              />
            </label>
            <label>
              <span className="visually-hidden">{t('ro.stepObsWindow')}</span>
              <input
                className="rs-obs"
                type="number"
                step="0.5"
                min="0"
                value={row.obs}
                placeholder={t('ph.obsWindowH')}
                onChange={ev => update(index, { obs: ev.currentTarget.value })}
              />
            </label>
            <label className="inline">
              <input
                type="checkbox"
                className="rs-auto"
                checked={row.auto}
                onChange={ev => update(index, { auto: ev.currentTarget.checked })}
              />{' '}
              {t('ro.autoAdvance')}
            </label>
            <Button
              size="sm"
              title={t('btn.remove')}
              onClick={() => setRows(current => current.filter((_, i) => i !== index))}
            >
              ✕
            </Button>
          </div>
        ))}
      </div>
      <div className="row">
        <Button
          size="sm"
          id="ro-add-step"
          onClick={() => setRows(current => [...current, { name: '', ratio: '', min: '', obs: '', auto: false }])}
        >
          {t('ro.addStep')}
        </Button>
      </div>
      {canPublish ? (
        <div className="row">
          <Button variant="primary" id="ro-create" onClick={() => void create()}>
            {t('ro.register')}
          </Button>
        </div>
      ) : null}
      <div className="muted">{t('ro.validates')}</div>
    </>
  );
}
