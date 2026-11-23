# Plan: per-environment egress policy

Status: proposal against `main` at `6183240c` and Substrate `v1.0.0`. The criteria are in
[acceptance-criteria.md](./acceptance-criteria.md). No code exists yet.

## Goal

Close known gap 8 of the [threat model](../../security/threat-model.md#known-gaps) ("ate-env sets no
actor egress policy"). A caller sets an egress policy when it creates an environment and can replace
it later. The API has an explicit deny-all shape: a policy with no rules.

## Background

### Substrate `v1.0.0`

Paths below are relative to the module `go.ate.dev/substrate@v1.0.0`.

- The egress policy is a singleton subresource of an actor, named `default`. It has four RPCs:
  `GetActorEgressPolicy`, `CreateActorEgressPolicy`, `UpdateActorEgressPolicy` and
  `DeleteActorEgressPolicy` (`api/ateapi/v1/ateapi.proto:66-84`, message `EgressPolicy` at
  `:585-626`). `CreateActorRequest` carries only the actor (`:1778-1783`), so the policy is a second
  call.
- `EgressPolicy.rules` holds at most 256 rules. Rules are allows, and traffic that matches no rule
  is denied (`ateapi.proto:604-625`). An empty list is valid (test case `no rules` in
  `cmd/ateapi/internal/apivalidation/egress_policy_test.go:465`).
- Each `EgressRule` is one of `http`, `https` or `tls_passthrough` (`ateapi.proto:650-672`). `https`
  is intercepted (MITM) and needs the actor to trust the gateway CA. `tls_passthrough` forwards TLS
  on the SNI without decryption (`:714-770`).
- Each rule has `hostnames`: 1 to 256 patterns, without repeats. A pattern is a lowercase DNS name
  (RFC 1123, no trailing dot), `*.<name>` for one leftmost label, or `*`. Uppercase letters, a
  trailing dot, IP addresses, URLs and names with ports are invalid (`ateapi.proto:676-700`; test
  cases at `egress_policy_test.go:572-604`).
- `ports` is a union: `all`, or 1 to 16 numbers in 1..65535 without repeats. A `ports` with neither
  is invalid. It defaults to 80 for `http` and 443 for `https`. `tls_passthrough` requires it
  (`ateapi.proto:756-791`).
- A rule must set one of `http`, `https` or `tls_passthrough` (test case `no protocol` at
  `egress_policy_test.go:497`).
- Two rules that tie on a pattern and a port are rejected, whatever their protocols. Defaults are
  applied first, so an `https` rule left on 443 ties with a `tls_passthrough` rule on 443 for the
  same pattern (`cmd/ateapi/internal/apivalidation/egress_policy.go:92-127`).
- The policy table references the actor with `ON DELETE CASCADE`
  (`cmd/ateapi/internal/store/atepg/migrations/000001_initial.sql:34-43`): deleting an actor deletes
  its policy.
- `UpdateActorEgressPolicy` replaces the whole policy and requires `metadata.uid` and
  `metadata.version` as preconditions (`ateapi.proto:1917-1932`). A version or UID conflict is
  `Aborted`. A second create is `AlreadyExists`. A create under a missing actor is
  `FailedPrecondition` (`cmd/ateapi/internal/controlapi/egress_policy.go:118-135`). A get with no
  policy is `NotFound` (`:60-64`).
- DNS from an actor without a policy, or for a name no rule allows, gets `NXDOMAIN`. A policy
  created, updated or deleted while the actor runs reaches its DNS within about 5 seconds
  (`docs/guides/egress-traffic.md:18-23`). The egress gateway caches each policy for
  `--egress-policy-cache-ttl`, 10 seconds by default
  (`cmd/atenet/internal/router/egress/policycache.go:34-36`). An open passthrough connection
  survives an update (`docs/guides/network-egress.md:35-38`).
- `CreateActor` stores the actor `SUSPENDED` (`cmd/ateapi/internal/controlapi/actor.go:127-131`).
  Substrate's own template controller writes a golden actor's policy after create and before
  `ResumeActor`, and asks callers to do the same for other actors
  (`docs/reference/api-guide.md:177-191`). Actors restored from a golden snapshot do not inherit the
  golden policy.
- The atespace editor role can create, update and delete egress policies
  (`docs/guides/authorization.md:127`). `ate-env manifest` grants `ate-env-api` that role
  ([manifest.go](../../../cmd/ate-env/manifest.go)), so it needs no new permission.

### env-rl today

`CreateEnvironment` ([server.go](../../../internal/apiservice/server.go)) calls `ate.Client.Create`
([client.go](../../../internal/ate/client.go)), which sends one `CreateActor`. Nothing writes a
policy, so each actor has none and Substrate denies all its egress. `docs/plans/harbor` (`HB-20`,
`HB-22`) and `docs/plans/e2b-runtime-support` (`E2B-22`) refuse egress options for this reason.

## Design

### API (`proto/ateenv/v1alpha/env.proto`)

The messages follow Substrate's, one message per protocol, so a later field for one protocol (such
as `effects` on `http` and `https`) is not a breaking change.

```proto
message EgressPolicy {
  // Allows. Traffic that matches no rule is denied. Empty denies all egress.
  repeated EgressRule rules = 1;
}
message EgressRule {
  oneof protocol {
    HttpRule http = 1;
    HttpsRule https = 2;
    TlsPassthroughRule tls_passthrough = 3;
  }
}
// Cleartext HTTP. ports defaults to 80.
message HttpRule {
  repeated string hostnames = 1;
  Ports ports = 2;
  reserved 3; // effects
}
// HTTPS that the gateway intercepts. The task image must trust the gateway
// CA. For HTTPS without interception, use tls_passthrough. ports defaults to
// 443.
message HttpsRule {
  repeated string hostnames = 1;
  Ports ports = 2;
  reserved 3; // effects
}
// TLS forwarded on the SNI without decryption. ports is required.
message TlsPassthroughRule {
  repeated string hostnames = 1;
  Ports ports = 2;
}
// Exactly one of all and numbers.
message Ports {
  AllPorts all = 1;
  repeated int32 numbers = 2;
}
message AllPorts {}
```

- `CreateEnvironmentRequest.egress_policy` (optional `EgressPolicy`).
- `SetEnvironmentEgressPolicy(id, atespace, egress_policy)` replaces the policy and returns it.
- `GetEnvironmentEgressPolicy(id, atespace)` returns the stored policy.
- `GetServerInfo` lists the feature `egress_policy`.
- v1 maps rules only. Credential effects (`HttpRuleEffects`) are out of scope.

### Server

- Validate rules locally with each rule of Substrate listed in `EG-3`. Substrate's validator is in
  `internal/` and cannot be imported, so the criterion lists the full set and a test holds each
  case. A bad rule is `InvalidArgument`, and no actor is created.
- `CreateEnvironment` with a policy calls `CreateActor`, then `CreateActorEgressPolicy`, then writes
  the environment store's object when the request sets `time_to_live` (or labels, later), then
  responds. The actor stays suspended until its first wake, so the policy applies from first boot.
  Until the policy exists, the actor has none and Substrate denies its egress.
- When a write after `CreateActor` fails (the policy or the store's object), the server deletes the
  actor as `EM-6` does: with the actor's uid as the precondition (`DeleteWithUID`), under
  `context.WithoutCancel` with a bounded timeout. A caller that cancels cannot leak the actor, and a
  slow rollback cannot delete a new actor with the same id. Substrate deletes the policy with the
  actor. If the delete fails too, the server returns both errors.
- `CreateEnvironment` without `egress_policy` makes no policy call.
- `SetEnvironmentEgressPolicy` gets the actor (`NotFound` when missing), gets the policy, and calls
  `UpdateActorEgressPolicy` with its `uid` and `version`. On `NotFound` for the policy it calls
  `CreateActorEgressPolicy`. On `Aborted` or `AlreadyExists` it retries a bounded number of times,
  then returns `Aborted`. A `FailedPrecondition` from `CreateActorEgressPolicy` means the actor was
  deleted after the get, and the server returns `NotFound`.
- `GetEnvironmentEgressPolicy` returns `NotFound` for an environment with no policy, as Substrate
  does. On a `NotFound` it gets the actor, so the error tells a missing environment from a missing
  policy.
- `WarmImage` probe actors get no policy. The node pulls the image before the actor starts, and the
  probe only waits for the guest on port 80.
- The `unknownfields` interceptor makes an older server refuse `egress_policy`, so a client never
  gets an environment with no policy when it asked for one.
- `ate.Client` gains `CreateEgressPolicy`, `GetEgressPolicy` and `UpdateEgressPolicy`, and
  `fakecontrol` serves the four Substrate RPCs with the same error codes.

## Out of scope

- Credential-injection effects (`HttpRuleEffects`).
- A server-wide default policy for environments created without one.
- Mapping E2B `allow_internet_access`, `network` and `update_network`, and Harbor `network_mode`.
  These are follow-ups once this lands.

## Decisions

Decided in the review of #45 (2026-11-23):

- **EG-A:** A failed write after `CreateActor` deletes the actor by uid under a bounded
  `WithoutCancel` context, as `EM-6`.
- **EG-B:** `GetEnvironmentEgressPolicy` with no policy returns `NotFound`, as Substrate does. A
  distinct "no policy" marker can come with a server-wide default.
- **EG-C:** `WarmImage` probe actors need no policy.
- **EG-D:** v1 exposes `https`. The proto comment and the SDK docs point plain HTTPS callers to
  `tls_passthrough` and say that `https` needs the gateway CA in the task image.
- **EG-E:** Confirmed in Substrate `v1.0.0` (`docs/reference/api-guide.md:177-191`): an actor
  restored from a golden snapshot does not inherit `golden_egress_policy`. The per-environment
  policy is the only source.

## Testing

The Kind e2e rows (`EG-17`, `EG-18`) check DNS only: an allowed name resolves and a denied one gets
`NXDOMAIN`. DNS is decided from the policy alone, while a connection through the egress gateway
needs a reachable external host, which makes CI flaky. `EG-19` checks allowed and denied connections
on a GKE cluster with Substrate's egress gateway, and the pull request that meets it records the
run.

## Pull requests

One change per pull request, in this order:

1. `ate.Client` egress policy methods and `fakecontrol` support.
2. Proto, `CreateEnvironment` with `egress_policy`, and the `egress_policy` feature.
3. `SetEnvironmentEgressPolicy` and `GetEnvironmentEgressPolicy`.
4. Go SDK.
5. Python SDK.
6. Threat model: close known gap 8.
7. Kind e2e test (DNS), and the live GKE check (connections).
