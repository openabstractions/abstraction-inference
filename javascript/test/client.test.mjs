import assert from 'node:assert/strict';
import test from 'node:test';
import {readFileSync} from 'node:fs';
import {FoldV3, ChatV3, ChatV3Session, ChatStartOutcome, ChatGuarantee} from '../index.js';
import {decodeChatEvent, decodeRequestV3, encodeRequestV3} from '../js/abstraction/inference/api/index.mjs';
import {Chat, PartKind, Role, chatV2Contract, resolveInferenceV2} from '../index.js';

const encoder = new TextEncoder();
const decoder = new TextDecoder();

test('chat@3 policy local_only survives actual codec roundtrip', () => {
  const bytes = readFileSync(new URL('../../../../conformance/clients/inference/chat3_policy_local_only.json', import.meta.url));
  const request = decodeRequestV3(bytes);
  const roundtrip = decodeRequestV3(encodeRequestV3(request));
  for (const r of [request,roundtrip]) {
    assert.equal(r.target.selectionPolicy,'private'); assert.equal(r.target.model,'');
    assert.deepEqual(r.content.guarantees,[ChatGuarantee.LocalOnly]); assert.equal(r.content.credential,'');
    assert.equal(r.identity.key,'privacy-key'); assert.equal(r.identity.historyEpoch,'epoch');
  }
});

test('chat@3 typed fold shared corpus preserves content and observations', () => {
  const events = readFileSync(new URL('../../../../conformance/clients/inference/chat3_managed.jsonl', import.meta.url), 'utf8').trim().split('\n').map(s => decodeChatEvent(encoder.encode(s)));
  const suffix = new FoldV3({cursor:2n}); for (const e of events.slice(2)) suffix.add(e);
  assert.equal(suffix.reply().blocks.length,4); assert.equal(suffix.reply().blockEnds.length,5);
  for (const outcome of ['completed','failed','cancelled']) {
    const f = new FoldV3(); const p = structuredClone(events[1]), e = structuredClone(events.at(-1));p.sequence=0n;e.sequence=1n;e.end.outcome=outcome;
    if (outcome !== 'completed') e.end.finishReason=null;
    f.add(p);f.add(e);assert.equal(f.reply().result.outcome,outcome);assert.deepEqual(f.reply().blockEnds,[]);
  }
  for (const retainReasoning of [false,true]) {
    const f = new FoldV3({retainReasoning});
    for (const e of events) { assert.equal(f.reply(), null); f.add(e); }
    const r = f.reply(); assert.equal(r.text, 'Hello world'); assert.equal(r.blocks.length, 5);
    assert.equal(r.blocks[3].arguments, '{}'); assert.equal(r.blocks[4].text, 'declined');
    assert.equal(Boolean(r.blocks[0].text), retainReasoning); assert.equal(Boolean(r.blocks[1].text), retainReasoning);
    assert.equal(r.blocks[0].seal.origin, 'vendor/opaque'); assert.deepEqual([...r.blocks[0].seal.bytes], [0,255,65]);
    assert.equal(r.blockEnds[1].sealOmitted, 'encoded_size');
    assert.equal(f.progress.done, 0n); assert.equal(f.progress.total, undefined);
    assert.equal(r.result.usage.input, 0n); assert.equal(r.result.usage.reasoning, undefined);
    assert.equal(r.result.servedBy.server, 'fixture'); assert.equal(r.result.finishReason, 'other');
    assert.deepEqual([r.result.timing.queueMs,r.result.timing.loadMs,r.result.timing.prefillMs,r.result.timing.decodeMs,r.result.timing.reusedTokens],[0n,31n,14n,20n,0n]);
    assert.equal(r.result.timing.restoreMs,undefined);assert.equal(r.result.timing.load,'future_load');assert.equal(r.result.timing.loadSource,'future_source');
    assert.equal(events.at(-1).end.finishReason,'future_vendor_finish');
    assert.throws(() => f.add(events.at(-1)));
  }
  const f = new FoldV3(); assert.throws(() => f.add(events[1]));
  for (const e of events.slice(0,3)) f.add(e);
  assert.throws(() => f.add({...events[1],sequence:3n}));
});

test('chat@3 sessions retain handles on wait errors and gap, never restart', async () => {
  let starts = 0, cancels = 0, failure = new Error('deadline');
  const chat = new ChatV3(null);
  chat.start = async () => { starts++; return {outcome:ChatStartOutcome.Unknown, request:''}; };
  chat.observe = async (id,cursor) => { assert.equal(id,'retained'); assert.equal(cursor,7n); if (failure) throw failure; return {outcome:'gap',events:[],next:9n,atEnd:false}; };
  chat.cancel = async () => { cancels++; };
  const unknown = await chat.open({}); assert.equal(unknown.admission.outcome, ChatStartOutcome.Unknown);
  const s = chat.resume('retained',7n);
  await assert.rejects(s.next()); failure = new Error('disconnect'); await assert.rejects(s.next());
  failure = null; await assert.rejects(s.next());
  assert.equal(s.request,'retained'); assert.equal(s.cursor,7n); assert.equal(s.fold.cursor,7n);
  assert.equal(starts,1); assert.equal(cancels,0); await s.cancel(); assert.equal(cancels,1);
  assert.ok(s instanceof ChatV3Session);
});

function reply(method, value, service = 'abstraction.inference/chat@1') {
  return encoder.encode(JSON.stringify({version: 1, service, method, ok: true,
    payload: {value}}));
}

class ScriptedBinding {
  constructor(state, waiting) { this.state = state; this.waiting = waiting; }
  withWaiting(waiting) { return new ScriptedBinding(this.state, waiting); }
  withCallDeadline(deadline) { this.state.boundedCalls = (this.state.boundedCalls ?? 0) + 1; return this.withWaiting({...this.waiting, deadline}); }
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
  withCallDeadline(deadline) { return this.withWaiting({...this.waiting, deadline}); }
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

test('caller guarantees use the generated additive method once and never retry unrestricted Start', async () => {
  for (const unsupported of [false, true]) {
    const calls = [];
    const binding = {
      withCallDeadline() { return this; },
      async exchangeFrame(frame) {
        const call = JSON.parse(decoder.decode(frame)); calls.push(call);
        assert.equal(call.method, 'StartWithGuarantees');
        const args = call.arguments;
        assert.deepEqual(args.guarantees, ['local-only']);
        assert.deepEqual(args.request.content.guarantees, []);
        if (unsupported) throw new Error('unknown_method: StartWithGuarantees');
        return reply(call.method, {outcome: 'no_host', reason: 'selection:placement', operation: '', host: '', model: '',
          retention_ms: 0, idle_ms: 0, retained_deltas: 0, preferred_model: ''}, chatV2Contract);
      },
    };
    const chat = await resolveInferenceV2({async resolveService() { return binding; }});
    if (unsupported) await assert.rejects(chat.startStream(policyRequest(), ['local-only']), /unknown_method/);
    else assert.equal((await chat.startStream(policyRequest(), ['local-only'])).admission.reason, 'selection:placement');
    assert.equal(calls.length, 1);
  }
});

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

test('ordinary inference calls forward their limit to the shared bounded-call owner', async () => {
  const state = {};
  const chat = new Chat(new ScriptedBinding(state, {}));
  await chat.start({model: 'fixture', messages: [], tools: [], options: null,
    extensions: {}, requiredExtensions: [], guarantees: [], credential: ''});
  await chat.observe('op', 0n, 1n, 1024n, 25000n);
  await chat.cancel('op');
  assert.equal(state.boundedCalls, 3);
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
