import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, GraylingClient, errMsg, requestWithDeadline } from '../client';

type FetchImpl = (url: string, opts: RequestInit & { signal: AbortSignal }) => Promise<unknown>;

function jsonResponse(status: number, body: unknown, statusText = ''): Promise<unknown> {
  const text = typeof body === 'string' ? body : JSON.stringify(body);
  return Promise.resolve({
    ok: status >= 200 && status < 300,
    status,
    statusText,
    text: () => Promise.resolve(text),
    blob: () => Promise.resolve(new Blob([text])),
  });
}

function stubFetch(impl: FetchImpl) {
  const mock = vi.fn(impl);
  vi.stubGlobal('fetch', mock);
  return mock;
}

describe('requestWithDeadline', () => {
  beforeEach(() => {
    localStorage.clear();
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it('times out while the body stream is still pending', async () => {
    vi.useFakeTimers();
    stubFetch((_url, opts) =>
      Promise.resolve({
        ok: true,
        status: 200,
        text: () =>
          new Promise<string>((_resolve, reject) => {
            opts.signal.addEventListener('abort', () =>
              reject(new DOMException('The operation was aborted.', 'AbortError')));
          }),
      }),
    );
    const pending = requestWithDeadline('/api/v1/x', {}, r => (r as { text(): Promise<string> }).text());
    const assertion = expect(pending).rejects.toMatchObject({ code: 'timeout', status: 0 });
    await vi.advanceTimersByTimeAsync(30000);
    await assertion;
  });

  it('maps an external AbortController cancel to code cancelled', async () => {
    stubFetch((_url, opts) =>
      new Promise((_resolve, reject) => {
        opts.signal.addEventListener('abort', () =>
          reject(new DOMException('The operation was aborted.', 'AbortError')));
      }),
    );
    const external = new AbortController();
    const pending = requestWithDeadline('/api/v1/x', { signal: external.signal }, r =>
      (r as { text(): Promise<string> }).text(),
    );
    const assertion = expect(pending).rejects.toMatchObject({ code: 'cancelled' });
    external.abort();
    await assertion;
  });

  it('maps a fetch rejection to code network', async () => {
    stubFetch(() => Promise.reject(new TypeError('fetch failed')));
    await expect(requestWithDeadline('/api/v1/x', {}, r => (r as { text(): Promise<string> }).text()))
      .rejects.toMatchObject({ code: 'network', status: 0 });
  });
});

describe('GraylingClient', () => {
  beforeEach(() => {
    localStorage.clear();
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('issues exactly one fetch per call and never retries on failure', async () => {
    const fetchMock = stubFetch(() => Promise.reject(new TypeError('fetch failed')));
    const client = new GraylingClient();
    await expect(client.api('/api/v1/admin/session')).rejects.toMatchObject({ code: 'network' });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('clears credentials and fires onUnauthorized on 401', async () => {
    stubFetch(() => jsonResponse(401, { error: { code: 'auth_required', message: 'nope' } }, 'Unauthorized'));
    const onUnauthorized = vi.fn();
    const client = new GraylingClient({ onUnauthorized });
    client.cookieSession = true;
    await expect(client.api('/api/v1/admin/session')).rejects.toMatchObject({ status: 401, code: 'auth_required' });
    expect(client.cookieSession).toBe(false);
    expect(client.memoryToken).toBe('');
    expect(onUnauthorized).toHaveBeenCalledTimes(1);
  });

  it('sends the memory bearer token cross-origin and omits credentials', async () => {
    localStorage.setItem('grayling_url', 'https://grayling.example.com');
    const fetchMock = stubFetch(() => jsonResponse(200, { name: 'op', principalId: 'p1' }));
    const client = new GraylingClient();
    client.memoryToken = 'tok123';
    const session = await client.fetchSession();
    expect(session).toEqual({ name: 'op', principalId: 'p1' });
    const [, opts] = fetchMock.mock.calls[0] as unknown as [string, RequestInit & { headers: Record<string, string> }];
    expect(opts.headers.Authorization).toBe('Bearer tok123');
    expect(opts.credentials).toBe('omit');
  });

  it('refuses plain http logins to non-localhost hosts before any request', async () => {
    localStorage.setItem('grayling_url', 'http://grayling.example.com');
    const fetchMock = stubFetch(() => jsonResponse(200, {}));
    const client = new GraylingClient();
    await expect(client.login('tok')).rejects.toMatchObject({ code: 'tls_required', status: 0 });
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('posts the token form-urlencoded on same-origin login and enters cookie mode', async () => {
    const fetchMock = stubFetch(() => jsonResponse(200, { ok: true }));
    const client = new GraylingClient();
    await client.login('sek ret');
    expect(client.cookieSession).toBe(true);
    expect(client.memoryToken).toBe('');
    const [url, opts] = fetchMock.mock.calls[0] as unknown as [string, RequestInit & { headers: Record<string, string> }];
    expect(url).toBe('/api/v1/admin/session');
    expect(opts.method).toBe('POST');
    expect(opts.headers['Content-Type']).toBe('application/x-www-form-urlencoded');
    expect(opts.body).toBe('token=sek%20ret');
    expect(opts.credentials).toBe('same-origin');
  });
});

describe('errMsg', () => {
  // A transparent translate double: returns the key and any params verbatim.
  const t = (key: string, params?: Record<string, unknown>) =>
    params ? key + ':' + JSON.stringify(params) : key;

  it('maps every branch of the legacy error table', () => {
    const cases: Array<[{ status?: number; code?: string; message?: string }, string]> = [
      [{ code: 'flag_review_changed' }, 'flags.reviewChanged'],
      [{ code: 'report_changed' }, 'ro.reportChanged'],
      [{ code: 'report_unavailable' }, 'ro.reportUnavailable'],
      [{ code: 'self_approval_denied', status: 403 }, 'toast.selfApproval'],
      [{ status: 403 }, 'err.denied403'],
      [{ status: 409 }, 'err.conflict409'],
      [{ status: 429 }, 'err.limited429'],
      [{ code: 'timeout' }, 'err.requestTimeout'],
      [{ code: 'cancelled' }, 'err.requestCancelled'],
      [{ code: 'network' }, 'err.networkRetry'],
      [{ status: 500, code: 'boom' }, 'err.server5xx:{"status":500}'],
      [{ status: 502, code: 'boom' }, 'err.server5xx:{"status":502}'],
      [{ status: 423, code: 'token_revoked', message: 'revoked' }, '[token_revoked] revoked' + 'err.revoked423'],
      [{ status: 501, code: 'ai_disabled', message: 'off' }, '[ai_disabled] off' + 'err.notEnabled501'],
      [{ status: 400, code: 'bad_request', message: 'bad' }, '[bad_request] bad'],
    ];
    for (const [err, want] of cases) {
      expect(errMsg(err, t)).toBe(want);
    }
  });

  it('keeps ApiError instances intact through the mapping', () => {
    const e = new ApiError(503, 'http_503', 'Service Unavailable');
    expect(errMsg(e, t)).toBe('err.server5xx:{"status":503}');
  });
});
