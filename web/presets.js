'use strict';

// ---------------------------------------------------------------------------
// Grayling admin console — built-in targeting presets. Pure data plus two
// lookup functions: no DOM access, no build step. Loaded right after i18n.js
// (plain <script> order), so app.js shares this script scope.
//
// Every preset is a complete, save-ready document body for the SAME shape the
// API validates (pkg/model): build(key) returns the flag or segment content
// and all display strings live in the i18n catalogs behind nameKey/descKey/
// hintKey, so this file carries no literals in any UI language (see the CJK
// static scan in i18n_contract.mjs). Two contracts keep the presets honest:
//   * web/preset_contract.mjs lints every document with the console's own
//     lintFlag()/lintSegment(), asserts each i18n id resolves in en AND zh,
//     and checks the wiring in viewFlagDetail/viewSegmentDetail;
//   * web/render_contract_test.go re-validates the printed documents with the
//     real server validators (model.ValidateFlag/model.ValidateSegment), so a
//     preset can never turn into a 400 on save.
// ---------------------------------------------------------------------------

// presetFlag builds the shared flag skeleton: schema v1, bool, an explicit
// control/candidate fallback and the pinned hash configuration. Presets extend
// a fresh copy of it, never this template object.
function presetFlag(key) {
  return {
    schemaVersion: 1, key, type: 'bool', disable: false,
    bucketing: { algorithm: 'grayling-fnv1a-10000-v1', unit: 'user_id', salt: 's1' },
    fields: ['user_id'],
    variations: { control: false, candidate: true },
    defaultRule: { variation: 'control' },
    targeting: [],
    experiment: null,
  };
}

// isoSeconds renders an epoch-millisecond instant as RFC3339 at second
// precision (no milliseconds): the same shape the docs use, and still parsed by
// the server's time.Parse(time.RFC3339) operand reader.
function isoSeconds(ms) {
  return new Date(ms).toISOString().replace(/\.\d{3}Z$/, 'Z');
}

// presetSegment builds the shared segment skeleton. The server registers every
// field a segment references itself (ValidateSegment), so a segment preset only
// declares the constraints it needs; a flag that references the segment must
// still list those fields in its own allowlist.
function presetSegment(key) {
  return { schemaVersion: 1, key, constraints: [], segments: [] };
}

// The two time-window presets build their timestamps from Date.now() at APPLY
// time, never from a written-down date: a literal window rots, and a window
// that has already passed silently pins every unit to the control value, which
// looks like a broken preset. The window opens an hour after the pick and runs
// for seven days; operators adjust it to their own release schedule.
const WINDOW_START_MS = 3600e3;
const WINDOW_END_MS = 7 * 86400e3;

// Flag presets. Rules are matched in list order and each rule sets exactly one
// of variation/percentage; a percentage split must sum to 100.
const FLAG_PRESETS = [
  {
    id: 'percent-rollout',
    nameKey: 'preset.flag.percentRollout.name',
    descKey: 'preset.flag.percentRollout.desc',
    hintKey: 'preset.flag.percentRollout.hint',
    // One canary rule instead of a fixed variation: 5% of units land in the
    // candidate, every other unit falls through to the control default.
    build(key) {
      const content = presetFlag(key);
      content.targeting = [{ name: 'canary', percentage: { control: 95, candidate: 5 } }];
      return content;
    },
  },
  {
    id: 'ip-cidr',
    nameKey: 'preset.flag.ipCidr.name',
    descKey: 'preset.flag.ipCidr.desc',
    hintKey: 'preset.flag.ipCidr.hint',
    // client_ip is opt-in: it must be listed in fields before it can drive a
    // constraint, and the value must be a valid CIDR string.
    build(key) {
      const content = presetFlag(key);
      content.fields = ['user_id', 'client_ip'];
      content.targeting = [{
        name: 'internal-net', variation: 'candidate',
        constraints: [{ field: 'client_ip', op: 'in_cidr', value: '10.0.0.0/8' }],
      }];
      return content;
    },
  },
  {
    id: 'allowlist',
    nameKey: 'preset.flag.allowlist.name',
    descKey: 'preset.flag.allowlist.desc',
    hintKey: 'preset.flag.allowlist.hint',
    // Set operators carry `values` (a scalar list), never `value`.
    build(key) {
      const content = presetFlag(key);
      content.targeting = [{
        name: 'allowlist', variation: 'candidate',
        constraints: [{ field: 'user_id', op: 'in', values: ['alice', 'bob'] }],
      }];
      return content;
    },
  },
  {
    id: 'app-version',
    nameKey: 'preset.flag.appVersion.name',
    descKey: 'preset.flag.appVersion.desc',
    hintKey: 'preset.flag.appVersion.hint',
    build(key) {
      const content = presetFlag(key);
      content.fields = ['user_id', 'app_version'];
      content.targeting = [{
        name: 'new-clients', variation: 'candidate',
        constraints: [{ field: 'app_version', op: 'semver_gte', value: '2.0.0' }],
      }];
      return content;
    },
  },
  {
    id: 'time-window',
    nameKey: 'preset.flag.timeWindow.name',
    descKey: 'preset.flag.timeWindow.desc',
    hintKey: 'preset.flag.timeWindow.hint',
    // Multiple constraints inside ONE rule are ANDed; separate rules are the
    // only way to express OR. The two timestamps are relative to the moment the
    // preset is applied, so the window always lies ahead.
    build(key) {
      const content = presetFlag(key);
      const now = Date.now();
      content.fields = ['user_id', 'request_time'];
      content.targeting = [{
        name: 'window', variation: 'candidate',
        constraints: [
          { field: 'request_time', op: 'time_after', value: isoSeconds(now + WINDOW_START_MS) },
          { field: 'request_time', op: 'time_before', value: isoSeconds(now + WINDOW_END_MS) },
        ],
      }];
      return content;
    },
  },
  {
    id: 'kill-switch',
    nameKey: 'preset.flag.killSwitch.name',
    descKey: 'preset.flag.killSwitch.desc',
    hintKey: 'preset.flag.killSwitch.hint',
    // No targeting at all: the default rule is the whole behaviour, so one
    // publish of a changed defaultRule flips the entire fleet.
    build(key) {
      const content = presetFlag(key);
      content.variations = { on: false, off: true };
      content.defaultRule = { variation: 'off' };
      return content;
    },
  },
];

// Segment presets. A segment matches everything when it has neither
// constraints nor nested segments, so every preset declares at least one.
const SEGMENT_PRESETS = [
  {
    id: 'internal-net',
    nameKey: 'preset.seg.internalNet.name',
    descKey: 'preset.seg.internalNet.desc',
    build(key) {
      const content = presetSegment(key);
      content.constraints = [{ field: 'client_ip', op: 'in_cidr', value: '10.0.0.0/8' }];
      return content;
    },
  },
  {
    id: 'user-list',
    nameKey: 'preset.seg.userList.name',
    descKey: 'preset.seg.userList.desc',
    build(key) {
      const content = presetSegment(key);
      content.constraints = [{ field: 'user_id', op: 'in', values: ['alice', 'bob'] }];
      return content;
    },
  },
  {
    id: 'app-version',
    nameKey: 'preset.seg.appVersion.name',
    descKey: 'preset.seg.appVersion.desc',
    build(key) {
      const content = presetSegment(key);
      content.constraints = [{ field: 'app_version', op: 'semver_gte', value: '2.0.0' }];
      return content;
    },
  },
  {
    id: 'active-window',
    nameKey: 'preset.seg.activeWindow.name',
    descKey: 'preset.seg.activeWindow.desc',
    // Same relative window as the flag preset: computed at apply time, so a
    // segment preset never starts out already expired.
    build(key) {
      const content = presetSegment(key);
      const now = Date.now();
      content.constraints = [
        { field: 'request_time', op: 'time_after', value: isoSeconds(now + WINDOW_START_MS) },
        { field: 'request_time', op: 'time_before', value: isoSeconds(now + WINDOW_END_MS) },
      ];
      return content;
    },
  },
  {
    id: 'email-domain',
    nameKey: 'preset.seg.emailDomain.name',
    descKey: 'preset.seg.emailDomain.desc',
    build(key) {
      const content = presetSegment(key);
      content.constraints = [{ field: 'email', op: 'regex', value: '^.+@example\\.com$' }];
      return content;
    },
  },
];

// presetFlagContent returns a fresh document for one flag preset id, or null
// when the id is empty or unknown: callers then fall back to flagTemplate(key).
function presetFlagContent(id, key) {
  const preset = FLAG_PRESETS.find(item => item.id === id);
  return preset ? preset.build(key) : null;
}

// presetSegmentContent mirrors presetFlagContent for segments, falling back to
// segmentTemplate(key) at the call site.
function presetSegmentContent(id, key) {
  const preset = SEGMENT_PRESETS.find(item => item.id === id);
  return preset ? preset.build(key) : null;
}