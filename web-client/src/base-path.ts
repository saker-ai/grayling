// Copied verbatim from web-shared/src/base-path.ts (saker-ai/web-shared) —
// web-shared is the webhub host-contract layer and is intentionally NOT a
// dependency of grayling, so this single pure-function module is vendored
// here. Keep it in sync with the upstream file if it ever changes.

export function normalizeBasePath(path: string): string {
  const trimmed = path.trim()
  if (!trimmed || trimmed === '/' || trimmed === '.' || trimmed === './') return ''
  return `/${trimmed.replace(/^\/+|\/+$/g, '')}`
}

declare global {
  interface Window {
    __SAKER_NATIVE_API_BASE__?: string
  }
}

export function nativeApiBase(): string {
  if (typeof window === 'undefined') return ''
  const value = window.__SAKER_NATIVE_API_BASE__
  return value ? normalizeBasePath(value) : ''
}

export function setNativeApiBase(base: string): void {
  if (typeof window === 'undefined') return
  window.__SAKER_NATIVE_API_BASE__ = base
}

export function clearNativeApiBase(): void {
  if (typeof window === 'undefined') return
  delete window.__SAKER_NATIVE_API_BASE__
}

export function basePathFromScript(marker = '/assets/'): string {
  if (typeof document === 'undefined' || typeof window === 'undefined') return ''
  const scripts = Array.from(document.querySelectorAll<HTMLScriptElement>('script[src]')).reverse()
  for (const script of scripts) {
    try {
      const src = new URL(script.src, window.location.href)
      const index = src.pathname.lastIndexOf(marker)
      if (index >= 0) return normalizeBasePath(src.pathname.slice(0, index))
    } catch {
      // Ignore malformed script URLs and continue looking for the Vite entry.
    }
  }
  return ''
}

export function appBasePath(viteBase: string, marker = '/assets/'): string {
  const native = nativeApiBase()
  if (native) return native
  const normalizedViteBase = normalizeBasePath(viteBase)
  if (normalizedViteBase) return normalizedViteBase
  return basePathFromScript(marker)
}

export function appURL(basePath: string, path: string): string {
  return `${basePath}${path.startsWith('/') ? path : `/${path}`}`
}

export function appAbsoluteURL(basePath: string, path: string): string {
  return `${window.location.origin}${appURL(basePath, path)}`
}
