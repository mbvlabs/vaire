# Agent notes: extension command integration

Read-only E7-04 preparation against the accepted foundation plus stable dispatch/CLI paths; E7-03 packages/engine work was not reviewed. This is a wiring map, not completed behavior or a new design approval.

## CLI/RPC

`extCommands` in CLI tree still registers stubs. Switch add/remove confirmation flags to repeatable string arrays; remove obsolete broad-server restart metadata. Existing CommandMeta and checkOneCommand enforce flags, mutation, dry-run, destructive and restart contracts. `Confirm` never accepts yes as destructive authorization; `confirmOptions` supplies RPC Confirm/Confirms and `ConfirmTokens` merges them.

Node methods centralize handlers, locks, confirmation validators, run-log and activity policy. `confirmAny` validates submitted tokens but does not require them. Required target/restart tokens must be preflighted node-side before effects too; do not rely on CLI admission.

## Admission/locking

Current dispatcher checks awaiting/follower restrictions, submitted token validity, then global/cluster locks, then activity/run-log/handler. Extension methods need entries in awaitingRefused and followerRefused. Protected add is permitted without weakening grants; protected remove must use refusal policy. Check open restore and unfinished unsafe unfollow records explicitly.

Global locks already held by dispatcher must not be recursively acquired in extension helpers. For complete source/follower target sets, preserve global → heavy-I/O if needed → sorted cluster locks. Discover/preflight tokens before mutations and reload/revalidate current target membership/identities under lock. No broad server token substitute in direct extension commands.

## Activity/diagnostics

Activity derives action names from method registration but database correlation/reasons use explicit allowlists. Add safe fixed method/target/outcome metadata only; never descriptor, setting, SQL or param payloads. Existing activity tests cover admission/allowlists.

Doctor collects typed Facts in node/doctor then runs registered checks. New extension checks must preserve unavailable facts and pending restart as independent concepts. No extension doctor integration existed at this scout checkpoint.

## Evidence seams

Current config handling validates before save and dry-run skips save/access sync. Use injectable command/catalog/store callbacks to prove invalid descriptors and absent required tokens have zero persistence/package/restart/SQL effects. Add central method, activity redaction/correlation, awaiting/follower, protected remove, restore and unsafe-unfollow refusal tests. Existing follower direct-refusal and activity-policy tests are useful patterns. Keep follower-enabled extension mutations gated until full E7-05 ordering is complete.
