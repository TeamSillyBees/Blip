// Run against the unmodified modules from an installed DSH runtime. Registry
// membership and user answers are controlled; no model or network is involved.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { createRequire, syncBuiltinESMExports } from 'node:module';
import childProcess from 'node:child_process';

const [runtime, profile, manifestPath, capture] = process.argv.slice(2);
const require = createRequire(path.join(runtime,'package.json'));
const { Context, Service } = await import(pathToFileURL(require.resolve('@deepseek-ai/cordis')));
const { default: Loader } = await import(pathToFileURL(require.resolve('@deepseek-ai/cordis-plugin-loader')));
const { SessionStore } = await import(pathToFileURL(require.resolve('@deepseek-ai/dsh-session')));
const { scopeTarget } = await import(pathToFileURL(require.resolve('@deepseek-ai/dsh-scope')));
const originalSpawn = childProcess.spawnSync;
const captured = [];
childProcess.spawnSync = (exe,args,options) => {
  const value = JSON.parse(options.input);
  captured.push({ ...value, cwd:'C:/test/project', occurred_at: value.occurred_at ? '2026-11-07T00:00:00Z' : undefined });
  const result = originalSpawn(exe,args,options);
  assert.equal(result.status,0);
  assert.equal(result.stdout,'');
  return result;
};
syncBuiltinESMExports();
const members = new Map();
class Agents extends Service {
  constructor(ctx) { super(ctx,'agents'); }
  get(id) { return members.get(id); }
  roots() { return [...members.values()].filter(a=>!a.child); }
  turnBoundaryOf(session) { return { lastTurn: session.snapshotEvents().filter(e=>e.type==='turn/start').at(-1)?.data.turn??0 }; }
}
const ctx = new Context();
await ctx.plugin(Agents);
await ctx.plugin(SessionStore);
await ctx.plugin(Loader,{baseUrl:pathToFileURL(path.join(profile,'cordis.yml')).href});
const manifest = JSON.parse(fs.readFileSync(manifestPath,'utf8'));
await ctx.loader.create(manifest.entries.at(-1));
await ctx.loader.await();
function agent(id,child=false,origin) {
  const session=ctx.sessions.create(id,{meta:{cwd:path.resolve(profile),...(origin?{origin}:{})}});
  const a={id,session,child};members.set(id,a);return a;
}
const a=agent('test-session');
function start(turn, subject=a) { subject.session.append('turn/start',{turn}); }
function end(turn,reason,subject=a) { subject.session.append('turn/end',{turn,reason}); }
start(1);end(1,{kind:'completed'});
start(2);end(2,{kind:'error',error:{code:'TEST_ERROR',message:'never forwarded secret'}});
start(3);end(3,{kind:'aborted',reason:{kind:'user'}});
start(4);
const answer={answers:[{id:'q',selected:['A']}]};
const request={agent:a,questions:[{id:'q',question:'Choose one',options:[{label:'A'},{label:'B'}]}],wait:{callId:'input-1'}};
assert.equal(await ctx.waterfall(scopeTarget(a,a),'user-questions/request',request,async()=>answer),answer);
end(4,{kind:'completed'});
start(5);
const approval={...request,wait:{callId:'approval-1'},questions:[{id:'p',question:'Allow operation?',options:[{label:'Allow'},{label:'Deny'}],intent:{kind:'approval',approve:'Allow',reject:'Deny'}}]};
assert.equal(await ctx.waterfall(scopeTarget(a,a),'user-questions/request',approval,async()=>answer),answer);
end(5,{kind:'completed'});
start(6);
const timeout=Object.assign(new Error('still answerable'),{code:'ASK_TIMED_OUT'});
await assert.rejects(ctx.waterfall(scopeTarget(a,a),'user-questions/request',{...request,wait:{callId:'timed-1',timed:true}},async()=>{throw timeout}),e=>e===timeout);
end(6,{kind:'completed'});
const child=agent('child-session',true,'subagent');start(1,child);end(1,{kind:'completed'},child);
// A resumed child can be a current runtime root in this desktop version.
const resumed=agent('resumed-session',false,'subagent');start(2,resumed);end(2,{kind:'completed'},resumed);
start(7);end(7,{kind:'aborted',reason:{kind:'hook',reason:'policy stop'}});
assert.equal(captured.length,13,'native plugin did not observe all expected callbacks');
assert.equal(captured.some(e=>e.session_id==='child-session'),false);
assert.equal(captured.filter(e=>e.event==='resolved').length,2,'timed pending question was resolved');
assert.equal(JSON.stringify(captured).includes('never forwarded secret'),false);
fs.writeFileSync(capture,captured.map(e=>JSON.stringify(e)).join('\n')+'\n');
await ctx.loader.remove(manifest.entries.at(-1).id);
const before=captured.length;start(8);end(8,{kind:'completed'});assert.equal(captured.length,before,'unloaded plugin still active');
console.log(JSON.stringify({callbacks:captured.length,dispatch:'native Cordis Loader and SessionStore',registry:'controlled'}));
