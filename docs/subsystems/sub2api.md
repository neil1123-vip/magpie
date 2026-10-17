# Sub2API accounts and group rates

Sub2API panel sign-in attaches to a provider's existing balance settings.
Usage has a Sub2API groups tab for account recovery, all available groups, rate
changes and automatic local key-price synchronization. The gateway owns polling,
so closing the page does not stop it. Windows desktop permits changes; `magpie web`
and other platforms expose safe, read-only state.

The tab initially shows groups with linked keys and unidentified local keys;
All groups also shows groups without a local key. Site and change filters affect only this view,
not background polling. Source names appear beside the panel address. Each key
shows its billing rate, effective local rate and synchronization state.
Each group shows the panel's cache rate, availability and time to first token
once, shared by its local keys. Missing metrics remain unknown; older
readings retain their original time and scope.
Each account highlights its panel address and source names, with identity and
check status in secondary text. Each group has a wrapping header with its
enabled state, declared rate and shared observation metrics; its keys follow
in aligned Key, billing-rate and local-rate columns grouped on the left.
The three observation columns have stable widths across groups. Narrow screens
keep each key immediately above its two rates. A historical metric shows a
neutral dash and a keyboard-accessible history action that opens its dated
reading in the observation details. Expanded details retain exact timestamps
and provenance in a bounded label/value layout. Account summaries distinguish
linked groups from the total; polling progress is shared above the accounts,
while disabled or stopped monitoring remains visible on the affected account.
Current metric values use colored semibold numerals without badge backgrounds.
Cache-rate green/yellow/orange/red thresholds are 85%, 70% and 55%;
availability thresholds are 99%, 95% and 90%, with inclusive lower bounds.
First-token thresholds are 10, 20 and 30 seconds, with inclusive upper bounds.
These colors are display hints for panel statistics, not routing or polling rules.
Unknown metrics show a neutral dash; dated historical readings remain neutral.
Account sign-out is in the actions menu.
Recent changes are displayed newest first by their recorded timestamp, preserving
the order of equal timestamps and placing missing or invalid timestamps last.
This display order does not change stored history or background polling.
The recent-changes section starts collapsed and retains the reader's expanded
or collapsed state when data refreshes or the view redraws.
Visibility changes show the group name once; rate and status changes
retain their before and after values. Rename changes show the recorded old name
and new name joined by an arrow, without repeating the new name as a label.
New visible-group records also capture and
display the declared rate from that round, including zero. Older records without
that field and groups without a known rate show an unconfirmed rate; the current
group rate never fills in a historical record.
The UI calls the declared rate "Group rate" and the per-key rate "Key billing
rate". Within one account's recorded round, key-rate changes sharing a
nonempty group ID and the same known before/after rates as a group-rate
change share that group's history row, with the matching key count. Other
key-rate changes stay separate and show the source and key name (or saved
IDs when no longer configured). Rate transitions show multiplier units and
an arrow. Grouping changes presentation only: stored changes, independent
baselines, polling and notifications remain unchanged.

## Responsibilities and sources of truth

| Part | Responsibility | Source |
| --- | --- | --- |
| Protocol and session | Panel authentication, 2FA, identity, renewal, local credentials and account generations | [`protocol.go`](../../internal/sub2api/protocol.go), [`session.go`](../../internal/sub2api/session.go), [`store.go`](../../internal/sub2api/store.go) |
| Monitor | Settings, group and key-rate readings, baselines, cooldowns, change records and notification eligibility | [`poll.go`](../../internal/sub2api/poll.go), [`monitor_poll.go`](../../internal/sub2api/monitor_poll.go), [`monitor_control.go`](../../internal/sub2api/monitor_control.go) |
| Provider adapter | Existing balance requests, actual key groups, network context and local price commits | [`sub2api.go`](../../internal/provider/sub2api.go), [`sub2api_sync.go`](../../internal/provider/sub2api_sync.go) |
| API and UI | Safe DTOs, desktop write capability, source editor and Usage tab | [`sub2api.go`](../../internal/gui/sub2api.go), [`assets/sub2api/`](../../internal/gui/assets/sub2api/) |
| Browser host | Short-lived WebView2 process, native restrictions, anonymous pipes and profile cleanup | [`sub2apibrowser/`](../../internal/sub2apibrowser/), [`browser.go`](../../internal/sub2api/browser.go) |
| Wails preparation | Pinned, opt-in Windows hooks in a verified private module copy | [`build/sub2api-webview2/`](../../build/sub2api-webview2/) |

The core imports neither provider nor GUI. Gateway startup calls
`provider.KeepSub2API`; the adapter supplies current targets and the account
generation guard. Existing provider and GUI files keep the integration
points; feature code lives in the files above.

`providers.json` holds portable references: session id, the last verified
panel group per key fingerprint and a binding version. Synchronized key prices
live separately in `KeyPriceRates`, so unlinking or signing out of a panel does
not change local prices. Removed or replaced keys lose their own entries.
Old source-wide group selections are read as historical selections per key
until the next successful identification replaces them. Credentials,
account metadata, monitor settings, snapshots and baselines live under
`Config()/sub2api-local/`. Access and refresh tokens and an optionally
remembered password use current-user Windows DPAPI. GUI responses contain
no credentials. Local state and browser profiles never enter a portable
backup; see [sync and backup](sync-backup.md).

## Runtime and state

1. Panel sign-in is offered after saving the source; an unsaved source
   retains the existing balance-token editor. In a saved source's balance
   editor, password sign-in uses Go HTTP; a six-digit
   2FA challenge stays in the same flow. Sign-in and balance binding are
   separate results. A failed binding retains the signed-in account for
   recovery and keeps the source's old balance configuration. A successful
   binding refreshes an unchanged open source editor, including its balance
   fields and save revision. Edits made while the operation was pending
   remain intact and retain their original revision for conflict detection.
   The panel keeps its password and verification inputs across form refreshes;
   responses received before a delayed remount still complete the same attempt.
   Asynchronous remounts restore the editor's scroll position after all fields
   have returned; later model-list folding measurements preserve it too.
   Closing the editor, including a successful Save or Remove, cancels its
   pending sign-in. Password and 2FA requests honor cancellation before
   committing credentials; a returned 2FA attempt stays valid between requests.
   Completion callbacks may close only the editor or account form that started
   them, including when the same source is reopened. Account recovery stays
   cancellable through its final refresh; leaving the page or opening a new form
   prevents an older callback from closing that form or reporting success.
   A successful recovery adopts its returned account generation before that
   refresh. An older displayed snapshot does not cancel it; another login
   or logout with a newer generation still invalidates the old form. A site
   filter may temporarily detach that form without canceling its request:
   an owned successful result still closes it, and a returned 2FA challenge
   is redrawn when the account is shown again. A failed request also restores
   that owned form's button so it can be retried after the filter changes.
2. Binding checks identity and reads the actual configured balance request.
   URL, GET/POST method, parsing path, headers and proxy are preserved.
   A legacy balance token is removed only after the new binding succeeds.
   Successful sign-in or account association then queries the account's keys
   and synchronizes local groups and prices. Full API key values are matched
   exactly against `/api/v1/keys`; names, masked values and group rates are
   never used to guess a match. A server-capped page size is adopted from the
   first response; page size, total, page counts and item counts must stay
   consistent through later pages. Kedaya-style `group_ids` with one group
   identifies that group. Multiple routing groups cannot be represented by
   one local key multiplier: those keys report a group error and keep their
   existing local group and price, without blocking single-group keys.
   The source editor and Usage show the actual
   group without a manual group selector. This never changes membership on
   the panel.
   Keys from a source already linked to another account must be unlinked
   before that source can use the new account.
   Binding and synchronization recheck request cancellation after acquiring
   their final local locks and reading the source, before accepting a write.
   Conflicting authentication headers are rejected before sending a password.
   Linking the already associated account is disabled, preserving the chosen
   group and keys. The source editor offers a one-time group query without
   enabling monitoring. Its result stays in that editor, shows group errors,
   and is discarded on close or an account generation or settings change.
   Key synchronization requires the source's first configured API URL
   to match the account's origin. Balance-account association instead checks
   the configured balance URL, otherwise its first configured API URL.
   Its signed-in account picker requires both that source URL and the
   current panel-address field to match the account's origin. Editing the
   panel address refreshes only the picker, closes its old menu and clears
   an ineligible selection while preserving entered sign-in fields.
   Scheme, host and effective port must match; a name or domain suffix is
   not authority. Saving a source at a different API site rejects retained
   historical key groups. The billing reader also checks the API site,
   including sources restored or imported without going through the editor.
   Browser validation and binding enforce this before deriving a default
   balance URL, so another site's source cannot be silently reassigned.
3. Monitoring starts only after an explicit settings write. Enabling an account
   monitors every group returned by its available-groups API, including groups
   without a linked key. The account switch is the only monitoring selector;
   legacy watched-group lists remain readable but do not restrict sampling,
   field baselines or notifications. An account with no linked source or key
   can still monitor its groups. The runner waits
   one minute after each scheduled round. Changed enabled settings, successful
   sign-in and confirmed bindings wake the affected account; pending wakes
   merge and do not move the schedule for other accounts. Scheduled rounds
   sample every enabled account without using `NextPoll` as an account gate.
   Each round makes one available-groups request per account, reads its key
   list with validated pagination and reads every active local key's billing rate;
   request time and resource cooldowns can extend the interval. Both scheduled
   rounds and one-time queries also read the panel's V1/V2 observations.
   V2 uses one group matrix per account, falling back to group snapshots;
   repeated group IDs across platforms make only that group's matrix reading
   ambiguous, leaving other groups usable. A snapshot without an echoed group
   identity remains account-level even when the request includes a group filter.
   V1 shares one channel-monitor response across groups. The selected panel
   mode is cached. A valid V1 channel list may omit an available group;
   that group has no current observation, without failing the query or key
   synchronization. Ambiguous matches and invalid responses remain errors.
   Cache rate, availability and time to first token are panel
   statistics, not local gateway measurements. The first-token reading prefers
   p50; an older response's trimmed mean remains supported and is labeled
   separately in the details. Probe latency is
   distinct from time to first token. Global observations remain account-level
   and are never copied into a group. Each field retains its last valid value,
   timestamp, source, scope and statistics window independently. Observation
   failures and server cooldowns do not stop key billing or price synchronization.
   Panel and key
   failures have separate cooldowns; a panel challenge does not stop an
   independently valid key. Invalid source headers or proxy settings fail
   that source's key readings without stopping other accounts. A newer
   explicit sign-in releases the previous
   generation's panel authentication backoff, preserving field baselines,
   key cooldowns, HTTP 429 and server `Retry-After`. The local default delay
   saved after a failed renewal is not reported as server `Retry-After`.
   A round that cannot acquire sampling ownership returns without reloading
   shared state, so it cannot erase the active round's uncommitted cooldowns.
4. First successful readings establish baselines. All groups retain rate and
   enabled-state baselines independently of local key bindings. Later known changes are
   recorded and combined into one notification per account and round.
   Failed or partial reads retain dated history rather than implying a
   zero rate or a removed group. Disabled keys retain their baselines but
   no longer keep pending billing-verification actions, so a later panel
   verification requirement can produce a new notification. Reenabling an
   unchanged key restores its dated history even if its first read fails;
   that failed reading remains unknown and cannot replace the local price.
   Changed identities rebuild only the affected baseline. An unknown key
   network context retains that key's last identified baseline even when
   the panel account's context is known.
5. With monitoring off, Preview performs one sampling round and synchronizes local key
   groups and prices without updating monitor
   settings, snapshots, baselines or notifications. Credential rotation and
   request cooldowns still apply, including persisted server `Retry-After`
   on responses other than HTTP 429. The page accepts only the current
   account query's result; leaving the page or starting another query
   cancels the previous request and invalidates older completions and their error
   feedback. A partial query's resource errors remain visible alongside its
   successful groups and keys; its summary error does not discard the result.
   A local write already accepted before cancellation is not rolled back.
   With monitoring enabled, a newer successful sample under the same account
   generation and settings replaces the displayed preview, including a
   successful key in a partial round. Refreshing shared state or recording a
   failed attempt alone does not replace it.
6. A successful query or background round automatically changes only each
   verified active key's group and that fingerprint's `KeyPriceRates` entry.
   A missing, masked or ambiguous key match preserves its old group and price;
   a failed or invalid billing read preserves its old price. The UI identifies
   historical groups separately from verified current groups. Zero, negative,
   out-of-range or overly precise billing rates are not written as local prices.
   One key's error does not block a valid sibling. The final local commit holds
   the account generation and monitor settings guard, rechecks source revisions,
   API site and request cancellation, and uses the provider file's revision check.
   Changed targets are adopted by the same monitor ticket before its snapshot
   or notification is committed. Unchanged values cause no provider-file write.
   The legacy manual binding and apply endpoints remain for compatibility;
   the current GUI does not use them.
   Monitoring and synchronization use the same billing parser, including numeric
   strings, field priority and duplicate-field interpretation.
   The declared group rate comes from `/api/v1/groups/available`, preferring
   `group_rate_multiplier`, then `rate_multiplier`. The key
   billing rate comes from `/v1/sub2api/billing`, preferring
   `effective_rate_multiplier`, then `resolved_rate_multiplier`, then
   `group_rate_multiplier`; it is reported by the site, not measured from a
   model request's token cost. The local applied rate is the key's saved
   override, otherwise the source's `PriceRate` (default 1). Estimates for
   recorded requests, request tables and budget settlement use the actual
   `ProviderKeyID`; their price caches include that fingerprint. Estimates
   without an actual key, such as model lists and agent-only sessions,
   retain the source multiplier. Provider rate windows additionally multiply
   each recorded call at its own time, including explicit model prices; recorded
   actual costs remain unchanged. Model lists use the current window, while
   agent configuration prices leave windows out. Historical estimates follow the current local multiplier
   as before; synchronization may therefore change previously displayed estimates.

GUI reads share a pending request until a write request finishes. After that
point, the older read cannot update shared state, and a new read obtains the
current settings and account generations. Writes and conflict recovery do
not reuse a request started before the write completed. Invalidated reads
follow the current read rather than returning an older result or error.
Account-monitor setting conflicts reload the current
settings before redrawing; the rejected write is never replayed.
A successful settings write keeps its confirmed choices across redraws when
its following read fails, unless a newer settings revision or account generation
has already been read. The read error is shown separately; the next explicit
write still uses the last confirmed revision and recovers a conflict by
rereading rather than replaying.

The shared provider-list reads use the same ordering boundary, including
source edits, key and account actions and automatic price synchronization. A rate
write invalidates an older provider list even when the binding revision
has not changed. A superseded write response may refresh its returned list;
its mutation is never replayed. A write response that crosses a newly accepted
provider read also fetches a new list, including when another window made
the intervening change. See [GUI app shell](gui-shell.md).

An expired session merges concurrent renewal inside the process; an origin
lock excludes competing authentication from another process. Renewal writes
a pending record before consuming a refresh token. A lost or ambiguous
response becomes `refresh_unknown` / `REFRESH_UNCERTAIN` and requires a new
sign-in. An explicitly revoked refresh token may use the remembered password
once; another revocation, 2FA or a challenge requires user action. GET may
retry once after renewal. POST is never automatically replayed: authentication
requests refuse redirects, and renewal returns a query-again result.
A restart that recovers a durably saved new login also clears the previous
generation's local renewal backoff, as a completed login does. Recovery of a
same-generation rotation and independent server cooldowns retain their limits.

Explicit sign-in rechecks both its attempt and request context together under
the attempt mutex after local reads and before accepting credential writes.
Independent cancellation before that point prevents the commit. After acceptance,
the credential transaction finishes even if canceled; disk and protection calls
run outside the attempt mutex, so cancellation does not wait for slow I/O.

Every accepted explicit logout advances the durable account generation,
including an account already signed out, and then cleans up credentials.
Queued cleanup retains the latest logout generation, including a logout
received while an earlier cleanup is finishing, and cannot remove a new login.
When an encrypted credential cannot be decrypted, logout moves the original
file into a unique adjacent quarantine directory before clearing cleanup.
This permits another sign-in without discarding the original bytes. A failed
move keeps cleanup pending; a decryptable future format is still refused.
Old reads, browser candidates, settings actions, notifications
and price commits cannot revive that generation. Provider configuration
writes also use file revision checks. Binding and monitor-setting versions
are independent random tickets; importing a backup never imports authority
to finish an old operation. An unchanged sync keeps a local binding ticket.

Balance cards accept a Sub2API result under that same control boundary:
the account generation and current source request must still match before
the last-good balance, history and cache are updated together. Cache hits
also recheck the durable account, so another process's logout cannot leave
a cached success current. Forgetting balances invalidates in-flight cache
commits; a rejected response can retain a dated earlier reading, never add
its own value as a new success.
Balance trends use the same account-query identity as deduplication, independent
of the displayed provider and binding ticket. Rebinding an account or changing
the query starts a separate trend; returning to a retained query resumes its
own points. Legacy provider-only points cannot be assigned to an account and
are left untouched rather than imported into a panel trend.

Read-only card exports and the kept quota report perform the same local
authority check without requesting the panel or changing stored readings.
After a restart they restore the dated balance by its account query, rather
than by an API key; identical queries share a card and distinct queries keep
separate cards. Routing's key readings cannot replace a panel account balance.
A single-card refresh selects its source before deduplicating account queries,
so reordering a source's copy cannot suppress the displayed card's request.

## Browser verification

The Windows production build may open the same executable as an isolated
login host. It uses a temporary profile restricted to the current user.
An independent helper creates and owns that profile. It removes it after
the main process closes its anonymous lifetime pipe, including when only
the main process is forcibly terminated. The normal login path waits for
cleanup before accepting a candidate. Killing the helper as well, or
losing power, can still interrupt removal.
Only same-origin top-level navigation is allowed; popups, permissions and
Wails application messages are denied. Native request filtering blocks
refresh requests from documents and workers. Cloudflare challenge frames
have a limited cross-origin exception.

Tokens and the actual browser user agent travel through anonymous pipes.
The host must join a Windows Job before receiving its initialization;
failed containment rejects sign-in. The host must exit, its process tree
must end and its profile must be
removed before Go accepts a candidate. Cancellation, timeout, parent EOF
or cleanup failure discards it. Go then verifies identity, consumes the
candidate refresh token once, verifies identity again and checks the actual
source balance (or groups for an existing account) before saving.

HTTP, HTTPS and SOCKS5 proxies without proxy credentials are supported by
the host; a proxy URL containing credentials is refused. Browser cookies
are not transferred to Go. A site requiring such cookies may therefore
still reject the API session. Local native-host tests do not establish
compatibility with a real Cloudflare-protected panel.

Use the [build instructions](../../build/sub2api-webview2/README.md).
The preparer checks the pinned Wails version and upstream hashes, copies
and patches the dependency under `.tmp-sub2api-wails/`, and writes a private
modfile. It does not change the shared module cache or root module files.
Windows Makefile and CI entries use that same path. On a Wails upgrade,
review the patch and COM layouts, update the hashes, compile AMD64 and
ARM64, and rerun the native host tests.

## Verification

Run Go checks in a temporary HOME with pinned caches as described in
[provider and plugin ownership](provider-plugins.md#verification).

```sh
go test -tags nogui ./internal/sub2api -count=1 -timeout 3m
go test -race -tags nogui ./internal/sub2api -count=1 -timeout 5m
go test -tags nogui ./internal/provider ./internal/gui ./internal/backup -run 'TestSub2API|TestProviderConfig|TestProviderSaveRevision|TestProviderEditorRejects|TestKeepLast' -count=1 -timeout 5m
go test ./build/sub2api-webview2 -count=1
go run ./build/sub2api-webview2
go test -modfile .tmp-sub2api-wails/go.mod -tags production,sub2api_webview2 ./internal/sub2apibrowser -count=1 -timeout 3m
node --test internal/gui/tests/sub2api.test.cjs
```

The browser tests use a local fixture. Run the UI tests in Chromium and
WebKit, in all five languages, at desktop and narrow widths. Real panel
sign-in, Cloudflare-to-Go handoff, long-running monitoring and desktop
notification delivery require separate runtime checks.
