import { useState } from 'react';
import type { FormEvent, ReactElement } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { errMsg } from '@saker/grayling-client';
import type { ApiError, DebugEvalResponse, DebugEvalResult } from '@saker/grayling-client';
import { Banner } from '../components/Banner';
import { Button } from '../components/Button';
import { Chip } from '../components/Chip';
import { useToast } from '../components/Toast';
import { useHost } from '../host';
import { diagnosticLabel, scopePath } from '../utils';

// DebugPage ports legacy viewDebug: a free-form flag evaluation form. It
// performs no fetch on mount (the legacy view renders the form immediately)
// and never records exposures.
export function DebugPage(): ReactElement {
  const { t } = useTranslation();
  const { client, ws, env } = useHost();
  const { toast } = useToast();
  const [result, setResult] = useState<DebugEvalResponse | null>(null);

  if (!ws || !env) {
    return (
      <Banner kind="warn">
        <Trans i18nKey="banner.noScope" />
      </Banner>
    );
  }

  const submit = async (ev: FormEvent<HTMLFormElement>) => {
    ev.preventDefault();
    const form = ev.currentTarget;
    const attrsRaw = (form.elements.namedItem('d-attrs') as HTMLTextAreaElement).value;
    let attrs: unknown;
    try {
      attrs = JSON.parse(attrsRaw || '{}');
    } catch (e) {
      toast(t('toast.attrsInvalid', { msg: (e as Error).message }), false);
      return;
    }
    const flagKey = (form.elements.namedItem('d-key') as HTMLInputElement).value.trim();
    if (!flagKey) {
      toast(t('toast.flagKeyRequired'), false);
      return;
    }
    const targetingKey = (form.elements.namedItem('d-tk') as HTMLInputElement).value.trim();
    try {
      const res = await client.api<DebugEvalResponse>(scopePath(ws, env) + '/debug/eval', {
        method: 'POST',
        body: { flagKey, context: { targetingKey, attributes: attrs } },
      });
      setResult(res);
    } catch (e) {
      toast(errMsg(e as ApiError, t), false);
    }
  };

  const r: DebugEvalResult = result?.result ?? {};
  return (
    <>
      <div className="view-head">
        <h2>{t('debug.title')}</h2>
        <span className="muted">{t('debug.hint')}</span>
      </div>
      <section className="card">
        <form onSubmit={ev => void submit(ev)}>
          <div className="form-grid">
            <label className="field">
              {t('exp.flagKey')}
              <input name="d-key" placeholder="my-flag" />
            </label>
            <label className="field">
              {t('debug.targetingKey')}
              <input name="d-tk" placeholder={t('debug.tkPh')} />
            </label>
          </div>
          <label className="field">
            {t('debug.attrs')}
            <textarea name="d-attrs" className="json" rows={8} spellCheck={false} defaultValue={'{\n  \n}'} />
          </label>
          <div className="row">
            <Button variant="primary" type="submit" id="d-run">
              {t('btn.evaluate')}
            </Button>
          </div>
        </form>
      </section>
      <section className={result ? 'card' : 'card hidden'} id="d-out">
        <h3>{t('debug.result')}</h3>
        <div id="d-summary" className="row">
          {result ? (
            <>
              <Chip cls="blue" label={t('debug.valueChip', { v: JSON.stringify(r.value) })} />{' '}
              <Chip cls="teal" label={t('debug.variationChip', { v: r.variationType || '—' })} />{' '}
              <Chip cls={r.failed ? 'red' : 'green'} label={t('debug.reasonChip', { v: diagnosticLabel(r.reason, t) })} />
              {r.errorCode ? (
                <>
                  {' '}
                  <Chip cls="red" label={t('debug.errorChip', { v: r.errorCode })} />
                </>
              ) : null}
              {r.bucket != null ? <span className="muted"> {t('debug.bucket', { n: r.bucket })}</span> : null}{' '}
              <span className="muted">
                {t('debug.manifest', { n: result.manifestRevision ?? '—' })}
                {result.note ? ' · ' + result.note : ''}
              </span>
            </>
          ) : null}
        </div>
        <pre id="d-json" className="pre">
          {result ? JSON.stringify(result, null, 2) : null}
        </pre>
      </section>
    </>
  );
}
