// Keep every generated protocol export at its existing crate path.
include!("../rs/abstraction/inference/api/rec.rs");

/// Managed chat@3 folding and observation over the existing generated service port.
pub mod managed {
    use super::*;
    use std::collections::BTreeMap;

    #[derive(Clone, Debug)]
    pub struct ReplyV3 {
        pub blocks: Vec<ChatPart>,
        pub block_ends: Vec<ChatBlockEnd>,
        pub result: ChatResult,
        pub text: String,
    }
    #[derive(Clone, Debug)]
    pub struct FoldV3 {
        pub cursor: i64,
        pub progress: Option<ChatProgress>,
        pub usage: Option<ChatUsage>,
        retain_reasoning: bool,
        partial: bool,
        parts: BTreeMap<i64, ChatPart>,
        closed: BTreeMap<i64, ChatBlockEnd>,
        end: Option<ChatResult>,
    }
    impl FoldV3 {
        pub fn new(retain_reasoning: bool, cursor: i64) -> Self {
            Self {
                cursor,
                progress: None,
                usage: None,
                retain_reasoning,
                partial: cursor > 0,
                parts: BTreeMap::new(),
                closed: BTreeMap::new(),
                end: None,
            }
        }
        pub fn add(&mut self, e: &ChatEvent) -> Result<(), &'static str> {
            if self.end.is_some() || e.sequence != self.cursor {
                return Err("event sequence");
            }
            match e.kind {
                ChatEventKind::Progress => {
                    self.progress = Some(e.progress.clone().ok_or("progress")?)
                }
                ChatEventKind::Usage => self.usage = Some(e.usage.clone().ok_or("usage")?),
                ChatEventKind::Part => {
                    let mut d = e.part.clone().ok_or("part")?;
                    if self.closed.contains_key(&d.block) {
                        return Err("closed block");
                    }
                    if let Some(p) = self.parts.get(&d.block) {
                        if p.kind != d.kind
                            || (!d.call_id.is_empty() && d.call_id != p.call_id)
                            || (!d.name.is_empty() && d.name != p.name)
                        {
                            return Err("block identity");
                        }
                    }
                    if !self.retain_reasoning
                        && matches!(
                            d.kind,
                            ChatPartKind::Reasoning | ChatPartKind::ReasoningSummary
                        )
                    {
                        d.text.clear();
                    }
                    if let Some(p) = self.parts.get_mut(&d.block) {
                        p.text.push_str(&d.text);
                        p.arguments.push_str(&d.arguments);
                        if d.seal.is_some() {
                            p.seal = d.seal;
                        }
                    } else {
                        self.parts.insert(d.block, d);
                    }
                }
                ChatEventKind::BlockEnd => {
                    let b = e.block_end.clone().ok_or("block end")?;
                    if (!self.parts.contains_key(&b.block) && !self.partial)
                        || self.closed.contains_key(&b.block)
                    {
                        return Err("block end identity");
                    }
                    self.closed.insert(b.block, b);
                }
                ChatEventKind::End => {
                    self.end = Some(e.end.clone().ok_or("terminal")?);
                }
            }
            self.cursor += 1;
            Ok(())
        }
        pub fn reply(&self) -> Option<ReplyV3> {
            let mut result = self.end.clone()?;
            if result.outcome == ChatResultOutcome::Completed
                && result
                    .finish_reason
                    .as_ref()
                    .is_some_and(|v| !finish_reason::ALL.contains(&v.as_str()))
            {
                result.finish_reason = Some(finish_reason::OTHER.into());
            }
            let blocks: Vec<_> = self.parts.values().cloned().collect();
            let text = blocks
                .iter()
                .filter(|p| p.kind == ChatPartKind::Text)
                .map(|p| p.text.as_str())
                .collect();
            Some(ReplyV3 {
                blocks,
                block_ends: self.closed.values().cloned().collect(),
                result,
                text,
            })
        }
    }
    #[derive(Debug)]
    pub enum ObserveError<E> {
        Call(E),
        Refused(PageOutcome),
        Inconsistent(&'static str),
    }
    /// A resumable handle. Dropping it ends only the local wait; cancel is explicit.
    pub struct SessionV3<'a, P: ChatV3> {
        pub admission: Option<ChatAdmission>,
        pub request: String,
        pub cursor: i64,
        pub fold: FoldV3,
        port: &'a P,
    }
    impl<'a, P: ChatV3> SessionV3<'a, P> {
        pub fn resume(port: &'a P, request: String, cursor: i64, retain_reasoning: bool) -> Self {
            Self {
                admission: None,
                request,
                cursor,
                fold: FoldV3::new(retain_reasoning, cursor),
                port,
            }
        }
        pub fn open(
            port: &'a P,
            request: RequestV3,
            retain_reasoning: bool,
        ) -> Result<Self, P::Error> {
            let a = port.start(request)?;
            let mut s = Self::resume(port, a.request.clone(), 0, retain_reasoning);
            s.admission = Some(a);
            Ok(s)
        }
        pub fn next_page(&mut self) -> Result<ChatPage, ObserveError<P::Error>> {
            let p = self
                .port
                .observe(self.request.clone(), self.cursor, 256, 65536, 25000)
                .map_err(ObserveError::Call)?;
            if p.outcome != PageOutcome::Page {
                return Err(ObserveError::Refused(p.outcome));
            }
            if p.events.len() > 256
                || self.cursor.checked_add(p.events.len() as i64) != Some(p.next)
            {
                return Err(ObserveError::Inconsistent("page cursor"));
            }
            for (i, e) in p.events.iter().enumerate() {
                if e.sequence != self.cursor + i as i64
                    || (e.kind == ChatEventKind::End && (i + 1 != p.events.len() || !p.at_end))
                {
                    return Err(ObserveError::Inconsistent("page sequence"));
                }
            }
            if p.at_end
                && p.events
                    .last()
                    .is_some_and(|e| e.kind != ChatEventKind::End)
            {
                return Err(ObserveError::Inconsistent("page terminal"));
            }
            let mut fold = self.fold.clone();
            for e in &p.events {
                fold.add(e).map_err(ObserveError::Inconsistent)?;
            }
            self.fold = fold;
            self.cursor = p.next;
            Ok(p)
        }
        pub fn cancel(&self) -> Result<ChatCancellation, P::Error> {
            self.port.cancel(self.request.clone())
        }
        pub fn complete(&mut self) -> Result<ReplyV3, ObserveError<P::Error>> {
            loop {
                if let Some(r) = self.fold.reply() {
                    return Ok(r);
                }
                let p = self.next_page()?;
                if p.at_end && self.fold.reply().is_none() {
                    return Err(ObserveError::Inconsistent("missing terminal"));
                }
            }
        }
    }
}

#[cfg(test)]
mod managed_tests {
    use super::managed::*;
    use super::*;
    use std::cell::Cell;
    #[test]
    fn policy_local_only_codec_roundtrip() {
        let request = decode_request_v3_document(include_bytes!(
            "../../../conformance/clients/inference/chat3_policy_local_only.json"
        ))
        .unwrap();
        let roundtrip =
            decode_request_v3_document(&encode_request_v3_document(&request).unwrap()).unwrap();
        for r in [request, roundtrip] {
            assert_eq!(r.target.selection_policy, "private");
            assert!(r.target.model.is_empty());
            assert_eq!(
                r.content.guarantees,
                vec![chat_guarantee::LOCAL_ONLY.to_string()]
            );
            assert!(r.content.credential.is_empty());
            assert_eq!(
                (r.identity.key.as_str(), r.identity.history_epoch.as_str()),
                ("privacy-key", "epoch")
            );
        }
    }
    #[test]
    fn shared_typed_fold_corpus() {
        let events: Vec<_> =
            include_str!("../../../conformance/clients/inference/chat3_managed.jsonl")
                .lines()
                .map(|s| decode_chat_event_document(s.as_bytes()).unwrap())
                .collect();
        let mut suffix = FoldV3::new(false, 2);
        for e in &events[2..] {
            suffix.add(e).unwrap();
        }
        let r = suffix.reply().unwrap();
        assert_eq!((r.blocks.len(), r.block_ends.len()), (4, 5));
        for outcome in [
            ChatResultOutcome::Completed,
            ChatResultOutcome::Failed,
            ChatResultOutcome::Cancelled,
        ] {
            let mut f = FoldV3::new(false, 0);
            let mut p = events[1].clone();
            let mut e = events[14].clone();
            p.sequence = 0;
            e.sequence = 1;
            let terminal = e.end.as_mut().unwrap();
            terminal.outcome = outcome;
            if outcome != ChatResultOutcome::Completed {
                terminal.finish_reason = None;
            }
            f.add(&p).unwrap();
            f.add(&e).unwrap();
            let r = f.reply().unwrap();
            assert_eq!(r.result.outcome, outcome);
            assert!(r.block_ends.is_empty());
        }
        for retain in [false, true] {
            let mut f = FoldV3::new(retain, 0);
            for e in &events {
                assert!(f.reply().is_none());
                f.add(e).unwrap();
            }
            let r = f.reply().unwrap();
            assert_eq!(r.text, "Hello world");
            assert_eq!(r.blocks.len(), 5);
            assert_eq!(r.blocks[3].arguments, "{}");
            assert_eq!(r.blocks[4].text, "declined");
            assert_eq!(!r.blocks[0].text.is_empty(), retain);
            assert_eq!(!r.blocks[1].text.is_empty(), retain);
            let seal = r.blocks[0].seal.as_ref().unwrap();
            assert_eq!(seal.origin, "vendor/opaque");
            assert_eq!(seal.bytes, vec![0, 255, 65]);
            assert_eq!(r.block_ends[1].seal_omitted, Some("encoded_size".into()));
            assert_eq!(f.progress.as_ref().unwrap().done, Some(0));
            assert_eq!(f.progress.as_ref().unwrap().total, None);
            assert_eq!(r.result.usage.input, Some(0));
            assert_eq!(r.result.usage.reasoning, None);
            assert_eq!(r.result.served_by.unwrap().server, "fixture");
            let timing = r.result.timing.as_ref().unwrap();
            assert_eq!(
                (
                    timing.queue_ms,
                    timing.load_ms,
                    timing.prefill_ms,
                    timing.decode_ms,
                    timing.reused_tokens
                ),
                (Some(0), Some(31), Some(14), Some(20), Some(0))
            );
            assert_eq!(timing.restore_ms, None);
            assert_eq!(
                (timing.load.as_deref(), timing.load_source.as_deref()),
                (Some("future_load"), Some("future_source"))
            );
            assert_eq!(r.result.finish_reason, Some("other".into()));
            assert_eq!(
                events[14].end.as_ref().unwrap().finish_reason.as_deref(),
                Some("future_vendor_finish")
            );
            assert!(f.add(&events[14]).is_err());
        }
        let mut f = FoldV3::new(false, 0);
        assert!(f.add(&events[1]).is_err());
        for e in &events[..3] {
            f.add(e).unwrap();
        }
        let mut d = events[1].clone();
        d.sequence = 3;
        assert!(f.add(&d).is_err());
    }
    struct Port {
        starts: Cell<i32>,
        cancels: Cell<i32>,
        fail: Cell<bool>,
    }
    impl ChatV3 for Port {
        type Error = &'static str;
        fn get_history_window(&self) -> Result<ChatHistoryRead, Self::Error> {
            Err("unused")
        }
        fn start(&self, _: RequestV3) -> Result<ChatAdmission, Self::Error> {
            self.starts.set(self.starts.get() + 1);
            decode_chat_admission_document(br#"{"outcome":"unknown","reason":"","request":"","server":"","model":"","retention_ms":0,"idle_ms":0,"retained_deltas":0,"preferred_model":""}"#).map_err(|_|"fixture")
        }
        fn observe(
            &self,
            id: String,
            cursor: i64,
            _: i64,
            _: i64,
            _: i64,
        ) -> Result<ChatPage, Self::Error> {
            assert_eq!((id.as_str(), cursor), ("retained", 7));
            if self.fail.get() {
                Err("wait deadline/disconnect")
            } else {
                decode_chat_page_document(
                    br#"{"outcome":"gap","events":[],"next":9,"at_end":false}"#,
                )
                .map_err(|_| "fixture")
            }
        }
        fn cancel(&self, _: String) -> Result<ChatCancellation, Self::Error> {
            self.cancels.set(self.cancels.get() + 1);
            Err("ack timeout")
        }
    }
    #[test]
    fn wait_errors_keep_handle_and_never_resubmit() {
        let port = Port {
            starts: Cell::new(0),
            cancels: Cell::new(0),
            fail: Cell::new(true),
        };
        let request=decode_request_v3_document(br#"{"identity":{"key":"key","history_epoch":"epoch","attempt":0},"target":{"model":"fixture"},"content":{"messages":[{"role":"user","parts":[{"kind":"text","text":"hello"}]}],"guarantees":[]}}"#).unwrap();
        let unknown = SessionV3::open(&port, request, false).unwrap();
        assert_eq!(
            unknown.admission.unwrap().outcome,
            ChatStartOutcome::Unknown
        );
        let mut s = SessionV3::resume(&port, "retained".into(), 7, false);
        for i in 0..3 {
            if i == 2 {
                port.fail.set(false);
            }
            assert!(s.next_page().is_err());
            assert_eq!(
                (s.request.as_str(), s.cursor, s.fold.cursor),
                ("retained", 7, 7)
            );
            assert_eq!((port.starts.get(), port.cancels.get()), (1, 0));
        }
        assert!(s.cancel().is_err());
        assert_eq!(port.cancels.get(), 1);
    }
}
