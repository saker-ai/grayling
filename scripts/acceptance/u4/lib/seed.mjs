// U4 acceptance harness — data seeding through the real REST API.
// The dev-autologin middleware only signs GET/HEAD in, so the seeder first
// GETs /api/v1/admin/session to harvest the session cookie and then sends it
// with every mutating call. Everything is created via the same endpoints the
// console uses — no back doors, no fixtures injected into the store.
export const SEED = {
  ws: 'u4acc',
  env: 'prod',
  flagKey: 'u4-bool',
  flagDraftOnly: 'u4-json',
  segKey: 'u4-seg',
  metricKey: 'u4.metric',
  aiType: 'prompt',
  aiId: 'u4-prompt',
};

function flagContent(key, salt) {
  return {
    schemaVersion: 1,
    key,
    type: 'bool',
    disable: false,
    bucketing: { algorithm: 'grayling-fnv1a-10000-v1', unit: 'user_id', salt },
    fields: ['user_id'],
    variations: { control: false, candidate: true },
    defaultRule: { variation: 'control' },
    // 50/50 so the experiment bind precheck finds a percentage rule matching
    // the registered proportions.
    targeting: [{ name: 'split', percentage: { control: 50, candidate: 50 } }],
    experiment: null,
  };
}

function segmentContent(key) {
  return {
    schemaVersion: 1,
    key,
    constraints: [{ field: 'user_id', op: 'in', values: ['u4-alice', 'u4-bob'] }],
    segments: [],
  };
}

// apiClient is a minimal cookie-aware REST helper for the admin surface.
export async function apiClient(baseURL, log = console.log) {
  let cookie = '';
  const signin = await fetch(baseURL + '/api/v1/admin/session');
  if (!signin.ok) throw new Error(`session probe failed: HTTP ${signin.status}`);
  for (const line of signin.headers.getSetCookie?.() || []) {
    const [pair] = line.split(';');
    if (pair.startsWith('grayling_admin_session=')) cookie = pair;
  }
  if (!cookie) throw new Error('dev-autologin did not set grayling_admin_session — is GRAYLING_DEV_AUTOLOGIN=1?');

  const api = async (path, { method = 'GET', body } = {}) => {
    const res = await fetch(baseURL + path, {
      method,
      headers: {
        Cookie: cookie,
        // The admin API rejects cross-origin mutations (sameOriginRequest);
        // the browser would send this automatically, Node fetch does not.
        Origin: baseURL,
        ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}),
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
    const text = await res.text();
    let data = null;
    try {
      data = text ? JSON.parse(text) : null;
    } catch {
      data = { raw: text };
    }
    return { status: res.status, data };
  };

  // must succeeds or throws with the server error body; tolerate(409) allows
  // idempotent re-seeding against an attached (U4_BASE_URL) server.
  const must = async (path, opts, tolerate = []) => {
    const { status, data } = await api(path, opts);
    if (status >= 400 && !tolerate.includes(status)) {
      throw new Error(`${opts.method || 'GET'} ${path} → HTTP ${status}: ${JSON.stringify(data)}`);
    }
    return { status, data };
  };

  return { api, must, cookie };
}

// seedAll creates the workspace/env/flag/segment/metric/experiment/ai baseline
// and discovers the dynamic ids (approval, audit object) the routes need.
export async function seedAll(baseURL, log = console.log) {
  const { must, api } = await apiClient(baseURL, log);
  const scope = `/api/v1/admin/workspaces/${SEED.ws}/environments/${SEED.env}`;
  const wsBase = `/api/v1/admin/workspaces/${SEED.ws}`;

  await must('/api/v1/admin/workspaces', { method: 'POST', body: { slug: SEED.ws, name: 'U4 Acceptance' } }, [409]);
  await must(wsBase + '/environments', { method: 'POST', body: { slug: SEED.env, name: 'Production' } }, [409]);

  // u4-bool: published twice so revision history + rollback targets exist.
  const flagBase = `${scope}/flags/${SEED.flagKey}`;
  await must(flagBase + '/draft', {
    method: 'PUT',
    body: { content: flagContent(SEED.flagKey, 'u4-a'), risk: 'normal', baseRevision: 0, expectedDraftHash: '' },
  }, [409]);
  const flagView = await must(flagBase, {});
  let revision = flagView.data?.current?.revision || 0;
  if (revision === 0) {
    await must(flagBase + '/submit', { method: 'POST', body: { reason: 'u4 seed v1' } });
    await must(flagBase + '/approve', { method: 'POST', body: { note: 'u4 seed v1' } });
    await must(flagBase + '/publish', { method: 'POST', body: { note: 'u4 seed publish' } });
    revision = 1;
  }
  if (revision < 2) {
    const v2 = flagContent(SEED.flagKey, 'u4-b');
    v2.description = 'U4 acceptance flag, revision 2 (rollback target source)';
    // Publishing leaves the approved draft in place, so the next save must
    // chain on the fresh draftHash rather than the empty marker.
    const fresh = await must(flagBase, {});
    await must(flagBase + '/draft', {
      method: 'PUT',
      body: {
        content: v2,
        risk: 'normal',
        baseRevision: revision,
        expectedDraftHash: fresh.data?.draftHash || '',
      },
    });
    await must(flagBase + '/submit', { method: 'POST', body: { reason: 'u4 seed v2' } });
    await must(flagBase + '/approve', { method: 'POST', body: { note: 'u4 seed v2' } });
    await must(flagBase + '/publish', { method: 'POST', body: { note: 'u4 seed publish' } });
    revision = 2;
  }
  log(`[seed] flag ${SEED.flagKey} at revision ${revision}`);

  // u4-json: draft left in submitted state so approvals has a pending entry.
  const jsonFlag = flagContent(SEED.flagDraftOnly, 'u4-json');
  jsonFlag.type = 'json';
  jsonFlag.variations = { control: { mode: 'off' }, candidate: { mode: 'on' } };
  const jsonBase = `${scope}/flags/${SEED.flagDraftOnly}`;
  const jsonView = await must(jsonBase, {});
  if (!jsonView.data?.draft) {
    await must(jsonBase + '/draft', {
      method: 'PUT',
      body: { content: jsonFlag, risk: 'normal', baseRevision: 0, expectedDraftHash: '' },
    });
    await must(jsonBase + '/submit', { method: 'POST', body: { reason: 'u4 seed pending approval' } });
  }
  log(`[seed] flag ${SEED.flagDraftOnly} submitted (pending approval)`);

  // u4-seg: published segment (no single-segment GET exists; use the list).
  const segBase = `${scope}/segments/${SEED.segKey}`;
  const segList = await must(`${scope}/segments`, {});
  const segExisting = (segList.data || []).find(s => s.key === SEED.segKey);
  if (!segExisting?.current) {
    await must(segBase + '/draft', { method: 'PUT', body: { content: segmentContent(SEED.segKey), risk: 'normal' } });
    await must(segBase + '/submit', { method: 'POST', body: { reason: 'u4 seed' } });
    await must(segBase + '/approve', { method: 'POST', body: { note: 'u4 seed' } });
    await must(segBase + '/publish', { method: 'POST', body: { note: 'u4 seed publish' } });
  }
  log(`[seed] segment ${SEED.segKey} published`);

  // u4.metric: draft metric definition v1, submitted so the approvals list
  // holds a real pending entry (flag drafts use a separate review workflow
  // and never surface here).
  const metrics = await must(wsBase + '/metrics?limit=200', {});
  const metric = (metrics.data?.items || []).find(m => m.key === SEED.metricKey);
  if (!metric) {
    await must(wsBase + '/metrics', {
      method: 'POST',
      body: {
        key: SEED.metricKey,
        displayName: 'U4 Metric',
        owner: 'u4-acceptance',
        source: 'collect',
        eventKeys: ['u4_complete'],
        dedupUnit: 'user',
        attributionWindow: 86_400_000_000_000,
        spec: { key: 'u4_complete', type: 'proportion', direction: 'increase' },
      },
    });
    await must(`${wsBase}/metrics/${SEED.metricKey}/versions/1/submit`, { method: 'POST', body: { reason: 'u4 seed' } });
  }
  log(`[seed] metric ${SEED.metricKey} present`);

  // Experiment bound to u4-bool; pushed as far as the state machine allows.
  const experiments = await must(scope + '/experiments', {});
  let exp = (experiments.data || []).find(e => e.flagKey === SEED.flagKey);
  if (!exp) {
    const created = await must(scope + '/experiments', {
      method: 'POST',
      body: {
        flagKey: SEED.flagKey,
        unit: 'user_id',
        variants: ['control', 'candidate'],
        proportions: { control: 50, candidate: 50 },
        primaryMetric: { key: SEED.metricKey, type: 'proportion', direction: 'increase' },
        alpha: 0.05,
        analysisWindow: 86_400_000_000_000,
      },
    });
    exp = created.data;
  }
  const expBase = `/api/v1/admin/experiments/${encodeURIComponent(exp.id)}`;
  const state = exp.state;
  if (state === 'draft') {
    await must(expBase + '/review', { method: 'POST', body: { note: 'u4 seed review' } });
  }
  const afterReview = await must(expBase, {});
  if (afterReview.data?.state === 'reviewed') {
    await must(expBase + '/bind', { method: 'POST', body: {} });
  }
  const afterBind = await must(expBase, {});
  if (afterBind.data?.state === 'bound') {
    const tr = await api(expBase + '/transition', { method: 'POST', body: { to: 'running', reason: 'u4 seed start' } });
    if (tr.status >= 400) log(`[seed] experiment transition to running declined: HTTP ${tr.status} (non-fatal)`);
  }
  const finalExp = await must(expBase, {});
  log(`[seed] experiment ${exp.id} state=${finalExp.data?.state}`);

  // AI resource for the ai/{type}/{logicalId} route.
  await must(scope + '/ai/resources', {
    method: 'POST',
    body: { type: SEED.aiType, logicalId: SEED.aiId, name: 'U4 Prompt' },
  }, [409]);
  log(`[seed] ai resource ${SEED.aiType}/${SEED.aiId} present`);

  // Dynamic route parameters discovered from the seeded data.
  const approvals = await must(wsBase + '/approvals', {});
  const approval = (approvals.data || []).find(a => String(a.object || '').includes(SEED.flagDraftOnly)) || (approvals.data || [])[0];
  const audit = await must(wsBase + '/audit?limit=20', {});
  const auditObject = (audit.data || [])[0]?.object || '';

  const summary = {
    ws: SEED.ws,
    env: SEED.env,
    flagKey: SEED.flagKey,
    flagRevision: revision,
    flagDraftOnly: SEED.flagDraftOnly,
    segKey: SEED.segKey,
    metricKey: SEED.metricKey,
    metricVersion: 1,
    expId: exp.id,
    expState: finalExp.data?.state,
    approvalId: approval?.id || '',
    aiType: SEED.aiType,
    aiId: SEED.aiId,
    auditObject,
  };
  log(`[seed] summary ${JSON.stringify(summary)}`);
  return summary;
}

// bulkFlags creates n draft flags (u4-bulk-0001…) for the 1000-row list
// scenario. Drafts are enough: the flags list renders draft content. The
// default workspace quota is 500 flags, so it is raised first (and left
// raised — every U4 run uses a throwaway store).
export async function bulkFlags(baseURL, n, log = console.log) {
  const { must } = await apiClient(baseURL, log);
  const wsBase = `/api/v1/admin/workspaces/${SEED.ws}`;
  const scope = `${wsBase}/environments/${SEED.env}`;
  const quotas = await must(`${wsBase}/quotas`, {});
  const current = quotas.data || quotas;
  if ((current.maxFlags || 0) < n + 10) {
    await must(`${wsBase}/quotas`, { method: 'PUT', body: { ...current, maxFlags: n + 100 } });
    log(`[seed] quota maxFlags ${current.maxFlags} → ${n + 100}`);
  }
  const CONCURRENCY = 20;
  let done = 0;
  const keys = Array.from({ length: n }, (_, i) => `u4-bulk-${String(i + 1).padStart(4, '0')}`);
  await Promise.all(
    Array.from({ length: CONCURRENCY }, async (_, lane) => {
      for (let i = lane; i < keys.length; i += CONCURRENCY) {
        await must(`${scope}/flags/${keys[i]}/draft`, {
          method: 'PUT',
          body: { content: flagContent(keys[i], 'u4-bulk'), risk: 'normal', baseRevision: 0, expectedDraftHash: '' },
        }, [409]);
        done++;
      }
    }),
  );
  log(`[seed] bulk flags: ${done}/${n}`);
  return keys;
}
