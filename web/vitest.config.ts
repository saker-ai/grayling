import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';
import path from 'node:path';

// Keep the alias table in sync with vite.config.ts — same source layout.
export default defineConfig({
  plugins: [react()],
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
  test: {
    environment: 'jsdom',
  },
});
