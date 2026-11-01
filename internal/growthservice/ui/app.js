'use strict';
const el = id => document.getElementById(id);
let current = null;
let polling;
const show = (id, value) => { el(id).textContent = JSON.stringify(value, null, 2); };
async function api(path, body, headers = {}) {
  const response = await fetch(path, { method: body === undefined ? 'GET' : 'POST', headers: { Authorization: 'Bearer ' + el('token').value, 'Content-Type': 'application/json', ...headers }, ...(body === undefined ? {} : {body: JSON.stringify(body)}) });
  const value = await response.json();
  if (!response.ok) throw new Error(value.error || 'Request failed');
  return value;
}
async function guard(fn) { try { await fn(); el('status').textContent = 'Saved or refreshed successfully.'; } catch (err) { el('status').textContent = err.message; if (current) { try { await select(current.id); } catch {} } } }
async function select(id) {
  current = await api('/v1/tasks/' + encodeURIComponent(id));
  el('work').hidden = false;
  el('task-title').textContent = current.goal.objective;
  el('state').textContent = current.state + (current.errorCode ? ' — ' + current.errorCode : '') + ' · revision ' + current.revision;
  show('evidence', { goal: current.goal, evidence: current.evidence, observation: current.observation, suggestions: current.suggestions, readiness: current.readiness, remediation: current.remediation, review: current.review });
  el('proposal').value = JSON.stringify(current.proposal, null, 2);
  show('prepared', current.prepared);
  show('learning-view', { learning: current.learning, rollout: current.rollout, experiment: current.manifest });
  show('audit', {audit: await api('/v1/tasks/' + encodeURIComponent(id) + '/audit'), versions: await api('/v1/tasks/' + encodeURIComponent(id) + '/versions')});
  clearTimeout(polling);
  if (['QUEUED','ANALYZING'].includes(current.state)) polling = setTimeout(() => guard(() => select(id)), 1000);
}
async function list() {
  const tasks = await api('/v1/tasks'); el('tasks').replaceChildren();
  for (const task of tasks) {
    const li = document.createElement('li'); const button = document.createElement('button');
    button.textContent = task.goal.objective + ' · ' + task.state;
    button.onclick = () => guard(() => select(task.id)); li.append(button); el('tasks').append(li);
  }
}
async function action(name, extra = {}) {
  if (!current) throw new Error('Select a task first.');
  const headers = {};
  if (['submit', 'platform-review', 'bind'].includes(name)) headers['X-Grayling-Token'] = el('upstream').value;
  try { current = await api('/v1/tasks/' + current.id + '/' + name, {revision: current.revision, ...extra}, headers); }
  finally { el('upstream').value = ''; }
  await select(current.id); await list();
}
el('connect').onclick = el('refresh').onclick = () => guard(list);
el('create').onclick = () => guard(async () => { const task = await api('/v1/tasks', JSON.parse(el('goal').value), {'Idempotency-Key': el('key').value}); await select(task.id); await list(); });
el('edit').onclick = () => guard(() => action('edit', {proposal: JSON.parse(el('proposal').value)}));
for (const [id, decision] of [['approve','approve'],['reject','reject'],['changes','request_changes']]) el(id).onclick = () => guard(() => action('review', {decision, rationale: el('rationale').value}));
el('prepare').onclick = () => guard(() => action('prepare', {settings: JSON.parse(el('settings').value)}));
el('learning').onclick = () => guard(() => action('learning', {followups: el('followups').value.split('\n').filter(Boolean)}));
for (const name of ['submit','platform-review','bind','monitor','retry','cancel']) el(name).onclick = () => guard(() => action(name));
el('search').onclick = () => guard(async () => show('results', await api('/v1/learning?text=' + encodeURIComponent(el('query').value))));
