import assert from 'node:assert/strict';
import test from 'node:test';
import {Chat, PartKind, Role, chatV2Contract, resolveInferenceV2} from '../index.js';

const encoder = new TextEncoder();
const decoder = new TextDecoder();

function reply(method, value, service = 'abstraction.inference/chat@1') {
  return encoder.encode(JSON.stringify({version: 1, service, method, ok: true,
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

class ScriptedV2Binding {
  constructor(state, waiting = {}) { this.state = state; this.waiting = waiting; }
  withWaiting(waiting) { return new ScriptedV2Binding(this.state, waiting); }
  async exchangeFrame(frame) {
    const call = JSON.parse(decoder.decode(frame));
    this.state.calls.push(call.method);
    if (call.method === 'Start') {
      return reply('Start', {outcome: 'accepted', reason: '', operation: 'policy-operation', host: 'actual-host',
        model: 'actual-model', retention_ms: 60000, idle_ms: 10000, retained_deltas: 16,
        preferred_model: 'preferred-model', selection_reason: 'preferred'}, chatV2Contract);
    }
    if (call.method === 'Observe') {
      if (this.state.pages.length === 0 && this.state.observeError) throw this.state.observeError;
      return reply('Observe', this.state.pages.shift(), chatV2Contract);
    }
    if (call.method === 'Cancel') {
      this.state.cancelCount++;
      this.state.resolveCancelled?.();
      return reply('Cancel', {outcome: 'cancelled'}, chatV2Contract);
    }
    throw new Error(`unexpected chat@2 ${call.method}`);
  }
}

function v2EndDelta(outcome = 'completed') {
  return {sequence: 0, kind: 'end', index: 0, end: {outcome, reason: '', host: 'actual-host',
    model: 'actual-model', message: {role: 'assistant', parts: []}, stop_reason: 'end', usage: {input: 1, output: 1, cached: 0}}};
}

function v2AdmissionState(overrides = {}) {
  let resolveCancelled;
  return {calls: [], cancelCount: 0, pages: [], observeError: null,
    cancelled: new Promise((resolve) => { resolveCancelled = resolve; }), resolveCancelled, ...overrides};
}

function policyRequest() {
  return {target: {model: '', selectionPolicy: 'preferredcloud-local'}, content: {
    messages: [], tools: [], options: null, extensions: {}, requiredExtensions: [], guarantees: [], credential: '',
  }};
}

async function v2Chat(state, abortSignal) {
  return resolveInferenceV2({async resolveService(contract, options) {
    assert.equal(contract, chatV2Contract);
    assert.equal(options.scope, 'local');
    return new ScriptedV2Binding(state, {cancellation: abortSignal ?? null});
  }}, {scope: 'local'});
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

test('chat@2 resolves the generated client and exposes its single accepted admission', async () => {
  const state = v2AdmissionState({pages: [{outcome: 'page', deltas: [v2EndDelta()], next: 1, at_end: true}]});
  const chat = await v2Chat(state);
  const opened = await chat.startStream(policyRequest());
  assert.equal(opened.admission.operation, 'policy-operation');
  assert.equal(opened.admission.host, 'actual-host');
  assert.equal(opened.admission.model, 'actual-model');
  assert.equal(opened.admission.preferredModel, 'preferred-model');
  assert.equal(opened.admission.selectionReason, 'preferred');
  const deltas = [];
  for await (const delta of opened.stream) deltas.push(delta);
  assert.equal(state.calls.filter((method) => method === 'Start').length, 1);
  assert.equal(state.cancelCount, 0);
  assert.equal(deltas[0].end.model, 'actual-model');
});

test('chat@2 explicit handle cancels an accepted operation when the caller never consumes it', async () => {
  const state = v2AdmissionState();
  const chat = await v2Chat(state);
  const opened = await chat.startStream(policyRequest());
  await opened.cancel();
  await state.cancelled;
  assert.equal(state.calls.filter((method) => method === 'Start').length, 1);
  assert.equal(state.cancelCount, 1);
});

test('chat@2 abort immediately after Start cancels before the first Observe', async () => {
  const abort = new AbortController();
  const state = v2AdmissionState();
  const chat = await v2Chat(state, abort.signal);
  await chat.startStream(policyRequest());
  abort.abort();
  await state.cancelled;
  assert.equal(state.calls.includes('Observe'), false);
  assert.equal(state.cancelCount, 1);
});

test('chat@2 partial output followed by an observe error keeps one admission and cancels once', async () => {
  const state = v2AdmissionState({pages: [
    {outcome: 'page', deltas: [{sequence: 0, kind: 'part', index: 0,
      part: {kind: 'text', text: 'partial', digest: '', media_type: '', call_id: '', name: '', arguments: ''}}], next: 1, at_end: false},
  ], observeError: new Error('fixture observe failure')});
  const chat = await v2Chat(state);
  const opened = await chat.startStream(policyRequest());
  const iterator = opened.stream[Symbol.asyncIterator]();
  assert.equal((await iterator.next()).value.part.text, 'partial');
  await assert.rejects(iterator.next(), /fixture observe failure/);
  await state.cancelled;
  assert.equal(state.calls.filter((method) => method === 'Start').length, 1);
  assert.equal(state.cancelCount, 1);
});
