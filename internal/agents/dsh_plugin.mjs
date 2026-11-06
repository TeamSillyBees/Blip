// Embedded by Blip's DSH installer. No shell, network, or decision output.
import { spawnSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { isAbsolute } from 'node:path';

export const name = 'blip-handoffs';
export const inject = ['agents'];

export function apply(ctx, config) {
  if (!isAbsolute(config.executable ?? '') || !isAbsolute(config.configPath ?? '') ||
      !/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$/.test(config.sourceId ?? '')) {
    throw new Error('Blip requires absolute executable/config paths and a source ID');
  }
  function root(session) {
    const agent = ctx.agents.get(session.header.id);
    if (!agent || agent.session !== session) return false;
    // New runtimes distinguish current ownership from a resumed child's lineage.
    if (typeof ctx.agents.roots === 'function') return ctx.agents.roots().includes(agent);
    return session.header.origin !== 'subagent';
  }
  function payload(session, event, turn, extra = {}) {
    if (!session || !root(session) || !Number.isSafeInteger(turn) || turn < 1) return;
    return { blip_dsh_version: 1, event, session_id: session.header.id,
      turn_id: String(turn), root: true,
      cwd: ctx.get('workingDirectory')?.get(session) ?? session.header.cwd ?? '', ...extra };
  }
  function send(value) {
    if (!value) return;
    try {
      const input = JSON.stringify(value);
      if (Buffer.byteLength(input) > 1024 * 1024) throw new Error('payload size');
      // session/event is synchronous and does not await listeners. Complete
      // durable capture before returning; never leave a detached write behind.
      const result = spawnSync(config.executable,
        ['--config', config.configPath, 'hook', '--source', config.sourceId],
        { input, encoding: 'utf8', shell: false, windowsHide: true,
          timeout: 4000, maxBuffer: 4096, stdio: ['pipe', 'pipe', 'pipe'] });
      if (result.error || result.status !== 0) throw new Error('capture failed');
    } catch {
      ctx.logger.warn('Blip callback failed; inspect blip doctor for this source.');
    }
  }
  ctx.on('session/event', (session, event) => {
    if (event.type !== 'turn/end') return;
    const reason = event.data.reason;
    send(payload(session, 'turn/end', event.data.turn, {
      occurred_at: new Date(event.time).toISOString(),
      reason: reason?.kind ?? 'unknown', cancel_cause: reason?.reason?.kind,
      error_code: typeof reason?.error?.code === 'string' ? reason.error.code.slice(0,120) : undefined,
    }));
  }, { global: true });
  ctx.on('user-questions/request', async (request, next) => {
    const session = request.agent?.session;
    if (!session || !root(session) || request.signal?.aborted) return next();
    const turn = ctx.agents.turnBoundaryOf(session).lastTurn;
    const requestId = request.wait?.callId ?? randomUUID();
    const base = payload(session, 'request', turn, { request_id: requestId });
    const questions = (request.questions ?? []).slice(0,20).map(q => ({
      text: String(q.question ?? '').slice(0,2000),
      options: (q.options ?? []).slice(0,20).map(o => String(o.label ?? '').slice(0,300)),
    }));
    send(base && { ...base, kind: request.questions?.some(q => q.intent?.kind === 'approval')
      ? 'approval' : 'input', questions });
    try {
      const answer = await next();
      send(base && { ...base, event: 'resolved' });
      return answer;
    } catch (error) {
      // A timed question stays answerable after the foreground wait returns.
      // Keep its specific reminder as coverage of the same turn's stop.
      if (error?.code !== 'ASK_TIMED_OUT') send(base && { ...base, event: 'resolved' });
      throw error;
    }
  }, { global: true, prepend: true });
}
