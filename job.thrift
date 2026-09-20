namespace * abstraction.inference.job

encoding json {
 escape="minimal"
 indent="2"
 map_keys="utf8-bytes"
 numbers="integer-decimal"
 opaque="verbatim"
 terminator="newline"
 duplicate_keys="refuse"
 depth_limit="64"
}
refusal {
 1: malformed(stage="grammar")
 2: bad_string(stage="grammar")
 3: number_spelling(stage="grammar")
 4: wrong_type(stage="grammar")
 5: depth_exceeded(stage="grammar")
 6: duplicate_key(stage="grammar")
 7: duplicate_field(stage="structure")
 8: unknown_field(stage="structure")
 9: missing_field(stage="structure")
 10: bad_enum(stage="structure")
 11: trailing_bytes(stage="document")
}

const list<string> kinds = ["inference"]
const list<string> execution_guarantees = ["abstraction.inference/recoverable-upstream@1"]

enum Profile {
 1: video
 2: image_batch
}(unknown="refuse",reader="act")

enum Mode {
 1: generate
 2: edit
}(unknown="refuse",reader="act")

// The durable document keeps its own typed copy of the request guarantee
// vocabulary so a persisted job can be decoded without importing a service
// transport codec. Wire names exactly match the live inference requests. Job
// validation rejects an unsupported carried word before work is accepted.
enum RequestGuarantee {
 1: local_only(wire="abstraction.inference/local-only@1")
 2: hosted_allowed(wire="abstraction.inference/hosted-allowed@1")
}(unknown="grant",reader="validate")

struct ImageRequest {
 1: required string model
 2: required Mode mode
 3: required string prompt
 4: required string size
 5: required i64 count
 6: optional string image_digest(omit="zero")
 7: optional string image_media_type(omit="zero")
 8: optional string mask_digest(omit="zero")
 9: optional string mask_media_type(omit="zero")
 10: required list<RequestGuarantee> guarantees
 11: optional string credential(omit="zero")
 12: optional list<string> required_extensions(omit="zero")
 13: optional map<string,string> extensions(omit="zero")
}(unknown_fields="refuse",doc="The durable image-shaped request. Its fields and bounds match abstraction.inference/image@1 ImageRequest. Keeping this job document self-contained lets the job SDK persist and reconcile it without importing a service transport codec.")

struct Delivery {
 1: required string digest
 2: required string media_type
 3: required i64 size
 4: required string location
}(unknown_fields="refuse",doc="A typed result reference with the same meaning as abstraction.inference/image@1 Delivery.")

struct Request {
 1: required Profile profile
 2: required ImageRequest image
 3: optional i64 duration_ms(omit="zero")
}(unknown_fields="refuse",doc="Durable inference work submitted as job kind inference. video carries an image-shaped generate request and duration_ms of 1..600000; image_batch carries duration zero and count 1..10. The embedded request retains its digest, guarantee, credential and extension bounds. The recoverable-upstream guarantee means the executor durably marks submission before the paid call, records a returned provider handle, resumes a known handle, and invokes the adapter's reconciliation path after an uncertain submit. It does not promise eventual resolution: an adapter such as Replicate that offers no lookup by caller operation keeps the job waiting as upstream:uncertain and never blindly submits it again. No URL, path, header, credential bytes or inline media crosses this value.")

struct JobResult {
 1: required Profile profile
 2: required list<Delivery> deliveries
 3: required string host
 4: required string model
}(unknown_fields="refuse",doc="The immutable successful result returned by the job operation. deliveries are ordered typed references committed for the original caller. Local references are canonical SHA-256 digests readable through that caller's authorized content reader; remote locations state their owning trust domain.")

struct Document {
 1: optional Request request(omit="absent")
 2: optional JobResult result(omit="absent")
}(document="true",unknown_fields="refuse",doc="Exactly one of request or result is present. Request documents are opaque job submission spec bytes. Result documents are immutable operation result bytes read through abstraction.job/operation@1. The job layer does not parse either variant.")
