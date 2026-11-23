# Plan: per-environment egress policy

Status: proposal against `main` at `a9b5c19` and Substrate `v1.0.0`. The criteria are in
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
- Each rule has `hostnames` (at most 256): a lowercase DNS name, `*.<name>` for one leftmost label,
  or `*`. IP addresses, URLs and names with ports are invalid (`ateapi.proto:676-700`, test case
  `IP literal pattern` at `egress_policy_test.go:604`). Two rules that tie on a name and port are
  rejected (`ateapi.proto:609-619`).
- `ports` is `all` or up to 16 numbers in 1..65535. It defaults to 80 for `http` and 443 for
  `https`. `tls_passthrough` requires it (`ateapi.proto:700-791`).
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

```proto
message EgressPolicy {
  // Allows. Traffic that matches no rule is denied. Empty denies all egress.
  repeated EgressRule rules = 1;
}
message EgressRule {
  oneof protocol {
    HostRule http = 1;
    HostRule https = 2;
    HostRule tls_passthrough = 3;
  }
}
message HostRule {
  repeated string hostnames = 1;
  repeated int32 ports = 2;
  bool all_ports = 3;
}
```

- `CreateEnvironmentRequest.egress_policy` (optional `EgressPolicy`).
- `SetEnvironmentEgressPolicy(id, atespace, egress_policy)` replaces the policy and returns it.
- `GetEnvironmentEgressPolicy(id, atespace)` returns the stored policy.
- `GetServerInfo` lists the feature `egress_policy`.
- v1 maps rules only. Credential effects (`HttpRuleEffects`) are out of scope.

### Server

- Validate rules locally with Substrate's limits. A bad rule is `InvalidArgument`, and no actor is
  created.
- `CreateEnvironment` with a policy calls `CreateActor`, then `CreateActorEgressPolicy`, then
  responds. The actor stays suspended until its first wake, so the policy applies from first boot.
  Until the policy exists, the actor has none and Substrate denies its egress.
- If the policy write fails, the server deletes the actor and returns the policy error. If the
  delete fails too, it returns both errors.
- `CreateEnvironment` without `egress_policy` makes no policy call.
- `SetEnvironmentEgressPolicy` gets the actor (`NotFound` when missing), gets the policy, and calls
  `UpdateActorEgressPolicy` with its `uid` and `version`. On `NotFound` for the policy it calls
  `CreateActorEgressPolicy`. On `Aborted` or `AlreadyExists` it retries a bounded number of times,
  then returns `Aborted`.
- The `unknownfields` interceptor makes an older server refuse `egress_policy`, so a client never
  gets an environment with no policy when it asked for one.
- `ate.Client` gains `CreateEgressPolicy`, `GetEgressPolicy` and `UpdateEgressPolicy`, and
  `fakecontrol` serves the four Substrate RPCs with the same error codes.

## Out of scope

- Credential-injection effects (`HttpRuleEffects`).
- A server-wide default policy for environments created without one.
- Mapping E2B `allow_internet_access`, `network` and `update_network`, and Harbor `network_mode`.
  These are follow-ups once this lands.

## Open points

- **EG-A:** On a policy write failure, `CreateEnvironment` deletes the actor. The alternative leaves
  the environment and returns an error naming it.
- **EG-B:** `GetEnvironmentEgressPolicy` on an environment with no policy returns `NotFound`, or a
  "no policy" marker distinct from deny-all. Both deny today, but they differ once a server-wide
  default exists.
- **EG-C:** `WarmImage` probe actors run with no policy. Do they need one (for example, to pull at
  boot)?
- **EG-D:** v1 exposes `https` (MITM) rules, or only `http` and `tls_passthrough`. `https` needs the
  gateway CA in the task image.
- **EG-E:** Environments restored from a derived template's golden snapshot get no policy from the
  template. Confirm the per-environment policy is the only source.

## Pull requests

One change per pull request, in this order:

1. `ate.Client` egress policy methods and `fakecontrol` support.
2. Proto, `CreateEnvironment` with `egress_policy`, and the `egress_policy` feature.
3. `SetEnvironmentEgressPolicy` and `GetEnvironmentEgressPolicy`.
4. Go SDK.
5. Python SDK.
6. Threat model: close known gap 8.
7. Live e2e test.
