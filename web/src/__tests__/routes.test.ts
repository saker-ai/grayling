import { describe, expect, it } from 'vitest';
import { buildConsoleHash, parseConsoleParams } from '../routes';
import type { ConsoleParams } from '../routes';

// Every branch here has a twin in the legacy consoleHash/restoreConsoleHash
// (web/legacy/app.js); the hash format must stay byte-compatible.

function base(partial: Partial<ConsoleParams>): ConsoleParams {
  return {
    ws: 'w',
    env: 'e',
    view: 'overview',
    flagKey: null,
    segKey: null,
    expId: null,
    expTab: 'summary',
    expCreate: false,
    metricRef: null,
    approvalID: null,
    aiRef: null,
    auditObject: '',
    ...partial,
  };
}

describe('parseConsoleParams', () => {
  it('parses a bare view hash', () => {
    expect(parseConsoleParams('#/console/ws1/env1/overview')).toEqual(
      base({ ws: 'ws1', env: 'env1', view: 'overview' }),
    );
  });

  it('decodes the ~ empty-scope markers', () => {
    const p = parseConsoleParams('#/console/~/~/flags');
    expect(p).not.toBeNull();
    expect(p?.ws).toBe('');
    expect(p?.env).toBe('');
    expect(p?.view).toBe('flags');
  });

  it('parses flag and segment detail keys', () => {
    expect(parseConsoleParams('#/console/w/e/flags/my-flag')?.flagKey).toBe('my-flag');
    expect(parseConsoleParams('#/console/w/e/segments/seg-1')?.segKey).toBe('seg-1');
  });

  it('decodes percent-encoded segment values', () => {
    expect(parseConsoleParams('#/console/w/e/flags/a%2Fb')?.flagKey).toBe('a/b');
  });

  it('parses experiment detail with default and explicit tabs', () => {
    const def = parseConsoleParams('#/console/w/e/experiments/exp-9');
    expect(def?.expId).toBe('exp-9');
    expect(def?.expTab).toBe('summary');
    expect(parseConsoleParams('#/console/w/e/experiments/exp-9/rollout')?.expTab).toBe('rollout');
  });

  it('normalizes the legacy diagnostics tab alias to health', () => {
    expect(parseConsoleParams('#/console/w/e/experiments/exp-9/diagnostics')?.expTab).toBe('health');
  });

  it('parses the ~new create state', () => {
    const p = parseConsoleParams('#/console/w/e/experiments/~new');
    expect(p?.expCreate).toBe(true);
    expect(p?.expId).toBeNull();
  });

  it('rejects a tab on the ~new create state', () => {
    expect(parseConsoleParams('#/console/w/e/experiments/~new/health')).toBeNull();
  });

  it('rejects unknown experiment tabs and overlong experiment hashes', () => {
    expect(parseConsoleParams('#/console/w/e/experiments/exp-9/bogus')).toBeNull();
    expect(parseConsoleParams('#/console/w/e/experiments/exp-9/health/extra')).toBeNull();
  });

  it('parses metric references only with a positive integer version', () => {
    expect(parseConsoleParams('#/console/w/e/metrics/signup.rate/3')?.metricRef).toEqual({
      key: 'signup.rate',
      version: 3,
    });
    const bad = parseConsoleParams('#/console/w/e/metrics/signup.rate/0');
    expect(bad).not.toBeNull();
    expect(bad?.metricRef).toBeNull();
    expect(parseConsoleParams('#/console/w/e/metrics/signup.rate/abc')?.metricRef).toBeNull();
  });

  it('parses approvals and audit deep links', () => {
    expect(parseConsoleParams('#/console/w/e/approvals/ap-1')?.approvalID).toBe('ap-1');
    // The audit object is one path segment; embedded slashes stay encoded.
    expect(parseConsoleParams('#/console/w/e/audit/flag%2Fmy.flag')?.auditObject).toBe('flag/my.flag');
    expect(parseConsoleParams('#/console/w/e/audit/flag/extra')).toBeNull();
  });

  it('parses ai references only at exactly six segments', () => {
    expect(parseConsoleParams('#/console/w/e/ai')?.aiRef).toBeNull();
    expect(parseConsoleParams('#/console/w/e/ai/model/gpt-x')?.aiRef).toEqual({
      type: 'model',
      logicalId: 'gpt-x',
    });
    expect(parseConsoleParams('#/console/w/e/ai/model')).toBeNull();
  });

  it('rejects unknown views, non-console hashes and short hashes', () => {
    expect(parseConsoleParams('#/console/w/e/nope')).toBeNull();
    expect(parseConsoleParams('#/other/w/e/flags')).toBeNull();
    expect(parseConsoleParams('#/console/w/e')).toBeNull();
    expect(parseConsoleParams('')).toBeNull();
  });

  it('rejects malformed percent escapes', () => {
    expect(parseConsoleParams('#/console/w/e/flags/%zz')).toBeNull();
  });

  it('accepts trailing segments on detail views like the legacy console did', () => {
    const p = parseConsoleParams('#/console/w/e/flags/key/extra');
    expect(p?.flagKey).toBe('key');
  });
});

describe('buildConsoleHash', () => {
  it('builds the empty-scope markers', () => {
    expect(buildConsoleHash(base({ ws: '', env: '' }))).toBe('#/console/~/~/overview');
  });

  it('round-trips every deep-link shape', () => {
    const shapes: ConsoleParams[] = [
      base({ ws: 'ws1', env: 'env1', view: 'overview' }),
      base({ view: 'flags', flagKey: 'my-flag' }),
      base({ view: 'segments', segKey: 'seg-1' }),
      base({ view: 'experiments', expId: 'exp-9' }),
      base({ view: 'experiments', expId: 'exp-9', expTab: 'health' }),
      base({ view: 'experiments', expCreate: true }),
      base({ view: 'metrics', metricRef: { key: 'signup.rate', version: 3 } }),
      base({ view: 'approvals', approvalID: 'ap-1' }),
      base({ view: 'ai', aiRef: { type: 'model', logicalId: 'gpt-x' } }),
      base({ view: 'audit', auditObject: 'flag/my.flag' }),
      base({ view: 'debug' }),
      base({ view: 'tokens' }),
      base({ view: 'quotas' }),
    ];
    for (const shape of shapes) {
      expect(parseConsoleParams(buildConsoleHash(shape))).toEqual(shape);
    }
  });

  it('drops the default experiment tab from the hash', () => {
    expect(buildConsoleHash(base({ view: 'experiments', expId: 'e1', expTab: 'summary' }))).toBe(
      '#/console/w/e/experiments/e1',
    );
  });

  it('percent-encodes reserved characters in values', () => {
    expect(buildConsoleHash(base({ view: 'flags', flagKey: 'a/b c' }))).toBe(
      '#/console/w/e/flags/a%2Fb%20c',
    );
  });
});
