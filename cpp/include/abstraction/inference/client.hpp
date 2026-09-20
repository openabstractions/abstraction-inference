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
 static constexpr std::uint32_t kMarginMs=5000;
 ipc::FrameTransport transport(std::uint32_t timeout_ms)const{
  return ipc::FrameTransport(endpoint_,timeout_ms,1u<<20).with_server_expectation(server_).with_cancellation(cancellation_);
 }
 static api::Reply refused(const api::Admission& admission){
  api::Reply r;r.outcome=*api::parse_reply_outcome(api::wire_name(admission.outcome));r.reason=admission.reason;
  r.stop_reason=api::StopReason::NoStop;r.message.role=api::Role::Assistant;return r;
 }
 std::string endpoint_;
 std::optional<ipc::ServerExpectation> server_;
 ipc::CancellationToken cancellation_;
};
}
