import { useEffect } from 'react';
import type { ReactElement } from 'react';
import { RouterProvider } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { ToastProvider } from './components/Toast';
import { ConfirmProvider } from './components/ConfirmDialog';
import { SessionProvider } from './session/SessionProvider';
import { router } from './routes';

export default function App(): ReactElement {
  const { t, i18n } = useTranslation();

  // Chrome-level side effects that follow the active language.
  useEffect(() => {
    document.title = t('app.title');
    document.documentElement.lang = i18n.language === 'zh' ? 'zh-CN' : 'en';
  }, [t, i18n.language]);

  return (
    <ToastProvider>
      <ConfirmProvider>
        <SessionProvider>
          <RouterProvider router={router} />
        </SessionProvider>
      </ConfirmProvider>
    </ToastProvider>
  );
}
