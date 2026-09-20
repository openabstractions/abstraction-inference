// complete() and a native async stream() over abstraction.inference/chat@1.
//
// resolveInference(machine) resolves the contract through the facade Machine
// and returns a Chat over that binding. stream() is an async generator of
// typed deltas; leaving a for-await loop early cancels the operation. start()
// is never retried.
import {ChatClient, DeltaKind, PageOutcome, ReplyOutcome, Role, StartOutcome, StopReason} from './js/abstraction/inference/api/index.mjs';

export {
  CancelOutcome, ChatClient, DeltaKind, DispatchError, PageOutcome, PartKind, Refusal, ReplyOutcome, Role, ServiceError,
  StartOutcome, StopReason, credentialConsumers, decode, encode, features, newMessage, newOptions, newPart, newRequest,
  newTemperature, newTool, RequestGuarantee, resourceActions,
} from './js/abstraction/inference/api/index.mjs';

export const chatContract = 'abstraction.inference/chat@1';
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
  constructor(binding) { this.#binding = binding; }
  #client(waitMs = 0n, cancellation = this.#binding.waiting?.cancellation ?? null) {
    const binding = this.#binding.withWaiting({
      deadline: performance.now() + callMarginMs + Number(waitMs),
      cancellation,
    });
    return new ChatClient(binding);
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

  /**
   * Deltas until the end delta, which carries the reply. A start refusal is one
   * end delta carrying that outcome. Leaving the loop early, or an exception,
   * cancels the operation. A gap or an observe refusal throws ObserveRefused.
   */
  async *stream(request) {
    const admission = await this.start(request);
    if (admission.outcome !== StartOutcome.Accepted) {
      yield {sequence: 0n, kind: DeltaKind.End, index: 0n, part: null, usage: null, end: refused(admission)};
      return;
    }
    let ended = false;
    try {
      let cursor = 0n;
      for (;;) {
        const page = await this.observe(admission.operation, cursor, pageDeltas, pageBytes, pageWaitMs);
        if (page.outcome !== PageOutcome.Page) throw new ObserveRefused(page.outcome);
        for (const delta of page.deltas) {
          if (delta.kind === DeltaKind.End) ended = true;
          yield delta;
        }
        cursor = page.next;
        if (page.atEnd) { ended = true; return; }
      }
    } finally {
      if (!ended) {
        // Cleanup gets a fresh bounded wait after caller cancellation, like the
        // Go client's context.WithoutCancel cleanup. If it still cannot be sent,
        // the service's idle bound retires the operation.
        await this.#client(0n, null).cancel(admission.operation).catch(() => undefined);
      }
    }
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
