#pragma once
#include <abstraction/inference/api/rec.h>
#include <abstraction/ipc/frame.hpp>
#include <cstdint>
#include <optional>
#include <string>
namespace abstraction::inference {
// Calls abstraction.inference/operator@1: the runtime's hosts, the gateway
// window's local keys and the inference audit. The runtime decides
// host.manage, key.issue or audit.read for this program on each call.
class Operator {
public:
 explicit Operator(std::string endpoint):endpoint_(std::move(endpoint)){}
 Operator with_server_expectation(std::optional<ipc::ServerExpectation> server)const{auto copy=*this;copy.server_=std::move(server);return copy;}
 Operator with_cancellation(ipc::CancellationToken token)const{auto copy=*this;copy.cancellation_=std::move(token);return copy;}
 api::HostList hosts()const{auto t=transport();api::OperatorClient<ipc::FrameTransport> c(t);return c.hosts();}
 api::HostChange add_host(const std::string& expected_revision,const api::HostEntry& host)const{auto t=transport();api::OperatorClient<ipc::FrameTransport> c(t);return c.add_host(expected_revision,host);}
 api::HostChange remove_host(const std::string& expected_revision,const std::string& name)const{auto t=transport();api::OperatorClient<ipc::FrameTransport> c(t);return c.remove_host(expected_revision,name);}
 api::KeyList keys()const{auto t=transport();api::OperatorClient<ipc::FrameTransport> c(t);return c.keys();}
 // The reply carries the key once; nothing reads it back later.
 api::KeyIssued issue_key(const std::string& program,const std::string& credential)const{auto t=transport();api::OperatorClient<ipc::FrameTransport> c(t);return c.issue_key(program,credential);}
 api::KeyRevoked revoke_key(const std::string& program)const{auto t=transport();api::OperatorClient<ipc::FrameTransport> c(t);return c.revoke_key(program);}
 // One page of retained decisions from sequence cursor; max_entries is 1..256.
 api::AuditPage audit(std::int64_t cursor,std::int64_t max_entries)const{
  if(max_entries<1||max_entries>256||cursor<0)throw api::ServiceError("invalid_request","cursor >= 0 and max_entries 1..256");
  auto t=transport();api::OperatorClient<ipc::FrameTransport> c(t);auto page=c.audit(cursor,max_entries);
  if(page.outcome==api::AuditOutcome::Page){
   bool ok=static_cast<std::int64_t>(page.entries.size())<=max_entries&&page.next==cursor+static_cast<std::int64_t>(page.entries.size());
   for(std::size_t i=0;i<page.entries.size();++i)ok=ok&&page.entries[i].sequence==cursor+static_cast<std::int64_t>(i);
   if(!ok)throw api::ServiceError("invalid_response","inconsistent audit page");
  }
  return page;
 }
private:
 static constexpr std::uint32_t kTimeoutMs=10000;
 ipc::FrameTransport transport()const{
  return ipc::FrameTransport(endpoint_,kTimeoutMs,1u<<20).with_server_expectation(server_).with_cancellation(cancellation_);
 }
 std::string endpoint_;
 std::optional<ipc::ServerExpectation> server_;
 ipc::CancellationToken cancellation_;
};
}
