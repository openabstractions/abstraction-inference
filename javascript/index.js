// complete() and a native async stream() over abstraction.inference/chat@1.
//
// resolveInference(machine) resolves the contract through the facade Machine
// and returns a Chat over that binding. stream() is an async generator of
// typed deltas; leaving a for-await loop early cancels the operation. start()
// is never retried.
import {ChatClient, ChatV2Client, DeltaKind, PageOutcome, PreloaderClient, ReplyOutcome, Role, StartOutcome, StopReason} from './js/abstraction/inference/api/index.mjs';

export {
  CancelOutcome, ChatClient, ChatV2Client, DeltaKind, DispatchError, ImageClient, ImageV2Client, ImageMode, PageOutcome, PartKind, PreloadOutcome,
  PreloaderClient, Refusal, ReplyOutcome, Role,
  SelectionOperatorV2Client, ServiceError,
  StartOutcome, StopReason, credentialConsumers, decode, encode, features, newMessage, newOptions, newPart, newRequest,
  newTemperature, newTool, RequestGuarantee, resourceActions,
} from './js/abstraction/inference/api/index.mjs';

export const chatContract = 'abstraction.inference/chat@1';
export const chatV2Contract = 'abstraction.inference/chat@2';
export const preloadContract = 'abstraction.inference/preload@1';
// Page bounds stream() uses for each observe.
export const pageDeltas = 256n;
export const pageBytes = 65536n;
export const pageWaitMs = 25000n;
const callMarginMs = 5000;

/** A reply whose outcome and fields contradict the contract. */
export class InconsistentReply extends Error {
  constructor(message) { super(message); this.name = 'InconsistentReply'; }
}

/** An observe outcome other than page: gap, unknown, invalid, forbidden or unavailable. */
export class ObserveRefused extends Error {
  constructor(outcome) { super(`inference observe ${outcome}`); this.name = 'ObserveRefused'; this.outcome = outcome; }
}

function require(ok, what) {
  if (!ok) throw new InconsistentReply(what);
}

/** Assembles a reply: a part delta extends the part at its index, and the end delta supplies the rest. */
export class Fold {
  #parts = new Map();
  #end = null;
  add(delta) {
    if (delta.kind === DeltaKind.Part && delta.part) {
      const part = this.#parts.get(delta.index);
      if (!part) { this.#parts.set(delta.index, {...delta.part}); return; }
      part.text += delta.part.text;
      part.arguments += delta.part.arguments;
      for (const field of ['callId', 'name', 'digest', 'mediaType']) {
        if (!part[field]) part[field] = delta.part[field];
      }
    } else if (delta.kind === DeltaKind.End && delta.end) {
      this.#end = delta.end;
    }
  }
  /** The folded reply, or null before the end delta. */
  reply() {
    if (this.#end === null) return null;
    const indexes = [...this.#parts.keys()].sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
    return {...this.#end, message: {role: Role.Assistant, parts: indexes.map((i) => this.#parts.get(i))}};
  }
}

function refused(admission) {
  return {
    outcome: ReplyOutcome[Object.keys(ReplyOutcome).find((k) => ReplyOutcome[k] === admission.outcome)],
    reason: admission.reason,
    message: {role: Role.Assistant, parts: []},
    stopReason: StopReason.NoStop,
    usage: {input: 0n, output: 0n, cached: 0n},
    host: '',
    model: '',
    cost: null,
  };
}

/** Model calls performed by the runtime for this program. */
export class Chat {
  #binding;
  #Client;
  constructor(binding, Client = ChatClient) { this.#binding = binding; this.#Client = Client; }
  #client(waitMs = 0n, cancellation = this.#binding.waiting?.cancellation ?? null) {
    const binding = this.#binding.withWaiting({
      deadline: performance.now() + callMarginMs + Number(waitMs),
      cancellation,
    });
    return new this.#Client(binding);
  }
  async start(request) {
    const admission = await this.#client().start(request);
    require((admission.outcome === StartOutcome.Accepted) === (admission.operation !== ''), 'inconsistent admission');
    return admission;
  }
  /** The call's budget is the transport margin plus waitMs. */
  async observe(operation, cursor, maxDeltas, maxBytes, waitMs) {
    if (maxDeltas < 1n || maxDeltas > 256n || maxBytes < 1n || maxBytes > 65536n || waitMs < 0n || waitMs > 30000n) {
      throw new RangeError('maxDeltas 1..256, maxBytes 1..65536, waitMs 0..30000');
    }
    const page = await this.#client(waitMs).observe(operation, cursor, maxDeltas, maxBytes, waitMs);
    if (page.outcome === PageOutcome.Page) {
      require(BigInt(page.deltas.length) <= maxDeltas && page.next === cursor + BigInt(page.deltas.length) &&
        page.deltas.every((d, i) => d.sequence === cursor + BigInt(i)), 'inconsistent delta page');
    } else if (page.outcome === PageOutcome.Gap) {
      require(page.deltas.length === 0 && page.next > cursor, 'inconsistent gap');
    } else {
      require(page.deltas.length === 0 && page.next === cursor && !page.atEnd, 'inconsistent page refusal');
    }
    return page;
  }
  async cancel(operation) { return this.#client().cancel(operation); }

  /** Start once and return its admission beside the managed delta stream. */
  async startStream(request) {
    const admission = await this.start(request);
    const accepted = admission.outcome === StartOutcome.Accepted;
    let ended = !accepted;
    let cancelPromise;
    const cancellation = this.#binding.waiting?.cancellation;
    let listening = false;
    const removeAbortListener = () => {
      if (listening) cancellation.removeEventListener('abort', onAbort);
      listening = false;
    };
    const cancel = () => {
      if (!accepted || ended) {
        removeAbortListener();
        return Promise.resolve();
      }
      if (!cancelPromise) {
        removeAbortListener();
        cancelPromise = this.#client(0n, null).cancel(admission.operation);
      }
      return cancelPromise;
    };
    const onAbort = () => { void cancel().catch(() => undefined); };
    if (accepted && typeof cancellation?.addEventListener === 'function') {
      cancellation.addEventListener('abort', onAbort, {once: true});
      listening = true;
      if (cancellation.aborted) onAbort();
    }
    const chat = this;
    const stream = (async function* () {
      if (!accepted) {
        yield {sequence: 0n, kind: DeltaKind.End, index: 0n, part: null, usage: null, end: refused(admission)};
        return;
      }
      try {
        let cursor = 0n;
        for (;;) {
          const page = await chat.observe(admission.operation, cursor, pageDeltas, pageBytes, pageWaitMs);
          if (page.outcome !== PageOutcome.Page) throw new ObserveRefused(page.outcome);
          for (const delta of page.deltas) {
            if (delta.kind === DeltaKind.End) {
              ended = true;
              removeAbortListener();
            }
            yield delta;
          }
          cursor = page.next;
          if (page.atEnd) {
            ended = true;
            removeAbortListener();
            return;
          }
        }
      } finally {
        if (!ended) {
          // Explicit cancel also covers an accepted operation whose stream is
          // never consumed and therefore never enters this generator.
          await cancel().catch(() => undefined);
        } else {
          removeAbortListener();
        }
      }
    })();
    return {admission, stream, cancel};
  }

  /** Deltas until the end delta; leaving early cancels the accepted operation. */
  async *stream(request) {
    const opened = await this.startStream(request);
    yield* opened.stream;
  }

  /** Start request, observe it to its end and fold the deltas into the reply. */
  async complete(request) {
    const fold = new Fold();
    for await (const delta of this.stream(request)) fold.add(delta);
    const reply = fold.reply();
    require(reply !== null, 'stream ended without an end delta');
    return reply;
  }
}

/** Resolve chat@1 through a facade Machine; resolution grants nothing. */
export async function resolveInference(machine, {guarantees = [], scope = 'any'} = {}) {
  return new Chat(await machine.resolveService(chatContract, {guarantees, scope}));
}

/** Resolve chat@2 explicitly. A missing service remains a resolution error; there is no chat@1 retry. */
export async function resolveInferenceV2(machine, {scope = 'any'} = {}) {
  return new Chat(await machine.resolveService(chatV2Contract, {scope}), ChatV2Client);
}

/** Resolve preload@1: a client whose preload(request) has the server a chat
 * request would reach evaluate the request's messages and tools ahead of the
 * caller's next request. */
export async function resolvePreload(machine, {guarantees = [], scope = 'any'} = {}) {
  return new PreloaderClient(await machine.resolveService(preloadContract, {guarantees, scope}));
}
