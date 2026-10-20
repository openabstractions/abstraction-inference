#pragma once
#include <abstraction/inference/api/rec.h>
#include <abstraction/ipc/frame.hpp>
#include <algorithm>
#include <cstdint>
#include <functional>
#include <map>
#include <optional>
#include <string>
namespace abstraction::inference {
namespace detail {
inline void require(bool ok,const char* what){if(!ok)throw api::ServiceError("invalid_response",what);}
}
struct ChatV3Reply { std::vector<api::ChatPart> blocks; std::vector<api::ChatBlockEnd> block_ends; api::ChatResult result; std::string text; };
template<class Transport> using ChatV3 = api::ChatV3Client<Transport>;
// Public reasoning retention is explicit; opaque replay material stays unchanged.
class FoldV3 {
public:
 explicit FoldV3(bool retain_reasoning=false,std::int64_t cursor=0):cursor(cursor),retain_(retain_reasoning),partial_(cursor>0){}
 std::int64_t cursor;
 std::optional<api::ChatProgress> progress;
 std::optional<api::ChatUsage> usage;
 void add(const api::ChatEvent& e){
  detail::require(!end_&&e.sequence==cursor,"event sequence");
  switch(e.kind){
  case api::ChatEventKind::Progress:detail::require(e.progress.has_value(),"progress");progress=e.progress;break;
  case api::ChatEventKind::Usage:detail::require(e.usage.has_value(),"usage");usage=e.usage;break;
  case api::ChatEventKind::Part:{
   detail::require(e.part.has_value(),"part");auto d=*e.part;detail::require(!closed_.count(d.block),"closed block");auto p=parts_.find(d.block);
   detail::require(p==parts_.end()||(p->second.kind==d.kind&&(d.call_id.empty()||d.call_id==p->second.call_id)&&(d.name.empty()||d.name==p->second.name)),"block identity");
   if(!retain_&&(d.kind==api::ChatPartKind::Reasoning||d.kind==api::ChatPartKind::ReasoningSummary))d.text.clear();
   if(p==parts_.end())parts_.emplace(d.block,d);else{p->second.text+=d.text;p->second.arguments+=d.arguments;if(d.seal)p->second.seal=d.seal;}break;
  }
  case api::ChatEventKind::BlockEnd:detail::require(e.block_end.has_value()&&(parts_.count(e.block_end->block)||partial_)&&!closed_.count(e.block_end->block),"block end");closed_.emplace(e.block_end->block,*e.block_end);break;
  case api::ChatEventKind::End:detail::require(e.end.has_value(),"terminal");end_=e.end;break;
  default:detail::require(false,"event kind");
  }
  ++cursor;
 }
 std::optional<ChatV3Reply> reply()const{
  if(!end_){return std::nullopt;}
  ChatV3Reply r;r.result=*end_;
  if(r.result.outcome==api::ChatResultOutcome::Completed&&r.result.finish_reason&&std::find(api::kFinishReasonNames.begin(),api::kFinishReasonNames.end(),*r.result.finish_reason)==api::kFinishReasonNames.end())r.result.finish_reason="other";
  for(const auto& item:parts_){r.blocks.push_back(item.second);if(item.second.kind==api::ChatPartKind::Text)r.text+=item.second.text;}
  for(const auto& item:closed_){r.block_ends.push_back(item.second);}
  return r;
 }
private:
 bool retain_;bool partial_;std::map<std::int64_t,api::ChatPart> parts_;std::map<std::int64_t,api::ChatBlockEnd> closed_;std::optional<api::ChatResult> end_;
};
// Thin managed calls can wrap a generated client or an equivalent test port.
// The port owns transport deadlines. No destructor or wait error cancels work.
template<class Port> class ChatV3Session {
public:
 ChatV3Session(Port& port,std::string request,std::int64_t cursor=0,bool retain_reasoning=false):request(std::move(request)),cursor(cursor),fold(retain_reasoning,cursor),port_(port){}
 std::optional<api::ChatAdmission> admission;
 std::string request;std::int64_t cursor;FoldV3 fold;
 api::ChatPage next(){
  auto p=port_.observe(request,cursor,256,65536,25000);
  detail::require(p.outcome==api::PageOutcome::Page,"observe refused");
  detail::require(p.events.size()<=256&&p.next>=cursor&&p.next-cursor==static_cast<std::int64_t>(p.events.size()),"page cursor");
  for(std::size_t i=0;i<p.events.size();++i)detail::require(p.events[i].sequence==cursor+static_cast<std::int64_t>(i)&&(p.events[i].kind!=api::ChatEventKind::End||(i+1==p.events.size()&&p.at_end)),"page sequence");
  detail::require(!p.at_end||p.events.empty()||p.events.back().kind==api::ChatEventKind::End,"page terminal");
  auto next_fold=fold;for(const auto& e:p.events)next_fold.add(e);fold=std::move(next_fold);cursor=p.next;return p;
 }
 api::ChatCancellation cancel(){return port_.cancel(request);}
 ChatV3Reply complete(){while(!fold.reply()){auto p=next();detail::require(!p.at_end||fold.reply().has_value(),"missing terminal");}return *fold.reply();}
private:Port& port_;
};
template<class Port> ChatV3Session<Port> resume_chat_v3(Port& port,std::string request,std::int64_t cursor=0,bool retain_reasoning=false){return ChatV3Session<Port>(port,std::move(request),cursor,retain_reasoning);}
template<class Port> ChatV3Session<Port> open_chat_v3(Port& port,const api::RequestV3& request,bool retain_reasoning=false){auto a=port.start(request);auto s=resume_chat_v3(port,a.request,0,retain_reasoning);s.admission=a;return s;}
// Page bounds stream() uses for each observe.
inline constexpr std::int64_t kPageDeltas=256;
inline constexpr std::int64_t kPageBytes=65536;
inline constexpr std::int64_t kPageWaitMs=25000;
// Assembles a reply from deltas: a part delta extends the part at its index,
// and the end delta supplies outcome, reason, usage, host and model.
class Fold {
public:
 void add(const api::Delta& delta){
  if(delta.kind==api::DeltaKind::Part&&delta.part){
   auto found=parts_.find(delta.index);
   if(found==parts_.end()){parts_.emplace(delta.index,*delta.part);return;}
   auto& p=found->second;const auto& d=*delta.part;
   p.text+=d.text;p.arguments+=d.arguments;
   if(p.call_id.empty())p.call_id=d.call_id;
   if(p.name.empty())p.name=d.name;
   if(p.digest.empty())p.digest=d.digest;
   if(p.media_type.empty())p.media_type=d.media_type;
  }else if(delta.kind==api::DeltaKind::End&&delta.end){end_=*delta.end;}
 }
 // The folded reply, once the end delta arrived.
 std::optional<api::Reply> reply()const{
  if(!end_)return std::nullopt;
  auto r=*end_;r.message.role=api::Role::Assistant;r.message.parts.clear();
  for(const auto& entry:parts_)r.message.parts.push_back(entry.second);
  return r;
 }
private:
 std::map<std::int64_t,api::Part> parts_;
 std::optional<api::Reply> end_;
};
// Calls abstraction.inference/chat@1. complete() and stream() hide start,
// observe and cancel; start is never retried.
class Chat {
public:
 explicit Chat(std::string endpoint):endpoint_(std::move(endpoint)){}
 Chat with_server_expectation(std::optional<ipc::ServerExpectation> server)const{auto copy=*this;copy.server_=std::move(server);return copy;}
 Chat with_cancellation(ipc::CancellationToken token)const{auto copy=*this;copy.cancellation_=std::move(token);return copy;}
 Chat with_deadline(std::optional<ipc::Deadline> deadline)const{auto copy=*this;copy.deadline_=deadline;return copy;}
 api::Admission start(const api::Request& request)const{
  auto transport=this->transport(kMarginMs);api::ChatClient<ipc::FrameTransport> client(transport);auto a=client.start(request);
  const bool accepted=a.outcome==api::StartOutcome::Accepted;
  detail::require(accepted==!a.operation.empty(),"inconsistent admission");return a;
 }
 // The call's budget is the transport margin plus wait_ms.
 api::DeltaPage observe(const std::string& operation,std::int64_t cursor,std::int64_t max_deltas,std::int64_t max_bytes,std::int64_t wait_ms)const{
  if(max_deltas<1||max_deltas>256||max_bytes<1||max_bytes>65536||wait_ms<0||wait_ms>30000)throw api::ServiceError("invalid_request","max_deltas 1..256, max_bytes 1..65536, wait_ms 0..30000");
  auto transport=this->transport(kMarginMs+static_cast<std::uint32_t>(wait_ms));api::ChatClient<ipc::FrameTransport> client(transport);
  auto page=client.observe(operation,cursor,max_deltas,max_bytes,wait_ms);
  if(page.outcome==api::PageOutcome::Page){
   bool ok=static_cast<std::int64_t>(page.deltas.size())<=max_deltas&&page.next==cursor+static_cast<std::int64_t>(page.deltas.size());
   for(std::size_t i=0;i<page.deltas.size();++i)ok=ok&&page.deltas[i].sequence==cursor+static_cast<std::int64_t>(i);
   detail::require(ok,"inconsistent delta page");
  }else if(page.outcome==api::PageOutcome::Gap){detail::require(page.deltas.empty()&&page.next>cursor,"inconsistent gap");}
  else detail::require(page.deltas.empty()&&page.next==cursor&&!page.at_end,"inconsistent page refusal");
  return page;
 }
 api::Cancellation cancel(const std::string& operation)const{
  auto transport=this->transport(kMarginMs);api::ChatClient<ipc::FrameTransport> client(transport);return client.cancel(operation);
 }
 // Calls on_delta for each delta in sequence until the end delta, which
 // carries the reply. A start refusal is one end delta carrying that outcome.
 // Returning false from on_delta, or an exception, cancels the operation. A
 // gap or an observe refusal throws ServiceError with its outcome word.
 void stream(const api::Request& request,const std::function<bool(const api::Delta&)>& on_delta)const{
  const auto admission=start(request);
  if(admission.outcome!=api::StartOutcome::Accepted){
   api::Delta end;end.kind=api::DeltaKind::End;end.end=refused(admission);on_delta(end);return;
  }
  struct Guard{const Chat& chat;const std::string& operation;bool ended=false;~Guard(){if(!ended){try{chat.cancel(operation);}catch(...){}}}} guard{*this,admission.operation};
  std::int64_t cursor=0;
  for(;;){
   const auto page=observe(admission.operation,cursor,kPageDeltas,kPageBytes,kPageWaitMs);
   if(page.outcome!=api::PageOutcome::Page)throw api::ServiceError(std::string(api::wire_name(page.outcome)),"observe refused");
   for(const auto& delta:page.deltas){
    if(delta.kind==api::DeltaKind::End)guard.ended=true;
    if(!on_delta(delta))return;
   }
   cursor=page.next;
   if(page.at_end){guard.ended=true;return;}
  }
 }
 // Starts request, observes it to its end and folds the deltas into the reply.
 api::Reply complete(const api::Request& request)const{
  Fold fold;stream(request,[&fold](const api::Delta& delta){fold.add(delta);return true;});
  auto reply=fold.reply();detail::require(reply.has_value(),"stream ended without an end delta");return *reply;
 }
private:
 static constexpr std::uint32_t kMarginMs=ipc::kDefaultCallBudgetMs;
 ipc::FrameTransport transport(std::uint32_t timeout_ms)const{
  auto result=ipc::FrameTransport(endpoint_,timeout_ms,ipc::kDefaultMaxFrame).with_server_expectation(server_).with_cancellation(cancellation_);
  return deadline_?result.with_call_deadline(*deadline_):result;
 }
 static api::Reply refused(const api::Admission& admission){
  api::Reply r;r.outcome=*api::parse_reply_outcome(api::wire_name(admission.outcome));r.reason=admission.reason;
  r.stop_reason=api::StopReason::NoStop;r.message.role=api::Role::Assistant;return r;
 }
 std::string endpoint_;
 std::optional<ipc::ServerExpectation> server_;
 ipc::CancellationToken cancellation_;
 std::optional<ipc::Deadline> deadline_;
};
}
