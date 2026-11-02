'use strict';
const el=id=>document.getElementById(id);
let decision, scenario, exposed=false;
async function post(path,body){const response=await fetch(path,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});const result=await response.json();if(!response.ok)throw Error(result.error);return result;}
async function guard(fn){try{await fn();el('status').textContent='Synthetic action saved.';}catch(e){el('status').textContent=e.message;}}
async function event(kind){if(!decision)throw Error('Open an experience first.');if(kind!=='render'&&!exposed)throw Error('Wait for the experience to render.');const result=await post('/session/event',{scenario,kind,decision});el('result').textContent=JSON.stringify(result,null,2);if(kind==='render')exposed=true;return result;}
el('login').onclick=()=>guard(async()=>{await post('/idp/session',{subject:el('subject').value});decision=null;exposed=false;el('experience').hidden=true;});
el('open').onclick=()=>guard(async()=>{scenario=el('scenario').value;decision=await post('/session/evaluate',{scenario});exposed=false;const variant=decision.result.experiment.variant;const treatment=variant==='treatment';el('heading').textContent=scenario==='activation'?(treatment?'Create your first task':'Welcome to your workspace'):(treatment?'Try the sample upgrade':'Explore sample plans');el('guidance').textContent=treatment?'Choose this one next step. This is a synthetic variant.':'Review the sample instructions and choose an action. This is the control.';el('complete').textContent=scenario==='activation'?'Finish first sample task':'Confirm simulated upgrade';el('experience').dataset.variant=variant;el('experience').hidden=false;
 // Only a visible, painted experience emits onRender. No exposure on evaluation.
 const painted=()=>requestAnimationFrame(()=>requestAnimationFrame(()=>{if(document.visibilityState==='visible'&&!el('experience').hidden)guard(()=>event('render'));}));
 if(document.visibilityState==='visible')painted();else document.addEventListener('visibilitychange',()=>{if(document.visibilityState==='visible')painted();},{once:true});
});
el('complete').onclick=()=>guard(()=>event('complete'));
el('return').onclick=()=>guard(()=>event('return'));
