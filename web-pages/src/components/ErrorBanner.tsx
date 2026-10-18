import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import { errMsg } from '@saker/grayling-client';
import type { ApiError } from '@saker/grayling-client';
import { Banner } from './Banner';
import { Button } from './Button';
import { useHost } from '../host';

// ErrorBanner ports legacy errBanner(): the localized error message, a 501
// "not enabled" footnote, and on 403 a re-enter-token button that drops back
// to the login screen via the host bridge.
export function ErrorBanner({ error }: { error: unknown }): ReactElement {
  const { t } = useTranslation();
  const { reauth } = useHost();
  const e = error as ApiError;
  return (
    <Banner kind="err">
      {errMsg(e, t)}
      {e.status === 501 ? <div className="muted">{t('banner.notEnabled501')}</div> : null}
      {e.status === 403 ? (
        <>
          {' '}
          <Button size="sm" id="btn-reauth2" onClick={reauth}>
            {t('btn.reenterToken')}
          </Button>
        </>
      ) : null}
    </Banner>
  );
}
