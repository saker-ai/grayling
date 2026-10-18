import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import path from 'node:path';

const proxyTarget = process.env.GRAYLING_WEB_PROXY_TARGET || 'http://localhost:27002';

// The alias table is the single source of truth for cross-package resolution;
// tsconfig.app.json mirrors it (paths). react/react-dom are pinned to this
// package's node_modules so the workspace can never load two copies.
export default defineConfig({
  plugins: [react()],
  base: './',
  resolve: {
    alias: {
      '@saker/grayling-client': path.resolve(import.meta.dirname, '../web-client/src'),
      '@saker/grayling-pages': path.resolve(import.meta.dirname, '../web-pages/src'),
      react: path.resolve(import.meta.dirname, 'node_modules/react'),
      'react/jsx-runtime': path.resolve(import.meta.dirname, 'node_modules/react/jsx-runtime'),
      'react-dom': path.resolve(import.meta.dirname, 'node_modules/react-dom'),
    },
    dedupe: ['react', 'react-dom'],
  },
  build: {
    outDir: './static',
    emptyOutDir: true,
  },
  server: {
    fs: {
      allow: ['.', '../web-client', '../web-pages'],
    },
    proxy: {
      '/api': proxyTarget,
      '/v1': proxyTarget,
    },
  },
});
