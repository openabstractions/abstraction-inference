"""complete() and stream() over abstraction.inference/chat@1.

Chat wraps the generated ChatClient on one resolved transport. stream() is a
generator of typed deltas; closing it before the end cancels the operation.
start() is never retried.
"""
import time

import abstraction.inference.api as api

PAGE_DELTAS = 256
PAGE_BYTES = 65536
PAGE_WAIT_MS = 25000


class InconsistentReply(ValueError):
    """A reply whose outcome and fields contradict the contract."""


class ObserveRefused(RuntimeError):
    """An observe outcome other than page: gap, unknown, invalid, forbidden or unavailable."""

    def __init__(self, outcome):
        super().__init__("inference observe " + outcome.value)
        self.outcome = outcome


def _require(ok, what):
    if not ok:
        raise InconsistentReply(what)


class Fold:
    """Assembles a reply: a part delta extends the part at its index, and the end delta supplies the rest."""

    def __init__(self):
        self._parts = {}
        self._end = None

    def add(self, delta):
        if delta.kind == api.DeltaKind.PART and delta.part is not None:
            part = self._parts.get(delta.index)
            if part is None:
                self._parts[delta.index] = api.Part(kind=delta.part.kind, text=delta.part.text, digest=delta.part.digest,
                                                    media_type=delta.part.media_type, call_id=delta.part.call_id,
                                                    name=delta.part.name, arguments=delta.part.arguments)
                return
            part.text += delta.part.text
            part.arguments += delta.part.arguments
            for field in ("call_id", "name", "digest", "media_type"):
                if not getattr(part, field):
                    setattr(part, field, getattr(delta.part, field))
        elif delta.kind == api.DeltaKind.END and delta.end is not None:
            self._end = delta.end

    def reply(self):
        """The folded reply, or None before the end delta."""
        if self._end is None:
            return None
        parts = [self._parts[i] for i in sorted(self._parts)]
        end = self._end
        return api.Reply(outcome=end.outcome, reason=end.reason, message=api.Message(role=api.Role.ASSISTANT, parts=parts),
                         stop_reason=end.stop_reason, usage=end.usage, host=end.host, model=end.model, cost=end.cost)


class Chat:
    """Model calls performed by the runtime for this program."""

    def __init__(self, transport):
        self._transport = transport

    def _client(self, wait_ms=0):
        transport = self._transport
        if wait_ms and transport.deadline is None:
            transport = transport.with_waiting(deadline=time.monotonic() + transport.timeout + wait_ms / 1000,
                                               cancellation=transport.cancellation)
        return api.ChatClient(transport)

    def start(self, request):
        admission = self._client().start(request)
        _require((admission.outcome == api.StartOutcome.ACCEPTED) == bool(admission.operation), "inconsistent admission")
        return admission

    def observe(self, operation, cursor, max_deltas, max_bytes, wait_ms):
        if not (1 <= max_deltas <= 256 and 1 <= max_bytes <= 65536 and 0 <= wait_ms <= 30000):
            raise ValueError("max_deltas 1..256, max_bytes 1..65536, wait_ms 0..30000")
        page = self._client(wait_ms).observe(operation, cursor, max_deltas, max_bytes, wait_ms)
        if page.outcome == api.PageOutcome.PAGE:
            _require(len(page.deltas) <= max_deltas and page.next == cursor + len(page.deltas)
                     and all(d.sequence == cursor + i for i, d in enumerate(page.deltas)), "inconsistent delta page")
        elif page.outcome == api.PageOutcome.GAP:
            _require(not page.deltas and page.next > cursor, "inconsistent gap")
        else:
            _require(not page.deltas and page.next == cursor and not page.at_end, "inconsistent page refusal")
        return page

    def cancel(self, operation):
        return self._client().cancel(operation)

    def stream(self, request):
        """Yield deltas until the end delta, which carries the reply.

        A start refusal yields one end delta carrying that outcome. Closing the
        generator early, or an exception, cancels the operation. A gap or an
        observe refusal raises ObserveRefused.
        """
        admission = self.start(request)
        if admission.outcome != api.StartOutcome.ACCEPTED:
            yield api.Delta(sequence=0, kind=api.DeltaKind.END, index=0, part=None, usage=None,
                            end=api.Reply(outcome=api.ReplyOutcome(admission.outcome.value), reason=admission.reason,
                                          message=api.Message(role=api.Role.ASSISTANT, parts=[]),
                                          stop_reason=api.StopReason.NO_STOP, usage=api.Usage(input=0, output=0, cached=0),
                                          host="", model="", cost=None))
            return
        ended = False
        try:
            cursor = 0
            while True:
                page = self.observe(admission.operation, cursor, PAGE_DELTAS, PAGE_BYTES, PAGE_WAIT_MS)
                if page.outcome != api.PageOutcome.PAGE:
                    raise ObserveRefused(page.outcome)
                for delta in page.deltas:
                    if delta.kind == api.DeltaKind.END:
                        ended = True
                    yield delta
                cursor = page.next
                if page.at_end:
                    ended = True
                    return
        finally:
            if not ended:
                try:
                    self.cancel(admission.operation)
                except Exception:  # the operation idles out when cancel cannot be sent
                    pass

    def complete(self, request):
        """Start request, observe it to its end and fold the deltas into the reply."""
        fold = Fold()
        for delta in self.stream(request):
            fold.add(delta)
        reply = fold.reply()
        _require(reply is not None, "stream ended without an end delta")
        return reply
