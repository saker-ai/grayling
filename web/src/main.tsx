import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import './i18n'; // side effect: i18next init before first render
import './styles/global.css';
import App from './App';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
