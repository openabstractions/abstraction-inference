import assert from 'node:assert/strict';
import test from 'node:test';
import {Chat, PartKind, Role} from '../index.js';

const encoder = new TextEncoder();
const decoder = new TextDecoder();

function reply(method, value) {
  return encoder.encode(JSON.stringify({version: 1, service: 'abstraction.inference/chat@1', method, ok: true,
    payload: {value}}));
}

class ScriptedBinding {
  constructor(state, waiting) { this.state = state; this.waiting = waiting; }
  withWaiting(waiting) { return new ScriptedBinding(this.state, waiting); }
  async exchangeFrame(frame) {
    const call = JSON.parse(decoder.decode(frame));
    if (call.method === 'Start') {
      return reply('Start', {outcome: 'accepted', reason: '', operation: 'op', host: 'fixture', model: 'fixture',
        retention_ms: 60000, idle_ms: 10000, retained_deltas: 16});
    }
    if (call.method === 'Observe') {
      return reply('Observe', {outcome: 'page', deltas: [{sequence: 0, kind: 'part', index: 0,
        part: {kind: 'text', text: 'first', digest: '', media_type: '', call_id: '', name: '', arguments: ''}}],
        next: 1, at_end: false});
    }
    if (call.method === 'Cancel') {
      this.state.cancelWaiting = this.waiting;
      return reply('Cancel', {outcome: 'cancelled'});
    }
    throw new Error(`unexpected ${call.method}`);
  }
}

test('stream cleanup cancels with a fresh bounded wait after caller abort', async () => {
  const abort = new AbortController();
  const state = {};
  const binding = new ScriptedBinding(state, {cancellation: abort.signal});
  const chat = new Chat(binding);
  const iterator = chat.stream({model: 'fixture', messages: [{role: Role.User, parts: [{kind: PartKind.Text,
    text: 'hello', digest: '', mediaType: '', callId: '', name: '', arguments: ''}]}], tools: [], options: null,
    extensions: {}, requiredExtensions: [], guarantees: [], credential: ''})[Symbol.asyncIterator]();
  assert.equal((await iterator.next()).value.part.text, 'first');
  abort.abort();
  await iterator.return();
  assert.equal(state.cancelWaiting.cancellation, null);
  assert.ok(Number.isFinite(state.cancelWaiting.deadline));
  assert.ok(state.cancelWaiting.deadline > performance.now());
  assert.ok(state.cancelWaiting.deadline <= performance.now() + 5000);
});
