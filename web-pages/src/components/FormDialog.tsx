import { useRef, useState } from 'react';
import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import { errMsg } from '@saker/grayling-client';
import { Button } from './Button';
import { Dialog } from './Dialog';
import { useToast } from './Toast';

// FormDialog is the legacy promptForm as a component: a field-array-driven
// modal form. Fields are UNCONTROLLED (defaultValue) so the native validity
// flow (setCustomValidity/reportValidity) keeps working exactly like the
// legacy implementation.
export interface FormField {
  name: string;
  label: string;
  type?: string;
  value?: string;
  placeholder?: string;
  options?: Array<{ value: string; label: string } | string>;
  rows?: number;
  required?: boolean;
  hint?: string;
  validate?: (value: string) => string;
}

export interface FormDialogProps {
  title: string;
  fields: FormField[];
  submitLabel?: string;
  onSubmit: (values: Record<string, string>) => Promise<void> | void;
  onClose: () => void;
}

type FieldElement = HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement;

export function FormDialog({ title, fields, submitLabel, onSubmit, onClose }: FormDialogProps): ReactElement {
  const { t } = useTranslation();
  const { toast } = useToast();
  const [busy, setBusy] = useState(false);
  const bodyRef = useRef<HTMLDivElement>(null);

  const runValidate = (f: FormField, el: FieldElement, value: string) => {
    if (!f.validate) return;
    const message = f.validate(value);
    el.setCustomValidity(message);
    el.setAttribute('aria-invalid', String(!!message));
  };

  const validateOn = (f: FormField) => (ev: React.SyntheticEvent<HTMLInputElement | HTMLTextAreaElement>) => {
    runValidate(f, ev.currentTarget, ev.currentTarget.value.trim());
  };

  const fieldEl = (f: FormField): ReactElement => {
    const shared = {
      name: f.name,
      defaultValue: f.value ?? '',
      required: f.required,
      placeholder: f.placeholder,
      'aria-describedby': f.hint ? `modal-hint-${f.name}` : undefined,
      onInput: validateOn(f),
      onBlur: validateOn(f),
    };
    if (f.type === 'textarea') {
      return <textarea rows={f.rows || 6} {...shared} />;
    }
    if (f.type === 'select') {
      return (
        <select name={f.name} defaultValue={f.value ?? ''} aria-describedby={f.hint ? `modal-hint-${f.name}` : undefined}>
          {(f.options || []).map(o => {
            const value = typeof o === 'string' ? o : o.value;
            const label = typeof o === 'string' ? o : o.label;
            return (
              <option key={value} value={value}>
                {label}
              </option>
            );
          })}
        </select>
      );
    }
    return <input type={f.type || 'text'} {...shared} />;
  };

  const submit = async () => {
    if (busy) return;
    const root = bodyRef.current;
    if (!root) return;
    root
      .querySelectorAll<HTMLInputElement | HTMLTextAreaElement>('input, textarea')
      .forEach(el => {
        if (el.required) el.value = el.value.trim();
      });
    for (const f of fields) {
      const input = root.querySelector<FieldElement>(`[name="${f.name}"]`);
      if (input) runValidate(f, input, input.value);
    }
    const invalid = Array.from(root.querySelectorAll<FieldElement>('input, select, textarea')).find(
      el => !el.reportValidity(),
    );
    if (invalid) {
      invalid.focus();
      return;
    }
    setBusy(true);
    const vals: Record<string, string> = {};
    for (const f of fields) {
      vals[f.name] = root.querySelector<FieldElement>(`[name="${f.name}"]`)?.value ?? '';
    }
    try {
      await onSubmit(vals);
      onClose();
    } catch (e) {
      toast(errMsg(e as Parameters<typeof errMsg>[0], t), false);
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={title}
      busy={busy}
      onClose={onClose}
      footer={
        <>
          <Button data-x="cancel" onClick={onClose} disabled={busy}>
            {t('btn.cancel')}
          </Button>
          <Button variant="primary" onClick={() => void submit()} disabled={busy}>
            {submitLabel || t('btn.ok')}
          </Button>
        </>
      }
    >
      <div ref={bodyRef}>
        {fields.map(f => (
          <label className="field" key={f.name}>
            <span>{f.label}</span>
            {fieldEl(f)}
            {f.hint ? (
              <small id={`modal-hint-${f.name}`} className="muted">
                {f.hint}
              </small>
            ) : null}
          </label>
        ))}
      </div>
    </Dialog>
  );
}
