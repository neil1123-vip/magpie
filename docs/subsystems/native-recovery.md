# Native compaction and local recovery

Native compaction still sends the vendor's encrypted checkpoint to its writer.
An optional, machine-local recovery copy records the readable history replaced
by an explicit Responses `compaction_trigger`. It makes no second model call.
Replaying history can increase input tokens; it cannot restore private reasoning.

## Sources of truth

- [`recoverystore`](../../internal/recoverystore/store.go) owns local configuration,
  immutable compressed records, capacity reservations and branch views in the
  private `compaction-recovery` application directory. Configuration is separate
  from provider settings and is not included in setup sync or backups.
- [`native_recovery.go`](../../internal/gateway/native_recovery.go) wraps routed
  Responses requests and the Codex backend. It materializes checkpoint sources,
  gates publication and reruns routing for restored requests.
- [`native_recovery_input.go`](../../internal/gateway/native_recovery_input.go)
  validates readable input, matches the complete output window and makes local
  excerpts from complete interaction blocks.
- [`native_recovery_choices.go`](../../internal/gateway/native_recovery_choices.go)
  keeps bounded, expiring, one-use confirmations without request text or credentials.

## Capture and publication

The actual candidate's local setting determines whether capture is enabled.
Before an upstream compaction, the store reserves bounded input/output space,
registers an operating-system-locked pending operation and persists its input.
Successful compaction output is buffered until a complete successful terminal
response and its complete output window are validated and committed. Failed
streams, missing parents, storage errors and invalidated operations do not publish
a replacement checkpoint. Ordinary answers stream without a whole-response buffer.

Each checkpoint contains an independent materialized input. Parent records are
protected while reading, then their versions are checked before sending and
committing. New checkpoints inherit the actual adopted view's loss information;
they never expand the history discarded by an earlier excerpt. The whole returned
window is replaced on recovery, including any original messages retained by the
compactor. Matching must be unique; message IDs/status and equivalent text-part
spelling may be normalized, but tool call IDs and arguments are preserved.
Compaction matching also ignores only `metadata.turn_id`, which Codex removes
on replay; checkpoint IDs, ciphertext and unknown metadata remain binding.
Codex can retain an original prefix outside the returned compaction window.
Shared nonempty user/assistant message IDs must establish one alignment, and
the entire contiguous prefix must match the saved source before it is replaced.
An ID in a retained message inside the matched output window can establish
that alignment as well.
Changed, duplicate or conflicting anchors refuse recovery. Equal text without
matching IDs does not authorize removing messages from the current request.
The matched client prefix becomes part of the stored replacement window;
`PrefixItems` records exactly how many leading output items came from that
verified prefix. Old records default to zero. A local excerpt matches using
the full source before replacing it with the selected suffix, so omitted
prefix messages cannot return on the next request. The proof remains usable
after the original checkpoint is deleted. The prefix boundary is fixed before
the storage operation begins, survives restart, and cannot include a seal.

Records and reads are isolated by authenticated gateway key, or by verified local
requests. A ciphertext hash alone is not authority to read a record. Source input
is saved before target masking; recovery uses current instructions, tools,
permissions, masking and routing. Incomplete tool exchanges, remote media/file
references and unresolved response IDs are refused as recovery sources.
Responses `additional_tools` declarations are validated but excluded from the
saved source. The client's current declarations remain in restored requests and
their budget, so old declarations cannot restore withdrawn tools. Historical
tool-search results remain part of their original call/result exchange.

## Recovery and capacity

An unavailable writer, a route that excludes it, or an explicit encrypted-content
refusal before any answer is published can trigger restoration. Ordinary business
errors and responses already sent do not trigger replay. A restored Codex direct
request rejoins normal gateway routing. Pins, strict affinity, disabled models,
key restrictions and quotas still apply.

Reliable branch identity prefers the client's actual thread metadata over shared
session headers. Invalid, truncated, placeholder and conflicting identities do
not create shared mutable state. A branch that adopts a restored view continues
using it even if the original writer returns. Without reliable identity, only
stateless full replay is permitted; a later request can use the original writer
again. No new local excerpt is offered in that case.

Restored requests require a known context window on every attempted candidate,
including retries. The conservative budget counts the serialized request at one
UTF-8 byte per token, a 4,096-token safety allowance and reserved output. Missing
output limits become 8,192 tokens in the restored request. This can reject text
that a particular tokenizer would fit. Inline PNG, JPEG and WebP images can be
replayed on Responses endpoints with a verified model-specific image budget.
Base64 image payloads are excluded from the text budget; image tokens are added
separately. Files, audio, GIF, external references and unknown image-token rules
still refuse automatic replay. Provider/model context settings remain the only
window configuration.
The final wire check uses the actual protocol's output field. If an account
removes that field, the model's catalog output capacity is reserved instead;
unknown capacity refuses replay. A user-set soft output limit is not treated as
an enforced cap after the backend removes it.

[`native_recovery_media.go`](../../internal/gateway/native_recovery_media.go)
checks base64, MIME type, decoded image validity and dimensions.
URL-safe and unpadded base64 use the gateway's standard padded encoding for
validation and image matching, without rewriting the saved request or dropping
image detail or history positions. Local limits are
128 images and 32 MiB per request, 20 MiB encoded-file bytes per image, and 64 MiB
of decoded pixels per image. Count and request-size checks precede decoding;
a bounded digest/config cache avoids decoding the same image on every budget
check. Image bytes are never kept in that cache. Target image-capability overrides
and known upstream refusals still apply.
Image positions account for the current target model's configured system prompt,
which may insert a developer message. Each fallback computes its own expected
positions from canonical history; the target prompt is not saved into that history.
Extra messages injected by middleware or request overrides still fail this check.
The shared `imageConfig` reader checks gray PNG transparency before budgeting:
an 8/16-bit gray image with `tRNS` decodes to 4/8-byte pixels, respectively.
Opaque gray images retain their smaller pixel budget. Codex and Claude image
fitting use the same pre-decode check.

The explicit model table in
[`native_recovery_image_tokens.go`](../../internal/gateway/native_recovery_image_tokens.go)
uses the OpenAI images/vision sizing and token tables checked on 2026-11-09. It
covers GPT-6 Astra, GPT-5.6 Sol/Terra/Luna, GPT-5.5, GPT-5.4/mini/nano, GPT-5.2,
GPT-4.1/mini, GPT-4o/mini and GPT-5.1 under their exact listed model IDs. Unlisted
variants and dated aliases remain unsupported until their rules are verified.
Model names are resolved through the provider's upstream mapping;
a later change of wire model refuses replay because its context was not checked.

Exact IDs `gpt-6-sol` and `gpt-6.1-sol` additionally use an empirical profile
checked through real Responses generations on 2026-11-09. It is not an official
image-budget contract. Both use 32x32 patches and `ceil(patches * 1.2) + 2`:
low scales the longest side to at most 512 and rounds the other dimension up;
high bounds the patch count at 2500; original, auto and omitted detail bound it
at 30000. The estimator does not change the image or the transmitted detail.
Unlike a rejection rule, the latter cap budgets upstream downscaling for larger
images. Validation still independently enforces memory, dimensions and bytes.

The 41-call matrix produced 86 successful image attributions within this bound.
Both models reported 3002 tokens at 1600x1600/high and 36002 at
6400x4800/original; larger raw patch counts could complete with fewer tokens.
PNG/JPEG/WebP and user/tool image placement were exercised. Two isolated large
samples failed: 6.1 Sol original 3200x9601, and 6 Sol 9601x3200 with omitted
detail. Large combined requests also failed. These generic refusals do not
establish a fixed dimension limit; finite successful samples do not prove the
budget or acceptance behavior for every channel. Context-window settings do
not determine this image profile.

Recovery does not describe images through another model, remove them, resize
them, lower their detail or upload them into external file references. The final
wire check verifies each image's content, detail, message position and tool-call
association after normal transport cleanup. Responses-to-Chat translation is
not used for image recovery because it moves tool images into user messages.
Image positions ignore tool-declaration items lifted out by normal Responses
cleanup; moving an image relative to actual history still fails validation.
Known request-size and image-count caps (including the user's `MaxImages`) and
upstream refusals naming those limits try another allowed candidate without
deleting images or retrying the same candidate with fewer images.
A held HTTP 400 or equivalent SSE failure from either
exact Sol model also tries the next allowed candidate for an image recovery when
the structured error has type `invalid_request_error`, code `invalid_request`,
no named param, and message `The request could not be processed. Please check the
request parameters.` This applies only before any answer is published and when
error passthrough is off. It does not retry the same candidate, change the input,
learn an image restriction or rest the provider. Other models/errors and the
last candidate retain normal error handling.
The branch view is saved only after the final local
checks pass, immediately before sending; a locally rejected target does not
prevent a later native continuation.
Saving a view returns exactly the committed version. Its version and all other
parents are checked after validation, so a concurrent excerpt cannot lend its
new version to a request carrying older history.

When no allowed candidate fits, the client gets a capacity error. A reliable
branch can explicitly approve a local excerpt of recent complete interactions;
it is persisted before use. Updating an existing view uses compare-and-swap so
concurrent requests cannot restore a discarded version. The latest complete
interaction must fit; there is no model-generated backup summary.
Subsequent native checkpoints carry that marked prefix in their local matching
window, without changing the response published to the client. A lossy checkpoint
with such a prefix must use local replay on every continuation, including a new
or unidentified branch and a writer that is available again. This prevents the
client's old prefix from undoing the excerpt. The complete local output window
is still limited to 8 MiB. Multiple inherited prefixes are refused; a client
that later removes or changes the required window must provide its original
history instead of relying on automatic recovery. Explicitly disabling local
recovery withdraws this protection; probing a disabled copy does not block an
otherwise native continuation that has not adopted a branch view.
Image excerpts use the same model-specific budget and retain complete tool
exchanges. Among measurable candidates, the proposed excerpt keeps the most
history, rather than simply using the candidate with the largest window.

## Local controls and lifecycle

Provider editing exposes the subordinate recovery-copy switch. Management is
available through `/api/compaction-recovery` and `magpie compaction-recovery`.
Users can inspect storage, change its limit, remove selected records, clear it,
and approve pending operations. The default limit is 512 MiB, with 32 MiB input
and 8 MiB output bounds per checkpoint. Capacity exhaustion fails before calling
the compactor; it never silently removes existing copies to make room.

The one-request `native-only` exception needs explicit confirmation. It is bound
to caller, branch, exact request fingerprint, storage generation and a 30-minute
expiry, and is consumed once. Without a reliable branch, the confirmed request
must also carry its `X-Magpie-Recovery-Token`; the token is never forwarded.
Clear/disable/configuration changes invalidate pending work and old confirmations.
Multiple processes share file locks; crash cleanup checks operation-lock liveness
instead of assuming an old timestamp means the operation ended. Windows directories
use a protected ACL; other systems use private directory/file modes.
Provider IDs cannot change while either ID has recovery enabled or stored copies;
display names can still change. Enabling recovery rechecks the current provider
under the storage lock with fresh caches. Add uses the server's actual saved ID.

Response headers and recent request details distinguish `native-backed`,
`native-only`, `restored`, `replayed-this-request`, `excerpt` and `unavailable`.
These describe that request's outcome, not a promise that a copy still exists
after later deletion. The management list reports current stored records.

Only explicit `compaction_trigger` on routed Responses and the Codex backend is
covered. Legacy `/responses/compact` and vendor automatic compaction do not gain
a recovery guarantee. Old ciphertext without an exact local source cannot be
retrofitted into a complete copy.

## Verification

`go test -tags nogui ./internal/recoverystore` covers storage faults, concurrent
processes, immutable records and branch views. Gateway and control checks use
`go test -tags nogui ./internal/gateway ./internal/gui ./internal/cli -run
'NativeRecovery|NativeCompaction|CodexDirect|CompactionRecovery'` for branch/caller
isolation, retained output windows, capacity checks, excerpts and direct routing
with fake upstreams.
The `NativeRecoveryImage` and `NativeRecoveryMedia` tests cover decoded-image
validation, payload versus token accounting, model/detail limits, final wire
checks, incompatible candidates, byte caps, full image replay, excerpts,
restarts and subsequent compaction. They use generated PNG fixtures and local
fake upstreams, not real vendor calls.
Additional regressions cover preparation keepalives and native seal preservation
after the legacy writer cache expires, including learned protocol refusals.
Sol regressions cover the low-detail fractional-edge ceiling, empirical token
margins, above-cap grayscale originals that fit the decoded-memory limit, exact
model IDs and the scoped refusal fallback. Passthrough, already-published output
and other models do not take that fallback; later requests retain image support.
On 2026-11-09 an isolated Front harness also verified real Sol compaction followed
by Astra recovery across two configured channels: both original message/tool
images reached the target intact and their distinct visual codes were read
correctly. It made one request per channel. This verifies that route and sample,
not Sol's undocumented image budget or actual Codex client rollout behavior.
A subsequent isolated run used one `mdkj/gpt-6.1-sol` compaction, disabled that
writer locally, and restored the same checkpoint on separate branches through
`biu/gpt-6-sol` and `biu/gpt-6.1-sol`. All three requests completed. Both readers
received the exact original message/tool images, details and call association,
without the former seal, and read `user=731;tool=842`. Real provider configuration
was unchanged. This sample verifies Sol-to-Sol recovery, not universal image
acceptance or billing; the scoped refusal fallback is covered with fake upstreams.
A real Codex CLI 0.166.0 app-server session was then checked with a user image
and an image emitted from its code-mode dynamic tool. After native compaction
on `mdkj/gpt-6.1-sol`, the isolated writer was disabled. The same thread restored
through `biu/gpt-6-sol`, `biu/gpt-6.1-sol`, and again after both client and gateway
processes restarted. All three requests reported `restored`, retained both
original images/details/tool association exactly once, and read both visual
codes correctly. The checkpoint and branch view survived restart; hashes of
real provider, Codex configuration, auth and catalog files were unchanged.
This used the actual `Front` gateway with isolated storage and real Codex
app-server processes, not desktop UI clicks or the release `serve` lifecycle.
`TestNativeRecoveryClient*` and the current-tool tests cover the observed client
normalizations, identity-bound prefix matching and its rejection cases.
The transparent-gray PNG checks cover both bit depths, reject oversized decoded
images before pixel allocation, and retain the large opaque-gray positive case.
The GUI tests are `internal/gui/tests/compaction-recovery.test.cjs`; they exercise
the controls across languages and viewport sizes. No real-account fidelity or
actual-client rollout adoption experiment is implied by these automated tests.

Commit-time audit regressions additionally cover retained-prefix excerpts,
independent view reload, later compaction, fork/anonymous continuation,
upstream messages that are not client-prefix evidence, and the stored-window
size limit. These checks use local fake upstreams; the prior real-client run
does not claim real-vendor coverage of every excerpt shape.
