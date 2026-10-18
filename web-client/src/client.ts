// Grayling console API client — a line-for-line semantic port of the fetch
// layer in web/legacy/app.js (ApiError, baseUrl, requestBody, api, errMsg,
// downloadReport and doConnect). Two deliberate differences:
//   - credentials live on a GraylingClient instance instead of module globals
//     (cookieSession / memoryToken), and the 401 side effect is the injected
//     onUnauthorized callback instead of a direct DOM call;
//   - thrown error messages are English literals; the UI localizes them via
//     errMsg(e, t), exactly like the legacy code path did.
// The client never retries: every api()/downloadReport() call issues exactly
// one fetch.

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
  }
}

// baseUrl is the operator-configured API origin (empty = same origin), with
// trailing slashes stripped.
export function baseUrl(): string {
  return (localStorage.getItem('grayling_url') || '').replace(/\/+$/, '');
}

export function sameOriginConsole(): boolean {
  return !baseUrl() || baseUrl() === location.origin;
}

export interface RequestOptions {
  method?: string;
  headers?: Record<string, string>;
  credentials?: RequestCredentials;
  body?: string;
  signal?: AbortSignal | null;
}

export interface DeadlineResponse<T> {
  response: Response;
  body: T;
}

// requestWithDeadline keeps the deadline active while reading the response
// body as well as the headers. An external signal is chained in; its cancel
// maps to code 'cancelled', the internal timer to 'timeout' and anything else
// to 'network'.
export async function requestWithDeadline<T>(
  url: string,
  options: RequestOptions,
  readBody: (response: Response) => Promise<T>,
  timeoutMs = 30000,
): Promise<DeadlineResponse<T>> {
  const controller = new AbortController();
  const external = options.signal ?? undefined;
  const cancel = () => controller.abort();
  if (external?.aborted) cancel();
  else external?.addEventListener('abort', cancel, { once: true });
  let timedOut = false;
  const timer = setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, timeoutMs);
  try {
    const response = await fetch(url, { ...options, signal: controller.signal });
    return { response, body: await readBody(response) };
  } catch (error) {
    if (timedOut) throw new ApiError(0, 'timeout', 'request timed out');
    if (controller.signal.aborted) throw new ApiError(0, 'cancelled', 'request cancelled');
    throw new ApiError(0, 'network',
      `cannot reach ${baseUrl() || 'same origin'}: ${(error as Error).message}`);
  } finally {
    clearTimeout(timer);
    external?.removeEventListener('abort', cancel);
  }
}

export interface GraylingClientHooks {
  // onUnauthorized fires after a 401 cleared the stored credentials; the UI
  // layer uses it to drop back to the login screen.
  onUnauthorized?: () => void;
}

export interface ApiCallOptions {
  method?: string;
  body?: unknown;
  signal?: AbortSignal | null;
}

interface ErrorPayload {
  error?: { code?: string; message?: string };
}

export class GraylingClient {
  // cookieSession: same-origin HttpOnly cookie session (credentials:
  // same-origin). memoryToken: ephemeral bearer credential for cross-origin
  // API connections — never enters browser storage and is lost on reload.
  cookieSession = false;
  memoryToken = '';
  onUnauthorized: (() => void) | undefined;

  constructor(hooks: GraylingClientHooks = {}) {
    this.onUnauthorized = hooks.onUnauthorized;
  }

  private credentials(): RequestCredentials {
    return this.cookieSession ? 'same-origin' : 'omit';
  }

  private bearerHeader(headers: Record<string, string>): Record<string, string> {
    if (!this.cookieSession && this.memoryToken) headers.Authorization = 'Bearer ' + this.memoryToken;
    return headers;
  }

  private clearCredentials(): void {
    this.cookieSession = false;
    this.memoryToken = '';
    this.onUnauthorized?.();
  }

  // resetCredentials drops the auth mode without firing onUnauthorized; used
  // by the session-restore path, where a failed probe is not a rejection.
  resetCredentials(): void {
    this.cookieSession = false;
    this.memoryToken = '';
  }

  async api<T = unknown>(path: string, opts: ApiCallOptions = {}): Promise<T> {
    const { method = 'GET', body } = opts;
    const headers: Record<string, string> = {};
    this.bearerHeader(headers);
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    const { response: res, body: text } = await requestWithDeadline(baseUrl() + path, {
      method,
      headers,
      credentials: this.credentials(),
      body: body !== undefined ? JSON.stringify(body) : undefined,
      signal: opts.signal ?? undefined,
    }, response => response.text());
    let data: (Record<string, unknown> & ErrorPayload) | null = null;
    if (text) {
      try {
        data = JSON.parse(text) as Record<string, unknown> & ErrorPayload;
      } catch {
        // non-JSON body
      }
    }
    if (!res.ok) {
      const err: { code?: string; message?: string } = (data && data.error) || {};
      const e = new ApiError(res.status, err.code || 'http_' + res.status, err.message || res.statusText);
      if (res.status === 401) this.clearCredentials();
      throw e;
    }
    return data as T;
  }

  async downloadReport(path: string, format: string): Promise<void> {
    const headers: Record<string, string> = {};
    this.bearerHeader(headers);
    const { response: res, body: reportBody } = await requestWithDeadline(
      baseUrl() + path + '?format=' + format,
      { headers, credentials: this.credentials() },
      response => (response.ok ? response.blob() : response.text()) as Promise<Blob | string>,
    );
    if (!res.ok) {
      let body: ErrorPayload = {};
      try {
        body = JSON.parse(reportBody as string) as ErrorPayload;
      } catch {
        // non-JSON error body
      }
      const err: { code?: string; message?: string } = body.error || {};
      if (res.status === 401) this.clearCredentials();
      throw new ApiError(res.status, err.code || 'http_' + res.status, err.message || res.statusText);
    }
    const url = URL.createObjectURL(reportBody as Blob);
    const link = document.createElement('a');
    link.href = url;
    link.download = 'grayling-report.' + format;
    document.body.append(link);
    link.click();
    link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 30000);
  }

  // login exchanges the admin password/token for a session. Same origin: a
  // form-urlencoded POST sets the HttpOnly cookie and the token never touches
  // storage. Cross origin: the token becomes an in-memory bearer credential.
  // Plain http to anything but localhost is refused before any request.
  async login(token: string): Promise<void> {
    const target = new URL(baseUrl() || location.origin);
    if (target.protocol === 'http:' && !['localhost', '127.0.0.1', '[::1]'].includes(target.hostname)) {
      throw new ApiError(0, 'tls_required', 'https is required for non-localhost consoles');
    }
    if (sameOriginConsole()) {
      const { response: res, body: loginBody } = await requestWithDeadline(
        baseUrl() + '/api/v1/admin/session',
        {
          method: 'POST',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
          body: 'token=' + encodeURIComponent(token),
        },
        response => response.text(),
      );
      if (!res.ok) {
        let error: { code?: string; message?: string } = {};
        try {
          error = (JSON.parse(loginBody) as { error?: typeof error }).error || {};
        } catch {
          // server may return plain text
        }
        throw new ApiError(res.status, error.code || 'login_failed', error.message || 'login rejected');
      }
      this.cookieSession = true;
      this.memoryToken = '';
    } else {
      this.memoryToken = token;
      this.cookieSession = false;
    }
  }

  async logout(): Promise<void> {
    if (this.cookieSession) {
      try {
        await this.api('/api/v1/admin/session', { method: 'DELETE' });
      } catch (e) {
        if ((e as ApiError).status !== 401) throw e;
      }
    }
    this.cookieSession = false;
    this.memoryToken = '';
  }

  async fetchSession(signal?: AbortSignal): Promise<{ name: string; principalId: string }> {
    const session = await this.api<{ name?: string; principalId?: string }>('/api/v1/admin/session', { signal });
    return { name: session?.name || '', principalId: session?.principalId || '' };
  }
}

export type Translate = (key: string, params?: Record<string, unknown>) => string;

// errMsg maps an ApiError to the operator-facing message, branch for branch
// the legacy mapping in web/legacy/app.js.
export function errMsg(e: { status?: number; code?: string; message?: string }, t: Translate): string {
  if (e.code === 'flag_review_changed') return t('flags.reviewChanged');
  if (e.code === 'report_changed') return t('ro.reportChanged');
  if (e.code === 'report_unavailable') return t('ro.reportUnavailable');
  // Break-glass post-reviews still require an independent reviewer, so this
  // code outlives the general self-approval removal and outranks the plain 403
  // text (the server returns 403 for it, like any other permission denial).
  if (e.code === 'self_approval_denied') return t('toast.selfApproval');
  if (e.status === 403) return t('err.denied403');
  if (e.status === 409) return t('err.conflict409');
  if (e.status === 429) return t('err.limited429');
  if (e.code === 'timeout') return t('err.requestTimeout');
  if (e.code === 'cancelled') return t('err.requestCancelled');
  if (e.code === 'network') return t('err.networkRetry');
  if (typeof e.status === 'number' && e.status >= 500 && e.status !== 501) {
    return t('err.server5xx', { status: e.status });
  }
  let m = '[' + (e.code || 'error') + '] ' + (e.message || e);
  if (e.status === 423) m += t('err.revoked423');
  if (e.status === 501) m += t('err.notEnabled501');
  return m;
}
