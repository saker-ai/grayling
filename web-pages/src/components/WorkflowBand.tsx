import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';

// WorkflowBand ports legacy renderFlagWorkflow (web/legacy/app.js): the
// editing → submitted → approved → published strip with aria-current on the
// active step and the "what happens next" line. The legacy key assembly
// ('flags.status' + Capitalized) becomes an explicit map here — the i18n
// parity test only sees full literal keys.
export type WorkflowState = 'editing' | 'submitted' | 'approved' | 'published';

const STEP_KEYS: Record<WorkflowState, string> = {
  editing: 'flags.statusEditing',
  submitted: 'flags.statusSubmitted',
  approved: 'flags.statusApproved',
  published: 'chip.published',
};

const NEXT_KEYS: Record<WorkflowState, string> = {
  editing: 'flags.next.editing',
  submitted: 'flags.next.submitted',
  approved: 'flags.next.approved',
  published: 'flags.next.published',
};

const STEPS: WorkflowState[] = ['editing', 'submitted', 'approved', 'published'];

export function WorkflowBand({ state }: { state: WorkflowState }): ReactElement {
  const { t } = useTranslation();
  return (
    <section className="flag-workflow" aria-label={t('flags.workflow')}>
      <ol>
        {STEPS.map(step => (
          <li key={step} {...(step === state ? { 'aria-current': 'step' } : {})}>
            {t(STEP_KEYS[step])}
          </li>
        ))}
      </ol>
      <p>{t(NEXT_KEYS[state])}</p>
    </section>
  );
}
