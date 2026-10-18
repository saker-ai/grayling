import { useCallback, useEffect, useRef, useState } from 'react';
import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { ApiError, errMsg } from '@saker/grayling-client';
import type { ExperimentDetail, ExperimentGuardrail, ExperimentMetricSpec, FlagListItem, MetricPage } from '@saker/grayling-client';
import { Button } from '../components/Button';
import { ErrorBanner } from '../components/ErrorBanner';
import { useToast } from '../components/Toast';
import { EditorDirtyProvider } from '../editors/EditorDirtyContext';
import { useApiData } from '../hooks/useApiData';
import { useHost } from '../host';
import { consoleViewPath, fmtDurNs, scopePath, wsPath } from '../utils';
import { NS_DAY, NS_HOUR } from '../workbench/classify';

// ExperimentCreatePage ports legacy viewExpCreate (web/legacy/app.js
// 2103-2483): the five-step experiment wizard (goal → split → metrics →
// exposure → review). Per-step validation blocks advancing with the legacy
// message + focus target, step 0→1 runs the published-flag readiness check
// (checkWizardFlag), the review step renders the full pre-registration
// contract, and the submit creates a DRAFT only (exp.wizardDraftOnly). Every
// edit marks the wizard dirty; leaving the route (back button, sidebar,
// x-open-flag) runs the exp.wizardDiscard guard through EditorDirtyProvider.
const WIZARD_STEP_KEYS = ['exp.wizard.goal', 'exp.wizard.split', 'exp.wizard.metrics', 'exp.wizard.exposure', 'exp.wizard.review'];
const METRIC_TYPES = ['proportion', 'mean', 'ratio', 'quantile', 'funnel', 'retention'];
const GUARD_TYPES = ['proportion', 'mean', 'funnel', 'retention'];

interface VariantRow {
  name: string;
  prop: string;
}

interface GuardRow {
  key: string;
  type: string;
  dir: string;
  reg: string;
  min: string;
  journey: string;
}

type FlagCheck =
  | { kind: 'idle' }
  | { kind: 'checking' }
  | { kind: 'ready'; revision: number; variants: string }
  | { kind: 'unpublished'; key: string }
  | { kind: 'failed'; message: string };

// experimentNumericIssue ports the legacy numeric guard for the review step:
// every field optional-but-finite, with the legacy ranges; elig may be empty.
export function experimentNumericIssue(values: Record<string, string>): { field: string; range: string } | null {
  const finite = (key: string) => values[key] !== '' && Number.isFinite(Number(values[key]));
  const checks: Array<[string, boolean, string]> = [
    ['alpha', finite('alpha') && Number(values.alpha) > 0 && Number(values.alpha) < 1, '0 < x < 1'],
    ['power', finite('power') && Number(values.power) > 0 && Number(values.power) < 1, '0 < x < 1'],
    ['mde', finite('mde') && Number(values.mde) > 0, 'x > 0'],
    ['minsample', finite('minsample') && Number.isSafeInteger(Number(values.minsample)) && Number(values.minsample) >= 0, 'x ∈ ℤ, x ≥ 0'],
    [
      'minruntime',
      finite('minruntime') && Number(values.minruntime) >= 0 && Number.isSafeInteger(Math.round(Number(values.minruntime) * NS_HOUR)),
      'x ≥ 0',
    ],
    [
      'window',
      finite('window') && Number(values.window) > 0 && Number.isSafeInteger(Math.round(Number(values.window) * NS_DAY)),
      'x > 0',
    ],
    ['elig', values.elig === '' || (finite('elig') && Number(values.elig) >= 0 && Number(values.elig) <= 100), '0 ≤ x ≤ 100'],
  ];
  const failed = checks.find(([, valid]) => !valid);
  return failed ? { field: failed[0], range: failed[2] } : null;
}

// collectExperimentGuardrails ports the legacy guardrail reader: a fully
// blank default row is skipped, anything else must be complete (funnel and
// retention rows additionally carry a JSON journey spec and minSample > 0).
export function collectExperimentGuardrails(
  rows: GuardRow[],
  t: (key: string, options?: Record<string, unknown>) => string,
): ExperimentGuardrail[] {
  return rows.flatMap((row, index) => {
    const metricKey = row.key.trim();
    const threshold = row.reg.trim();
    const sample = row.min.trim();
    const journey = row.journey.trim();
    if (!metricKey && !threshold && !sample && !journey && row.type === 'proportion' && row.dir === 'increase') return [];
    const fail = (): never => {
      throw new ApiError(0, 'validation_failed', t('validation.guardrailRow', { n: index + 1 }));
    };
    const maxRelativeRegression = Number(threshold);
    const minSample = Number(sample);
    if (
      !metricKey ||
      !threshold ||
      !Number.isFinite(maxRelativeRegression) ||
      maxRelativeRegression < 0 ||
      !sample ||
      !Number.isSafeInteger(minSample) ||
      minSample < 0
    ) {
      fail();
    }
    const entry: ExperimentGuardrail = { metricKey, type: row.type, direction: row.dir, maxRelativeRegression, minSample };
    if (row.type === 'funnel' || row.type === 'retention') {
      if (minSample <= 0) fail();
      try {
        const spec: unknown = JSON.parse(journey);
        if (!spec || typeof spec !== 'object' || Array.isArray(spec)) fail();
        if (row.type === 'funnel') entry.funnel = spec as ExperimentGuardrail['funnel'];
        else entry.retention = spec as ExperimentGuardrail['retention'];
      } catch {
        fail();
      }
    }
    return [entry];
  });
}

export function ExperimentCreatePage(): ReactElement {
  const { t } = useTranslation();
  const [dirty, setDirty] = useState(false);
  return (
    <EditorDirtyProvider dirty={dirty} message={t('exp.wizardDiscard')}>
      <WizardBody markDirty={() => setDirty(true)} clearDirty={() => setDirty(false)} />
    </EditorDirtyProvider>
  );
}

function WizardBody({ markDirty, clearDirty }: { markDirty: () => void; clearDirty: () => void }): ReactElement {
  const { t } = useTranslation();
  const { client, ws, env } = useHost();
  const navigate = useNavigate();
  const { toast } = useToast();
  const { data: metricPage, loading, error } = useApiData<MetricPage>(
    signal => client.api<MetricPage>(wsPath(ws) + '/metrics?status=published&limit=200', { signal }),
    [client, ws],
  );
  const publishedMetrics = metricPage?.items || [];

  // --- wizard state (the legacy DOM fields as controlled state) -----------
  const [step, setStep] = useState(0);
  const [wizardError, setWizardError] = useState<{ message: string; field?: string } | null>(null);
  const [flagKey, setFlagKey] = useState('');
  const [hypothesis, setHypothesis] = useState('');
  const [unit, setUnit] = useState('user_id');
  const [layer, setLayer] = useState('');
  const [elig, setElig] = useState('');
  const [mode, setMode] = useState('fixed-split');
  const [variants, setVariants] = useState<VariantRow[]>([
    { name: 'control', prop: '50' },
    { name: 'treatment', prop: '50' },
  ]);
  const [policyVersion, setPolicyVersion] = useState('bandit-v1');
  const [strategy, setStrategy] = useState('thompson');
  const [priorA, setPriorA] = useState('1');
  const [priorB, setPriorB] = useState('1');
  const [epsilon, setEpsilon] = useState('0.1');
  const [feedbackDelay, setFeedbackDelay] = useState('11');
  const [metricRef, setMetricRef] = useState('');
  const [mkey, setMkey] = useState('');
  const [mtype, setMtype] = useState('proportion');
  const [mdir, setMdir] = useState('increase');
  const [numkey, setNumkey] = useState('');
  const [denkey, setDenkey] = useState('');
  const [qlevel, setQlevel] = useState('');
  const [covkey, setCovkey] = useState('');
  const [journey, setJourney] = useState('');
  const [guards, setGuards] = useState<GuardRow[]>([]);
  const [exposureMode, setExposureMode] = useState('');
  const [exposureTrigger, setExposureTrigger] = useState('');
  const [integrationOwner, setIntegrationOwner] = useState('');
  const [alpha, setAlpha] = useState('0.05');
  const [power, setPower] = useState('0.8');
  const [mde, setMde] = useState('0.01');
  const [minsample, setMinsample] = useState('1000');
  const [minruntime, setMinruntime] = useState('168');
  const [windowDays, setWindowDays] = useState('7');
  const [holm, setHolm] = useState('');
  const [flagView, setFlagView] = useState<FlagListItem | null>(null);
  const [flagCheck, setFlagCheck] = useState<FlagCheck>({ kind: 'idle' });
  const [nextBusy, setNextBusy] = useState(false);
  const [createBusy, setCreateBusy] = useState(false);

  // Focus the step heading on step change (legacy $('.wizard-step:not([hidden]) h3').focus()).
  const headingRefs = useRef<Array<HTMLHeadingElement | null>>([]);
  const initialFocus = useRef(false);
  useEffect(() => {
    if (!initialFocus.current) {
      initialFocus.current = true;
      return;
    }
    headingRefs.current[step]?.focus();
  }, [step]);

  // checkWizardFlag ports the legacy readiness check; the captured-key guard
  // replaces the legacy DOM re-read against a stale response.
  const flagKeyRef = useRef(flagKey);
  flagKeyRef.current = flagKey;
  const checkWizardFlag = useCallback(async (): Promise<boolean> => {
    const key = flagKeyRef.current.trim();
    setFlagCheck({ kind: 'checking' });
    try {
      const view = await client.api<FlagListItem>(scopePath(ws, env) + '/flags/' + encodeURIComponent(key));
      if (flagKeyRef.current.trim() !== key) return false;
      const ready = view.current ? view : null;
      setFlagView(ready);
      setFlagCheck(
        ready
          ? {
              kind: 'ready',
              revision: view.currentRevision || 0,
              variants: Object.keys((view.current?.content?.variations as Record<string, unknown> | undefined) || {}).join(', '),
            }
          : { kind: 'unpublished', key },
      );
      return !!ready;
    } catch (err) {
      if (flagKeyRef.current.trim() !== key) return false;
      setFlagView(null);
      setFlagCheck({ kind: 'failed', message: errMsg(err as Parameters<typeof errMsg>[0], t) });
      return false;
    }
  }, [client, ws, env, t]);

  const showWizardError = (message: string, field?: string) => {
    setWizardError({ message, field });
    if (field) document.getElementById(field)?.focus();
  };

  // validateWizardStep ports the legacy per-step guards verbatim (including
  // the dual index===2 blocks: metric fields AND guardrail rows).
  const validateWizardStep = (index: number): [string, string?] | null => {
    if (index === 0) {
      if (!flagKey.trim()) return [t('exp.wizardFlagRequired'), 'x-flag'];
      if (!hypothesis.trim()) return [t('exp.wizardHypothesisRequired'), 'x-hyp'];
    }
    if (index === 1) {
      const eligibility = elig.trim();
      if (eligibility !== '' && (!Number.isFinite(Number(eligibility)) || Number(eligibility) < 0 || Number(eligibility) > 100)) {
        return [t('validation.experimentNumber', { field: t('exp.elig'), range: '0 ≤ x ≤ 100' }), 'x-elig'];
      }
      if (!unit.trim()) return [t('exp.wizardUnitRequired'), 'x-unit'];
      const rows = variants.map(v => ({ name: v.name.trim(), proportion: Number(v.prop) }));
      if (rows.length < 2 || rows.some(v => !v.name)) return [t('toast.need2variants'), 'x-add-var'];
      if (new Set(rows.map(v => v.name)).size !== rows.length) return [t('exp.wizardDuplicateVariant'), 'x-vars'];
      const published = (flagView?.current?.content?.variations as Record<string, unknown> | undefined) || {};
      const unknown = rows.find(v => !Object.hasOwn(published, v.name));
      if (flagView && unknown) return [t('exp.flagVariantMissing', { name: unknown.name }), 'x-vars'];
      const sum = rows.reduce((total, v) => total + v.proportion, 0);
      if (rows.some(v => !Number.isFinite(v.proportion) || v.proportion <= 0) || Math.abs(sum - 100) > 1e-9) {
        return [t('toast.propSum', { sum }), 'x-vars'];
      }
      if (mode === 'bandit' && rows.length !== 2) return [t('exp.banditRequiresProportion'), 'x-mode'];
    }
    if (index === 2) {
      if (!mkey.trim()) return [t('exp.wizardMetricRequired'), 'x-mkey'];
      if (mtype === 'ratio' && !denkey.trim()) return [t('toast.ratioDenominator'), 'x-denkey'];
      if (mtype === 'quantile' && !(Number(qlevel) > 0 && Number(qlevel) < 1)) return [t('toast.quantileLevel'), 'x-qlevel'];
      if (mode === 'bandit' && mtype !== 'proportion') return [t('exp.banditRequiresProportion'), 'x-mtype'];
    }
    if (index === 3) {
      if (!exposureMode) return [t('exp.chooseExposureHelp'), 'x-exposure-mode'];
      if (exposureMode === 'onRender') {
        if (!exposureTrigger.trim()) return [t('exp.exposureTriggerRequired'), 'x-exposure-trigger'];
        if (!integrationOwner.trim()) return [t('exp.integrationOwnerRequired'), 'x-integration-owner'];
      }
    }
    if (index === 2) {
      try {
        collectExperimentGuardrails(guards, t);
      } catch (err) {
        return [errMsg(err as Parameters<typeof errMsg>[0], t), 'x-add-guard'];
      }
    }
    if (index === 4) {
      const issue = experimentNumericIssue({
        alpha,
        power,
        mde,
        minsample,
        minruntime,
        window: windowDays,
        elig,
      });
      if (issue) {
        const labels: Record<string, string> = {
          alpha: 'Alpha',
          power: 'Power',
          mde: 'MDE',
          minsample: t('exp.minSample'),
          minruntime: t('exp.minRuntimeH'),
          window: t('exp.window'),
          elig: t('exp.elig'),
        };
        return [t('validation.experimentNumber', { field: labels[issue.field], range: issue.range }), 'x-' + issue.field];
      }
    }
    return null;
  };

  const showWizardStep = (index: number) => {
    setStep(index);
    setWizardError(null);
  };

  const onNext = async () => {
    const issue = validateWizardStep(step);
    if (issue) {
      showWizardError(...issue);
      return;
    }
    if (step === 0) {
      setNextBusy(true);
      const ready = await checkWizardFlag();
      setNextBusy(false);
      if (!ready) {
        showWizardError(t('exp.flagCheckBlocked'), 'x-flag');
        return;
      }
    }
    showWizardStep(step + 1);
  };

  // Selecting a published metric freezes the metric fields (legacy disabled
  // the inputs and copied the frozen spec in).
  const selectedMetric = publishedMetrics.find(d => d.key + '/v' + d.version === metricRef) || null;
  const onMetricRefChange = (value: string) => {
    setMetricRef(value);
    const def = publishedMetrics.find(d => d.key + '/v' + d.version === value);
    if (!def) return;
    const spec = (def.spec || {}) as Record<string, unknown>;
    setMkey(String(spec.key || ''));
    if (spec.type) setMtype(String(spec.type));
    if (spec.direction) setMdir(String(spec.direction));
    setNumkey(String(spec.numeratorKey || ''));
    setDenkey(String(spec.denominatorKey || ''));
    setQlevel(spec.quantileLevel ? String(spec.quantileLevel) : '');
    setCovkey(String(spec.covariateKey || ''));
    if (spec.funnel || spec.retention) setJourney(JSON.stringify(spec.funnel || spec.retention));
  };

  const onCreate = async () => {
    if (createBusy) return;
    setCreateBusy(true);
    try {
      const ready = await checkWizardFlag();
      if (!ready) {
        showWizardStep(0);
        showWizardError(t('exp.flagCheckBlocked'), 'x-flag');
        return;
      }
      for (let index = 0; index < 5; index++) {
        const issue = validateWizardStep(index);
        if (issue) {
          showWizardStep(index);
          showWizardError(...issue);
          return;
        }
      }
      const rows = variants.map(v => ({ name: v.name.trim(), prop: Number(v.prop) })).filter(v => v.name);
      if (rows.length < 2) {
        toast(t('toast.need2variants'), false);
        return;
      }
      const propSum = rows.reduce((total, v) => total + (v.prop || 0), 0);
      if (Math.abs(propSum - 100) > 1e-9) {
        toast(t('toast.propSum', { sum: propSum }), false);
        return;
      }
      const proportions: Record<string, number> = {};
      rows.forEach(v => {
        proportions[v.name] = v.prop;
      });
      let guardrails: ExperimentGuardrail[];
      try {
        guardrails = collectExperimentGuardrails(guards, t);
      } catch (err) {
        toast(errMsg(err as Parameters<typeof errMsg>[0], t), false);
        return;
      }
      const pm: ExperimentMetricSpec = { key: mkey.trim(), type: mtype, direction: mdir };
      if (mtype === 'ratio') {
        const num = numkey.trim();
        if (num) pm.numeratorKey = num;
        pm.denominatorKey = denkey.trim();
      } else if (mtype === 'quantile') {
        pm.quantileLevel = Number(qlevel);
      } else if (mtype === 'mean') {
        const cov = covkey.trim();
        if (cov) pm.covariateKey = cov;
      } else if (mtype === 'funnel' || mtype === 'retention') {
        try {
          const spec: unknown = JSON.parse(journey);
          if (mtype === 'funnel') pm.funnel = spec as ExperimentMetricSpec['funnel'];
          else pm.retention = spec as ExperimentMetricSpec['retention'];
        } catch {
          toast(t('exp.journeyInvalid'), false);
          return;
        }
      }
      const body: Record<string, unknown> = {
        flagKey: flagKey.trim(),
        unit: unit.trim(),
        variants: rows.map(v => v.name),
        proportions,
        primaryMetric: pm,
        guardrails,
        alpha: Number(alpha),
        power: Number(power),
        mde: Number(mde),
        minSamplePerArm: Number(minsample),
        minRuntime: Math.round(Number(minruntime) * NS_HOUR),
        analysisWindow: Math.round(Number(windowDays) * NS_DAY),
        hypothesis: hypothesis.trim(),
        exposureMode,
      };
      if (exposureMode === 'onRender') {
        body.exposureTrigger = exposureTrigger.trim();
        body.integrationOwner = integrationOwner.trim();
      }
      if (selectedMetric) body.primaryMetricRef = { key: selectedMetric.key, version: selectedMetric.version };
      body.allocationMode = mode;
      if (mode === 'bandit') {
        if (rows.length !== 2 || pm.type !== 'proportion') {
          toast(t('exp.banditRequiresProportion'), false);
          return;
        }
        body.bandit = {
          policyVersion: policyVersion.trim(),
          strategy,
          priorAlpha: Number(priorA),
          priorBeta: Number(priorB),
          epsilon: Number(epsilon),
          feedbackDelay: Math.round(Number(feedbackDelay) * NS_DAY),
        };
      }
      if (!body.flagKey || !pm.key) {
        toast(t('toast.flagMetricRequired'), false);
        return;
      }
      if (mtype === 'ratio' && !pm.denominatorKey) {
        toast(t('toast.ratioDenominator'), false);
        return;
      }
      if (mtype === 'quantile' && !(Number(pm.quantileLevel) > 0 && Number(pm.quantileLevel) < 1)) {
        toast(t('toast.quantileLevel'), false);
        return;
      }
      if (layer.trim()) body.layer = layer.trim();
      if (elig !== '') body.eligibility = Number(elig);
      if (holm.trim()) {
        try {
          body.holmFamily = JSON.parse(holm);
        } catch {
          toast(t('exp.holmInvalid'), false);
          return;
        }
      }
      try {
        const exp = await client.api<ExperimentDetail>(scopePath(ws, env) + '/experiments', { method: 'POST', body });
        toast(t('toast.expCreated', { id: exp.id }));
        clearDirty();
        navigate(consoleViewPath(ws, env, 'experiments', exp.id));
      } catch (err) {
        toast(errMsg(err as Parameters<typeof errMsg>[0], t), false);
      }
    } finally {
      setCreateBusy(false);
    }
  };

  if (loading && !metricPage) return <p className="muted">{t('misc.loading')}</p>;
  if (error) return <ErrorBanner error={error} />;

  const metricFieldsDisabled = !!selectedMetric;

  return (
    <div onInputCapture={markDirty} onChangeCapture={markDirty}>
      <div className="view-head">
        <Button size="sm" id="exp-back" onClick={() => navigate(consoleViewPath(ws, env, 'experiments'))}>
          {t('exp.back')}
        </Button>
        <h2>{t('exp.newTitle')}</h2>
      </div>
      <section className="card">
        <ol className="wizard-progress" id="x-progress" aria-label={t('exp.wizardProgress')}>
          {WIZARD_STEP_KEYS.map((key, index) => (
            <li
              key={key}
              data-wizard-step={index}
              className={index === step ? 'current' : undefined}
              aria-current={index === step ? 'step' : undefined}
            >
              <span>{index + 1}</span>
              {t(key)}
            </li>
          ))}
        </ol>
        <div className="banner err" id="x-error" role="alert" hidden={!wizardError}>
          {wizardError?.message}
        </div>
        <div className="wizard-step" data-step="0" hidden={step !== 0}>
          <h3 tabIndex={-1} ref={el => { headingRefs.current[0] = el; }}>
            {t('exp.wizard.goal')}
          </h3>
          <p className="muted">{t('exp.wizardGoalHelp', { ws, env })}</p>
          <div className="form-grid">
            <label className="field">
              {t('exp.flagKey')}
              <input
                id="x-flag"
                placeholder="my-flag"
                value={flagKey}
                onChange={ev => {
                  setFlagKey(ev.currentTarget.value);
                  setFlagView(null);
                  setFlagCheck({ kind: 'idle' });
                }}
              />
            </label>
          </div>
          <div id="x-flag-check" role="status" aria-live="polite">
            {flagCheck.kind === 'checking' ? (
              t('exp.flagCheckLoading')
            ) : flagCheck.kind === 'ready' ? (
              <div className="banner ok">{t('exp.flagCheckReady', { n: flagCheck.revision, variants: flagCheck.variants })}</div>
            ) : flagCheck.kind === 'unpublished' ? (
              <div className="banner warn">
                {t('exp.flagCheckUnpublished', { key: flagCheck.key })}{' '}
                <button
                  type="button"
                  className="btn sm"
                  id="x-open-flag"
                  onClick={() => navigate(consoleViewPath(ws, env, 'flags', flagKey.trim()))}
                >
                  {t('exp.flagOpen')}
                </button>
              </div>
            ) : flagCheck.kind === 'failed' ? (
              <div className="banner warn">{t('exp.flagCheckFailed') + ' ' + flagCheck.message}</div>
            ) : null}
          </div>
          <label className="field">
            {t('exp.hypothesis')}
            <textarea id="x-hyp" rows={2} value={hypothesis} onChange={ev => setHypothesis(ev.currentTarget.value)}></textarea>
          </label>
        </div>
        <div className="wizard-step" data-step="1" hidden={step !== 1}>
          <h3 tabIndex={-1} ref={el => { headingRefs.current[1] = el; }}>
            {t('exp.wizard.split')}
          </h3>
          <div className="form-grid">
            <label className="field">
              {t('exp.unit')}
              <input id="x-unit" value={unit} onChange={ev => setUnit(ev.currentTarget.value)} />
            </label>
            <label className="field">
              {t('exp.layer')}
              <input id="x-layer" value={layer} onChange={ev => setLayer(ev.currentTarget.value)} />
            </label>
            <label className="field">
              {t('exp.elig')}
              <input id="x-elig" type="number" step="1" min="0" max="100" value={elig} onChange={ev => setElig(ev.currentTarget.value)} />
            </label>
            <label className="field">
              {t('exp.allocationMode')}
              <select id="x-mode" value={mode} onChange={ev => setMode(ev.currentTarget.value)}>
                <option value="fixed-split">fixed-split</option>
                <option value="bandit">bandit</option>
              </select>
            </label>
          </div>
          <h4>{t('exp.variantsHead')}</h4>
          <div id="x-vars">
            {variants.map((row, index) => (
              <div className="vrow" key={index}>
                <input
                  className="v-name"
                  placeholder={t('ph.variantName')}
                  value={row.name}
                  onChange={ev => {
                    const value = ev.currentTarget.value;
                    setVariants(current => current.map((v, i) => (i === index ? { ...v, name: value } : v)));
                  }}
                />
                <input
                  className="v-prop"
                  type="number"
                  step="1"
                  min="0"
                  max="100"
                  placeholder={t('ph.percent')}
                  value={row.prop}
                  onChange={ev => {
                    const value = ev.currentTarget.value;
                    setVariants(current => current.map((v, i) => (i === index ? { ...v, prop: value } : v)));
                  }}
                />
                <Button
                  size="sm"
                  title={t('btn.remove')}
                  onClick={() => {
                    markDirty();
                    setVariants(current => current.filter((_, i) => i !== index));
                  }}
                >
                  ✕
                </Button>
              </div>
            ))}
          </div>
          <Button
            size="sm"
            id="x-add-var"
            onClick={() => {
              markDirty();
              setVariants(current => [...current, { name: '', prop: '' }]);
            }}
          >
            {t('exp.addVariant')}
          </Button>
          <div className="form-grid" id="x-bandit" style={{ display: mode === 'bandit' ? '' : 'none' }}>
            <label className="field">
              {t('exp.policyVersion')}
              <input id="x-policy-version" value={policyVersion} onChange={ev => setPolicyVersion(ev.currentTarget.value)} />
            </label>
            <label className="field">
              {t('exp.strategy')}
              <select id="x-strategy" value={strategy} onChange={ev => setStrategy(ev.currentTarget.value)}>
                <option value="thompson">Thompson</option>
                <option value="epsilon-greedy">ε-greedy</option>
              </select>
            </label>
            <label className="field">
              Prior α
              <input id="x-prior-a" type="number" step="0.1" value={priorA} onChange={ev => setPriorA(ev.currentTarget.value)} />
            </label>
            <label className="field">
              Prior β
              <input id="x-prior-b" type="number" step="0.1" value={priorB} onChange={ev => setPriorB(ev.currentTarget.value)} />
            </label>
            <label className="field">
              ε
              <input id="x-epsilon" type="number" step="0.01" value={epsilon} onChange={ev => setEpsilon(ev.currentTarget.value)} />
            </label>
            <label className="field">
              {t('exp.feedbackDelayD')}
              <input id="x-feedback-delay" type="number" step="1" value={feedbackDelay} onChange={ev => setFeedbackDelay(ev.currentTarget.value)} />
            </label>
          </div>
        </div>
        <div className="wizard-step" data-step="2" hidden={step !== 2}>
          <h3 tabIndex={-1} ref={el => { headingRefs.current[2] = el; }}>
            {t('exp.wizard.metrics')}
          </h3>
          <h4>{t('exp.primaryMetric')}</h4>
          <label className="field">
            {t('metrics.selectPrimary')}
            <select id="x-mref" value={metricRef} onChange={ev => onMetricRefChange(ev.currentTarget.value)}>
              <option value="">{t('metrics.inline')}</option>
              {publishedMetrics.map(d => (
                <option key={d.key + '/v' + d.version} value={d.key + '/v' + d.version}>
                  {d.displayName + ' · ' + d.key + ' v' + d.version}
                </option>
              ))}
            </select>
          </label>
          <div className="form-grid">
            <label className="field">
              {t('exp.metricKey')}
              <input id="x-mkey" placeholder="checkout_success" value={mkey} disabled={metricFieldsDisabled} onChange={ev => setMkey(ev.currentTarget.value)} />
            </label>
            <label className="field">
              {t('th.type')}
              <select id="x-mtype" value={mtype} disabled={metricFieldsDisabled} onChange={ev => setMtype(ev.currentTarget.value)}>
                {METRIC_TYPES.map(type => (
                  <option key={type}>{type}</option>
                ))}
              </select>
            </label>
            <label className="field">
              {t('exp.betterDir')}
              <select id="x-mdir" value={mdir} disabled={metricFieldsDisabled} onChange={ev => setMdir(ev.currentTarget.value)}>
                <option value="increase">increase</option>
                <option value="decrease">decrease</option>
              </select>
            </label>
          </div>
          <div className="form-grid" id="x-metric-adv">
            <label className="field" id="x-numkey-wrap" style={{ display: mtype === 'ratio' ? '' : 'none' }}>
              {t('exp.numeratorKey')} <span className="muted">{t('exp.numeratorHint')}</span>
              <input id="x-numkey" placeholder="revenue" value={numkey} disabled={metricFieldsDisabled} onChange={ev => setNumkey(ev.currentTarget.value)} />
            </label>
            <label className="field" id="x-denkey-wrap" style={{ display: mtype === 'ratio' ? '' : 'none' }}>
              {t('exp.denominatorKey')} <span className="muted">{t('exp.denominatorHint')}</span>
              <input id="x-denkey" placeholder="sessions" value={denkey} disabled={metricFieldsDisabled} onChange={ev => setDenkey(ev.currentTarget.value)} />
            </label>
            <label className="field" id="x-qlevel-wrap" style={{ display: mtype === 'quantile' ? '' : 'none' }}>
              {t('exp.quantileLevel')} <span className="muted">{t('exp.quantileHint')}</span>
              <input
                id="x-qlevel"
                type="number"
                step="0.01"
                min="0.01"
                max="0.99"
                placeholder="0.95"
                value={qlevel}
                disabled={metricFieldsDisabled}
                onChange={ev => setQlevel(ev.currentTarget.value)}
              />
            </label>
            <label className="field" id="x-covkey-wrap" style={{ display: mtype === 'mean' ? '' : 'none' }}>
              {t('exp.covariateKey')} <span className="muted">{t('exp.covariateHint')}</span>
              <input id="x-covkey" placeholder="pre_period_value" value={covkey} disabled={metricFieldsDisabled} onChange={ev => setCovkey(ev.currentTarget.value)} />
            </label>
            <label className="field" id="x-journey-wrap" style={{ display: mtype === 'funnel' || mtype === 'retention' ? '' : 'none' }}>
              {t('exp.journeySpec')}
              <textarea
                id="x-journey"
                rows={3}
                placeholder='{"steps":["view","pay"],"dedupUnit":"user","stepTimeout":3600000000000}'
                value={journey}
                disabled={metricFieldsDisabled}
                onChange={ev => setJourney(ev.currentTarget.value)}
              ></textarea>
            </label>
          </div>
          <h4>{t('exp.guardrails')}</h4>
          <div id="x-guards">
            {guards.map((row, index) => (
              <div className="vrow" key={index}>
                <label>
                  {t('exp.metricKey')}
                  <input
                    className="g-key"
                    required
                    value={row.key}
                    onChange={ev => {
                      const value = ev.currentTarget.value;
                      setGuards(current => current.map((g, i) => (i === index ? { ...g, key: value } : g)));
                    }}
                  />
                </label>
                <label>
                  {t('th.type')}
                  <select
                    className="g-type"
                    value={row.type}
                    onChange={ev => {
                      const value = ev.currentTarget.value;
                      setGuards(current => current.map((g, i) => (i === index ? { ...g, type: value } : g)));
                    }}
                  >
                    {GUARD_TYPES.map(type => (
                      <option key={type}>{type}</option>
                    ))}
                  </select>
                </label>
                <label>
                  {t('exp.guardDirection')}
                  <select
                    className="g-dir"
                    value={row.dir}
                    onChange={ev => {
                      const value = ev.currentTarget.value;
                      setGuards(current => current.map((g, i) => (i === index ? { ...g, dir: value } : g)));
                    }}
                  >
                    <option value="increase">{t('exp.increaseGood')}</option>
                    <option value="decrease">{t('exp.decreaseGood')}</option>
                  </select>
                </label>
                <label>
                  {t('ph.maxReg')}
                  <input
                    className="g-reg"
                    type="number"
                    step="0.01"
                    placeholder={t('ph.maxReg')}
                    value={row.reg}
                    onChange={ev => {
                      const value = ev.currentTarget.value;
                      setGuards(current => current.map((g, i) => (i === index ? { ...g, reg: value } : g)));
                    }}
                  />
                </label>
                <label>
                  {t('exp.minSample')}
                  <input
                    className="g-min"
                    type="number"
                    min="0"
                    step="1"
                    value={row.min}
                    onChange={ev => {
                      const value = ev.currentTarget.value;
                      setGuards(current => current.map((g, i) => (i === index ? { ...g, min: value } : g)));
                    }}
                  />
                </label>
                <label>
                  {t('exp.journeySpec')}
                  <textarea
                    className="g-journey"
                    rows={2}
                    placeholder={t('exp.journeySpec') + ' JSON'}
                    value={row.journey}
                    onChange={ev => {
                      const value = ev.currentTarget.value;
                      setGuards(current => current.map((g, i) => (i === index ? { ...g, journey: value } : g)));
                    }}
                  ></textarea>
                </label>
                <Button
                  size="sm"
                  aria-label={t('btn.remove')}
                  onClick={() => {
                    markDirty();
                    setGuards(current => current.filter((_, i) => i !== index));
                  }}
                >
                  {t('btn.remove')}
                </Button>
              </div>
            ))}
          </div>
          <Button
            size="sm"
            id="x-add-guard"
            onClick={() => {
              markDirty();
              setGuards(current => [...current, { key: '', type: 'proportion', dir: 'increase', reg: '', min: '', journey: '' }]);
            }}
          >
            {t('exp.addGuardrail')}
          </Button>
        </div>
        <div className="wizard-step" data-step="3" hidden={step !== 3}>
          <h3 tabIndex={-1} ref={el => { headingRefs.current[3] = el; }}>
            {t('exp.wizard.exposure')}
          </h3>
          <label className="field">
            {t('exp.exposureMode')}
            <select id="x-exposure-mode" value={exposureMode} onChange={ev => setExposureMode(ev.currentTarget.value)}>
              <option value="">{t('exp.chooseExposureMode')}</option>
              <option value="onDecision">{t('exp.onDecision')}</option>
              <option value="onRender">{t('exp.onRender')}</option>
            </select>
          </label>
          <p className="muted" id="x-exposure-help">
            {t(
              exposureMode === 'onRender'
                ? 'exp.onRenderHelp'
                : exposureMode === 'onDecision'
                  ? 'exp.onDecisionHelp'
                  : 'exp.chooseExposureHelp',
            )}
          </p>
          <div className="form-grid" id="x-render-fields" hidden={exposureMode !== 'onRender'}>
            <label className="field">
              {t('exp.exposureTrigger')}
              <input
                id="x-exposure-trigger"
                maxLength={200}
                placeholder={t('exp.exposureTriggerExample')}
                value={exposureTrigger}
                onChange={ev => setExposureTrigger(ev.currentTarget.value)}
              />
            </label>
            <label className="field">
              {t('exp.integrationOwner')}
              <input
                id="x-integration-owner"
                maxLength={200}
                placeholder={t('exp.integrationOwnerExample')}
                value={integrationOwner}
                onChange={ev => setIntegrationOwner(ev.currentTarget.value)}
              />
            </label>
          </div>
        </div>
        <div className="wizard-step" data-step="4" hidden={step !== 4}>
          <h3 tabIndex={-1} ref={el => { headingRefs.current[4] = el; }}>
            {t('exp.wizard.review')}
          </h3>
          <h4>{t('exp.statsHead')}</h4>
          <div className="form-grid">
            <label className="field">
              Alpha
              <input id="x-alpha" type="number" step="0.001" value={alpha} onChange={ev => setAlpha(ev.currentTarget.value)} />
            </label>
            <label className="field">
              Power
              <input id="x-power" type="number" step="0.01" value={power} onChange={ev => setPower(ev.currentTarget.value)} />
            </label>
            <label className="field">
              MDE
              <input id="x-mde" type="number" step="0.001" value={mde} onChange={ev => setMde(ev.currentTarget.value)} />
            </label>
            <label className="field">
              {t('exp.minSample')}
              <input id="x-minsample" type="number" value={minsample} onChange={ev => setMinsample(ev.currentTarget.value)} />
            </label>
            <label className="field">
              {t('exp.minRuntimeH')}
              <input id="x-minruntime" type="number" step="0.5" value={minruntime} onChange={ev => setMinruntime(ev.currentTarget.value)} />
            </label>
            <label className="field">
              {t('exp.windowD')}
              <input id="x-window" type="number" step="1" value={windowDays} onChange={ev => setWindowDays(ev.currentTarget.value)} />
            </label>
          </div>
          <label className="field">
            {t('exp.holmFamily')}
            <textarea
              id="x-holm"
              rows={5}
              placeholder='{"id":"primary","hypotheses":[...]}'
              value={holm}
              onChange={ev => setHolm(ev.currentTarget.value)}
            ></textarea>
          </label>
          <p className="muted">{t('exp.holmHelp')}</p>
          <div className="wizard-review" id="x-review">
            <h4>{t('exp.wizardContract')}</h4>
            <dl className="kv">
              <dt>{t('exp.flagKey')}</dt>
              <dd>{flagKey.trim()}</dd>
              <dt>{t('exp.flagRevision')}</dt>
              <dd>{'r' + (flagView?.currentRevision || 0)}</dd>
              <dt>{t('exp.hypothesis')}</dt>
              <dd>{hypothesis.trim()}</dd>
              <dt>{t('exp.unit')}</dt>
              <dd>{unit.trim()}</dd>
              <dt>{t('exp.eligShort')}</dt>
              <dd>{(elig || '100') + '%'}</dd>
              <dt>{t('th.variants')}</dt>
              <dd>{variants.map(v => v.name.trim() + ' ' + v.prop + '%').join(' · ')}</dd>
              <dt>{t('exp.primaryMetric')}</dt>
              <dd>{selectedMetric ? selectedMetric.key + ' v' + selectedMetric.version : mkey.trim()}</dd>
              {selectedMetric ? (
                <>
                  <dt>{t('report.attributionWindow')}</dt>
                  <dd>{fmtDurNs(selectedMetric.attributionWindow, t)}</dd>
                </>
              ) : null}
              <dt>{t('exp.guardrails')}</dt>
              <dd>{guards.map(g => g.key.trim()).filter(Boolean).join(' · ') || t('misc.none')}</dd>
              <dt>{t('exp.exposureMode')}</dt>
              <dd>{t(exposureMode === 'onRender' ? 'exp.onRender' : 'exp.onDecision')}</dd>
              {exposureMode === 'onRender' ? (
                <>
                  <dt>{t('exp.exposureTrigger')}</dt>
                  <dd>{exposureTrigger.trim()}</dd>
                  <dt>{t('exp.integrationOwner')}</dt>
                  <dd>{integrationOwner.trim()}</dd>
                </>
              ) : null}
              <dt>{t('exp.window')}</dt>
              <dd>{windowDays + ' ' + t('exp.wizardDays')}</dd>
            </dl>
            <p>{t('exp.wizardSampleRule', { sample: minsample, runtime: minruntime })}</p>
            <p className="muted">{t('exp.wizardDraftOnly')}</p>
          </div>
          <div className="row">
            <Button variant="primary" id="x-create" disabled={createBusy} onClick={() => void onCreate()}>
              {t('exp.create')}
            </Button>
          </div>
        </div>
        <div className="row wizard-actions">
          <Button id="x-prev" hidden={step === 0} onClick={() => showWizardStep(step - 1)}>
            {t('exp.wizardBack')}
          </Button>
          <span className="muted" id="x-step-label">
            {t('exp.wizardStep', { current: step + 1, total: 5 })}
          </span>
          <Button variant="primary" id="x-next" hidden={step === 4} disabled={nextBusy} onClick={() => void onNext()}>
            {t('exp.wizardNext')}
          </Button>
        </div>
      </section>
    </div>
  );
}
