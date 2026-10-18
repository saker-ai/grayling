// SegmentMatchProbe ports legacy renderSegmentMatchForm + the #seg-match-run
// handler (web/legacy/segment_editor.js 177-203, app.js 1930-1953). The
// result clears whenever the draft source, the structured form, or the probe
// context changes; a result that arrives after such a change reports itself
// stale instead of rendering. clear() is exposed for the page's editor
// activity hook, matching legacy's delegated input/click clearing.
import { forwardRef, useImperativeHandle, useRef, useState } from 'react';
import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import { errMsg } from '@saker/grayling-client';
import type { SegmentMatchResult } from '@saker/grayling-client';
import { Button } from './Button';

export interface SegmentMatchProbeHandle {
  clear: () => void;
}

function parseSegmentPreviewContext(text: string): Record<string, unknown> {
  const value = JSON.parse(text) as Record<string, unknown>;
  if (!value || typeof value !== 'object' || Array.isArray(value) ||
      (value.targetingKey !== undefined && typeof value.targetingKey !== 'string') ||
      (value.attributes !== undefined && (!value.attributes || typeof value.attributes !== 'object' ||
        Array.isArray(value.attributes)))) throw new Error('bad context');
  return value;
}

const DEFAULT_CONTEXT = JSON.stringify({ targetingKey: 'sample-user', attributes: {} }, null, 2);

export const SegmentMatchProbe = forwardRef<SegmentMatchProbeHandle, {
  prepare: () => Record<string, unknown> | null;
  sourceText: () => string;
  onProbe: (content: Record<string, unknown>, context: Record<string, unknown>) => Promise<SegmentMatchResult>;
}>(function SegmentMatchProbe({ prepare, sourceText, onProbe }, ref): ReactElement {
  const { t } = useTranslation();
  const contextRef = useRef<HTMLTextAreaElement>(null);
  const [busy, setBusy] = useState(false);
  // result: plain text (loading/stale/error) or a match payload to render.
  const [result, setResult] = useState<{ kind: 'text'; text: string } | { kind: 'match'; match: SegmentMatchResult } | null>(null);

  useImperativeHandle(ref, () => ({
    clear: () => setResult(null),
  }), []);

  const run = async (): Promise<void> => {
    const content = prepare();
    if (!content) return;
    let context: Record<string, unknown>;
    try {
      context = parseSegmentPreviewContext(contextRef.current?.value || '');
    } catch {
      setResult({ kind: 'text', text: t('seg.matchBadContext') });
      return;
    }
    const contentText = sourceText();
    const contextText = contextRef.current?.value || '';
    setBusy(true);
    setResult({ kind: 'text', text: t('misc.loading') });
    try {
      const match = await onProbe(content, context);
      if (sourceText() !== contentText || (contextRef.current?.value || '') !== contextText) {
        setResult({ kind: 'text', text: t('seg.matchStale') });
      } else {
        setResult({ kind: 'match', match });
      }
    } catch (error) {
      setResult({ kind: 'text', text: errMsg(error as Parameters<typeof errMsg>[0], t) });
    } finally {
      setBusy(false);
    }
  };

  const state = (matched: boolean): string => t(matched ? 'seg.matchYes' : 'seg.matchNo');

  return (
    <section className="card segment-match">
      <h3>{t('seg.matchTitle')}</h3>
      <p className="muted">{t('seg.matchHelp')}</p>
      <label className="field">
        {t('seg.matchContext')}
        <textarea
          id="seg-match-context"
          className="json"
          rows={5}
          spellCheck={false}
          defaultValue={DEFAULT_CONTEXT}
          ref={contextRef}
          onInput={() => setResult(null)}
        />
      </label>
      <div className="row">
        <Button size="sm" id="seg-match-run" disabled={busy} onClick={() => void run()}>{t('seg.matchRun')}</Button>
      </div>
      <div id="seg-match-result" role="status" aria-live="polite">
        {result?.kind === 'text' ? result.text : null}
        {result?.kind === 'match' ? (
          <div className="flag-diff-values">
            <div>
              <strong>{t('seg.matchDraft')}</strong>
              <p>{state(result.match.draftMatched)}</p>
            </div>
            <div>
              <strong>
                {result.match.publishedMatched == null
                  ? t('seg.matchNoPublished')
                  : t('seg.matchPublished', { n: result.match.publishedRevision })}
              </strong>
              <p>{result.match.publishedMatched == null ? '—' : state(result.match.publishedMatched)}</p>
            </div>
          </div>
        ) : null}
      </div>
    </section>
  );
});
