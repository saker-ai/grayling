import { useState } from 'react';
import type { FormEvent, ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import { ApiError, errMsg } from '@saker/grayling-client';
import { Button } from '../components/Button';
import { useSession } from '../session/SessionProvider';

// LoginPage is the legacy #auth card: same-origin cookie-session login plus
// the cross-origin memory-token mode, with the plain-http refusal for
// non-localhost servers (client.login throws code tls_required).
export function LoginPage(): ReactElement {
  const { t } = useTranslation();
  const { login, authNotice } = useSession();
  const [url, setUrl] = useState(() => {
    try {
      return localStorage.getItem('grayling_url') || '';
    } catch {
      return '';
    }
  });
  const [token, setToken] = useState('');
  const [error, setError] = useState(authNotice ? t(authNotice) : '');
  const [connecting, setConnecting] = useState(false);

  const submit = async (ev: FormEvent) => {
    ev.preventDefault();
    const tok = token.trim();
    if (!tok) {
      setError(t('auth.enterToken'));
      return;
    }
    setConnecting(true);
    setError(t('auth.connecting'));
    try {
      await login(url, tok);
      // success: the provider swaps this page for the shell
    } catch (e) {
      const err = e as ApiError;
      setToken('');
      setError(
        err.code === 'tls_required'
          ? t('auth.httpsRequired')
          : err.status === 401
            ? t('auth.rejected401')
            : errMsg(err, t),
      );
    } finally {
      setConnecting(false);
    }
  };

  return (
    <div className="auth-wrap">
      <div className="auth-card">
        <h1>
          Grayling <span>{t('auth.subtitle')}</span>
        </h1>
        <p className="muted">{t('auth.hint')}</p>
        <form onSubmit={ev => void submit(ev)}>
          <label className="field">
            <span>{t('auth.urlLabel')}</span>
            <input
              id="auth-url"
              value={url}
              onChange={ev => setUrl(ev.currentTarget.value)}
              placeholder={t('auth.urlPh')}
              spellCheck={false}
            />
          </label>
          <label className="field">
            <span>{t('auth.tokenLabel')}</span>
            <input
              id="auth-token"
              type="password"
              value={token}
              onChange={ev => setToken(ev.currentTarget.value)}
              autoComplete="off"
              spellCheck={false}
            />
          </label>
          <div id="auth-error" className="err-text" role="alert">
            {error}
          </div>
          <Button variant="primary" grow type="submit" disabled={connecting} id="btn-connect">
            {t('auth.connect')}
          </Button>
        </form>
      </div>
    </div>
  );
}
