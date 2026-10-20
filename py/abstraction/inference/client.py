"""complete() and stream() over abstraction.inference/chat@1.

Chat wraps the generated ChatClient on one resolved transport. stream() is a
generator of typed deltas; closing it before the end cancels the operation.
start() is never retried.
"""
import time
import copy

import abstraction.inference.api as api

# chat@3 raw types remain available through the convenience module.
from abstraction.inference.api import (RequestV3, ChatAdmission, ChatPage, ChatEvent,
    ChatEventKind, ChatPart, ChatPartKind, ChatResult, ChatTiming, ChatLoad, ChatLoadSource, ChatUsage, ChatProgress,
    ChatBlockEnd, ReasoningSeal, ChatHistoryRead, ChatHistoryOutcome,
    ChatStartOutcome, ChatResultOutcome, FinishReason, SealOmission, ServedBy,
    ServedPlace, ChatTarget, ChatContent, ChatMessage, ChatInputPart, ChatTool,
    ChatOptions, ChatToolChoice, ChatToolChoiceKind, ChatGuarantee, ChatReplay,
    ChatProgressPhase, ChatCancellation)


class FoldV3:
    """Fold ordered chat@3 events; retain public reasoning by explicit choice."""

    def __init__(self, retain_reasoning=False, cursor=0):
        self.retain_reasoning = retain_reasoning
        self.cursor = cursor
        self._partial = cursor > 0
        self.progress = None
        self.usage = None
        self._parts = {}
        self._closed = {}
        self._end = None

    def add(self, event):
        _require(self._end is None and event.sequence == self.cursor, "inconsistent event sequence")
        if event.kind == api.ChatEventKind.PROGRESS:
            _require(event.progress is not None, "missing progress")
            self.progress = copy.deepcopy(event.progress)
        elif event.kind == api.ChatEventKind.USAGE:
            _require(event.usage is not None, "missing usage")
            self.usage = copy.deepcopy(event.usage)
        elif event.kind == api.ChatEventKind.PART:
            _require(event.part is not None, "missing part")
            d = copy.deepcopy(event.part)
            _require(d.block not in self._closed, "closed block")
            p = self._parts.get(d.block)
            _require(p is None or (p.kind == d.kind and (not d.call_id or d.call_id == p.call_id)
                                     and (not d.name or d.name == p.name)), "changed block identity")
            if not self.retain_reasoning and d.kind in (api.ChatPartKind.REASONING, api.ChatPartKind.REASONING_SUMMARY):
                d.text = ""
            if p is None:
                self._parts[d.block] = d
            else:
                p.text += d.text
                p.arguments += d.arguments
                if d.seal is not None:
                    p.seal = d.seal
        elif event.kind == api.ChatEventKind.BLOCK_END:
            _require(event.block_end is not None, "missing block end")
            b = event.block_end
            _require((b.block in self._parts or self._partial) and b.block not in self._closed, "invalid block end")
            self._closed[b.block] = copy.deepcopy(b)
        elif event.kind == api.ChatEventKind.END:
            _require(event.end is not None, "missing terminal")
            self._end = copy.deepcopy(event.end)
        else:
            raise InconsistentReply("unknown event kind")
        self.cursor += 1

    def reply(self):
        """Actual terminal plus typed blocks; progress and reasoning never enter text."""
        if self._end is None:
            return None
        blocks = [copy.deepcopy(self._parts[k]) for k in sorted(self._parts)]
        result = copy.deepcopy(self._end)
        if result.outcome == api.ChatResultOutcome.COMPLETED and result.finish_reason is not None and result.finish_reason not in tuple(api.FinishReason):
            result.finish_reason = api.FinishReason.OTHER
        return {"result": result, "blocks": blocks,
                "block_ends": [copy.deepcopy(self._closed[k]) for k in sorted(self._closed)],
                "text": "".join(p.text for p in blocks if p.kind == api.ChatPartKind.TEXT)}


class ChatV3:
    """Explicit chat@3 calls. Start is sent once; observation errors preserve work."""

    def __init__(self, transport):
        self._transport = transport

    def _client(self, wait_ms=0):
        transport = self._transport
        if wait_ms and transport.deadline is None:
            transport = transport.with_waiting(deadline=time.monotonic() + transport.timeout + wait_ms / 1000,
                                               cancellation=transport.cancellation)
        return api.ChatV3Client(transport)

    def start(self, request):
        return self._client().start(request)

    def get_history_window(self):
        return self._client().get_history_window()

    def cancel(self, request):
        return self._client().cancel(request)

    def observe(self, request, cursor, max_deltas=256, max_bytes=65536, wait_ms=25000):
        _require(cursor >= 0 and 1 <= max_deltas <= 256 and 1 <= max_bytes <= 65536 and 0 <= wait_ms <= 30000, "invalid page bounds")
        page = self._client(wait_ms).observe(request, cursor, max_deltas, max_bytes, wait_ms)
        if page.outcome == api.PageOutcome.PAGE:
            _require(len(page.events) <= max_deltas and page.next == cursor + len(page.events), "invalid page cursor")
            for i, event in enumerate(page.events):
                _require(event.sequence == cursor + i and (event.kind != api.ChatEventKind.END or (i == len(page.events)-1 and page.at_end)), "invalid page sequence")
            _require(not page.at_end or not page.events or page.events[-1].kind == api.ChatEventKind.END, "missing page terminal")
        else:
            _require(not page.events and not page.at_end and (page.next > cursor if page.outcome == api.PageOutcome.GAP else page.next == cursor), "invalid page refusal")
        return page

    def resume(self, request, cursor=0, retain_reasoning=False):
        return ChatV3Session(self, request, cursor, retain_reasoning)

    def open(self, request, retain_reasoning=False):
        admission = self.start(request)
        session = self.resume(admission.request, retain_reasoning=retain_reasoning)
        session.admission = admission
        return session


class ChatV3Session:
    """Resumable observation; cancel is explicit and never performed by next."""

    def __init__(self, chat, request, cursor=0, retain_reasoning=False):
        self.chat, self.request, self.cursor = chat, request, cursor
        self.admission = None
        self.fold = FoldV3(retain_reasoning, cursor)

    def next(self):
        page = self.chat.observe(self.request, self.cursor)
        if page.outcome != api.PageOutcome.PAGE:
            raise ObserveRefused(page.outcome)
        fold = copy.deepcopy(self.fold)
        for event in page.events:
            fold.add(event)
        self.fold = fold
        self.cursor = page.next
        return page

    def cancel(self):
        return self.chat.cancel(self.request)

    def complete(self):
        """Drain this handle; errors preserve it for explicit continuation."""
        while self.fold.reply() is None:
            page = self.next()
            _require(not page.at_end or self.fold.reply() is not None, "missing terminal")
        return self.fold.reply()

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
