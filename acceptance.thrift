namespace * abstraction.job.acceptance

// Additive service vocabulary; job.thrift's tagged Record is unchanged.
encoding json {
  escape = "minimal"
  indent = "2"
  map_keys = "utf8-bytes"
  numbers = "integer-decimal"
  opaque = "verbatim"
  terminator = "newline"
  duplicate_keys = "refuse"
  depth_limit = "64"
}
refusal {
  1: malformed (stage = "grammar")
  2: bad_string (stage = "grammar")
  3: number_spelling (stage = "grammar")
  4: wrong_type (stage = "grammar")
  5: depth_exceeded (stage = "grammar")
  6: duplicate_key (stage = "grammar")
  7: duplicate_field (stage = "structure")
  8: unknown_field (stage = "structure")
  9: missing_field (stage = "structure")
 10: bad_binary (stage = "structure")
 11: bad_enum (stage = "structure")
 12: trailing_bytes (stage = "document")
}
struct RequestIdentity {
  1: required string key
  2: required string history_epoch
} (unknown_fields = "refuse", doc="Stable SDK key in an owner-issued history epoch. Scope is authenticated caller plus this service contract, never a caller-provided principal. Persist before send for restart recovery.")
struct Submission {
  1: required RequestIdentity identity
  2: required string kind
  3: required binary spec
  4: required list<string> required_guarantees
} (unknown_fields = "refuse", doc="Opaque kind-specific specification bytes, not a second tagged job Record. Equality includes kind, exact spec bytes and the set of required guarantees. Credentials are supplied at the authorized service boundary.")
struct Receipt {
  1: required RequestIdentity identity
  2: required string logical_owner
  3: required string operation_id
  4: required list<string> accepted_guarantees
  5: required i64 history_retention_ms
} (unknown_fields = "refuse", doc="Recoverable acceptance evidence. Retention is a minimum duration from original acceptance, never renewed by replay. Expiry does not end work, transfer ownership or authorize duplicate execution. IDs confer no authority.")
enum Outcome {
  1: accepted
  2: definitely_not_accepted
  3: unknown
  4: key_conflict
  5: forbidden
  6: invalid
} (unknown = "refuse")
struct AcceptanceResult {
  1: required Outcome outcome
  2: optional Receipt receipt (omit = "absent")
  3: required string reason
} (document = "true", unknown_fields = "refuse", doc="Accepted requires a receipt; other outcomes forbid one. Definite nonacceptance requires authoritative sealed evidence preventing any delayed acceptance of this identity. Absence, timeout, expired history and access denial are insufficient.")
struct HistoryWindow {
  1: required string logical_owner
  2: required string history_epoch
  3: required i64 minimum_retention_ms
} (unknown_fields = "refuse", doc="Owner-issued acceptance epoch and minimum reconciliation retention. After closing an epoch the owner fences all its submissions, including delayed ones. This does not assert that old unknown work was never accepted.")
enum CancellationOutcome {
  1: requested
  2: already_terminal
  3: unknown
  4: forbidden
  5: unsupported
} (unknown = "refuse")
struct CancellationResult {
  1: required CancellationOutcome outcome
} (unknown_fields = "refuse", doc="Requested acknowledges cancellation intent, not stopped effects. Completion may win the race; observe the existing operation for its terminal result.")
service RecoverableAcceptance {
  HistoryWindow GetHistoryWindow() (doc="Obtain the logical owner and history epoch before first submission; creates no work.")
  AcceptanceResult Submit(1: Submission submission) (doc="Atomically associate authenticated request identity, arguments, operation and guarantees before acknowledging. Duplicate equal arguments recover the original receipt. No weaker provider fallback on unknown.")
  AcceptanceResult Reconcile(1: RequestIdentity identity) (doc="Authorized lookup at the original logical owner after lost request, lost reply or caller restart. A definite negative seals the identity against delayed submissions. Unavailable evidence gives unknown.")
  CancellationResult CancelWork(1: RequestIdentity identity) (doc="Authorized explicit work cancellation. Cancelling a transport wait is never this operation and never relinquishes accepted work ownership.")
} (wire_name = "abstraction.job/acceptance@1", doc="Version 1 recoverable acceptance vocabulary, local or remote. Providers must implement atomic recovery and downstream deduplication before advertising those guarantees. Legacy Store is not implicitly upgraded.")
