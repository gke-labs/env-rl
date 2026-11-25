# Plan: wait for an egress policy change, and check it from inside the environment

Status: proposal against `main` at `f81079e` and Substrate `v1.0.0`. It extends the
[per-environment plan](./README.md), which has no code yet. The criteria are rows `EG-31` to `EG-50`
of [acceptance-criteria.md](./acceptance-criteria.md). Decisions `EG-K` to `EG-O` are proposals
until the review. Issues: #47, #44.

## Goal

A caller that removes access with `SetEnvironmentEgressPolicy` can wait until the environment can no
longer use it, and a caller can check from inside an environment which hostnames its DNS answers.

- CUJ 1: As a harness that switches an environment from an agent phase to a verifier phase with less
  network, I should be able to make one call that returns once new connections to the removed hosts
  are refused, so that the verifier does not start under the old rules. Connections opened before
  the change are a separate case (see "Open connections").
- CUJ 2: As a caller, I should be able to check from inside my environment that a host is blocked,
  so that I can show a run had no network without trusting the stored rules alone.
- CUJ 3: As a caller that does not care, I should see no change: `SetEnvironmentEgressPolicy`
  without the new field returns after the write, as `EG-7` describes.

## Background

### Substrate `v1.0.0`

Paths below are relative to the module `go.ate.dev/substrate@v1.0.0`.

- DNS: the sandbox's DNS relay forwards a query only when the hostname matches a host or a host
  wildcard of the actor's policy, and answers `NXDOMAIN` otherwise. DNS bypasses the egress gateway
  (`internal/atunnel/dns/readme.md:70`). A policy created, updated or deleted while the actor runs
  applies to its DNS within about 5 seconds (`docs/guides/egress-traffic.md:18-23`). The relay
  caches the policy with the same expiry after the fetch as the gateway: `egressPolicyTTL` is 5
  seconds, `egressPolicyFetchTimeout` is 2 seconds, and the entry expires TTL after its fetch
  returns (`internal/atunnel/egresspolicy.go:41-45, 179-180`). So a DNS fetch that read the old
  policy just before a write can serve it until about 7 seconds after the write. When a refresh from
  `atelet` fails, the relay keeps serving the stale entry (`egresspolicy.go:164-165`), one more
  reason the wait probes DNS instead of trusting the clock alone.
- Connections: the egress gateway decides each new connection from a cached copy of the policy.
  `DefaultPolicyCacheTTL` is 10 seconds (`cmd/atenet/internal/router/egress/policycache.go:34-36`).
  Operators can change it with `--egress-policy-cache-ttl`, down to 0. The cache entry expires TTL
  after its fetch returns (`policycache.go:157-160`), and a fetch can take up to 4 seconds
  (`policyFetchTimeout`, `policycache.go:41`). So a fetch that read the old policy just before a
  write can serve it until about TTL plus 4 seconds after the write.
- Open connections: a TLS passthrough authorization lasts for its connection. An update applies to
  new connections after cached decisions expire, and need not end a connection that was already
  allowed (`docs/guides/network-egress.md:35-38`, agent-substrate/substrate#3463).
- The policy has no status that says when the relay and the gateway enforce a version.
  `EgressPolicy` carries rules and common metadata only.

- Metadata: `CreateActorEgressPolicy` sets `metadata.update_time` to `metadata.create_time` and
  `metadata.version` to 1 (`cmd/ateapi/internal/store/atepg/atepg.go:410-412`). `update_time` comes
  from `ateapi`'s clock, not from `ate-env-api`'s.
- Actor states: every request through the router's ingress resumes the actor
  (`cmd/atenet/internal/router/ingress/ingress.go:111`), and the resume waits on the actor's lease
  (`cmd/ateapi/internal/controlapi/workflow_resume.go:86`), so a guest call on a `SUSPENDING` actor
  wakes it again right after the suspend. The egress gateway refuses actors outside `RUNNING` and
  `RESUMING` (`cmd/atenet/internal/router/egress/egress.go:293-301`).

So a removed host stops resolving within about 7 seconds, and new connections to it stop within
about TTL plus 4 seconds. A user measured about 6 seconds from a write to enforcement on their cell
(#47). Nothing tells the caller when either has happened.

### env-rl today

- The [per-environment plan](./README.md) adds `SetEnvironmentEgressPolicy`, which returns after the
  Substrate write (`EG-7`). `EG-18` checks that an added host resolves within 15 seconds.
- The guest serves `ProcessService` and `FileSystemService`
  ([guest.proto](../../../proto/ateenv/v1alpha/guest.proto)). It has no call that resolves a name. A
  caller can run `getent` or `nslookup` with `StartProcess`, but many task images ship neither.
- The Harbor mapping (#47) needs a `set_network_policy()` that returns once the change holds. E2B
  `update_network` needs the same.

## Design

### Removed access

The server compares the old and the new policy in a canonical form: the set of
`(protocol, hostname, port)` after the port defaults of `EG-13`, with `all` ports kept as one value.

- A **removed pair** is a pair of the old policy that the new policy does not hold. The comparison
  is literal, so a pair that a broader new rule still allows counts as removed. This only makes the
  wait longer, never shorter.
- A **removed hostname** is a hostname of the old policy, without a wildcard, that no hostname of
  the new policy matches, with Substrate's matching (exact, `*.<name>` for one leftmost label, or
  `*`). DNS decides by hostname only, so these are the names whose DNS answer changes to `NXDOMAIN`.
- An environment with no policy has none, so a write to it removes nothing.
- The **slack** is 5 seconds. It covers the gateway's 4 second fetch timeout.
- The **span** is `max(TTL + slack, 7s)`. TTL plus the slack covers the gateway's cache. The 7
  second floor covers the DNS relay's cache (5 second TTL plus 2 second fetch timeout), which
  matters when the operator sets `--egress-policy-cache-ttl` below 2 seconds. With the default TTL
  the span is 15 seconds.

### Guest: resolve hostnames

`guest.proto` gains a service with one call:

```proto
service NetworkService {
  // Resolves each hostname with the guest's resolver, as code in the environment would.
  rpc ResolveHosts(ResolveHostsRequest) returns (ResolveHostsResponse);
}
message ResolveHostsRequest {
  repeated string hostnames = 1; // at most 256
}
message ResolveHostsResponse {
  repeated HostResolution results = 1; // one per hostname, in request order
}
message HostResolution {
  string hostname = 1;
  oneof result {
    Addresses addresses = 2; // A and AAAA records
    Empty nxdomain = 3; // rcode NXDOMAIN
    Empty no_records = 4; // rcode NOERROR with no A or AAAA record
    string error = 5; // any other failure, such as a timeout
  }
}
message Addresses {
  repeated string ips = 1;
}
```

- The guest sends A and AAAA queries straight to the nameservers of the environment's
  `/etc/resolv.conf`, with `golang.org/x/net/dns/dnsmessage` (already in `go.mod`), and reads the
  rcode. It skips `/etc/hosts` and keeps no cache, so the answer is the one the DNS relay gives now.
  Go's `net.Resolver` would read `/etc/hosts` and reports NXDOMAIN and an empty answer alike.
- The parser keeps the `A` and `AAAA` records of the Answer section and skips the other records,
  such as the `CNAME`s of a chain. When a UDP reply sets `Header.Truncated`, the guest repeats the
  query over TCP, which the relay serves (`internal/atunnel/dns/tcphandler.go`).
- The two answers combine into one result, in this order: `addresses` when either query returns an
  `A` or `AAAA` record; else `error` when either query fails; else `no_records` when either returns
  NOERROR with no `A` or `AAAA` record; else `nxdomain`, which needs both to answer NXDOMAIN. During
  a removal one query can still meet the old policy while the other meets the new one, and this
  order reports `nxdomain` only once both see the new policy.
- A hostname that is not a valid DNS name, a wildcard, an IP literal, or more than 256 hostnames is
  `InvalidArgument`. An empty list returns an empty response.
- A new service keeps name resolution apart from `ProcessService` and `FileSystemService`, which the
  [extend-env-surface](../../../.agents/skills/extend-env-surface/SKILL.md) guide names as the homes
  of in-environment operations. Pull request 1 adds the new service to that guide, to the
  architecture invariants in `AGENTS.md`, and to `hack/verify/layers.py` if the new package needs
  it.

### API: check an environment

`CheckEnvironmentEgress(id, atespace, hostnames)` forwards to `ResolveHosts` through
`s.guestSession` with `ensureAwake`, and returns the guest's results.

- It wakes a suspended environment, as every guest call does. It checks the hostnames before the
  wake, so a bad request does not resume an environment. An empty list returns an empty response
  without a guest call or a wake.
- An `NXDOMAIN` means that the policy blocks the name, or that the name does not exist. The SDK docs
  say so. It checks DNS only. A connection check needs a reachable external host (the reason the
  [per-environment plan](./README.md#testing) gives for DNS-only e2e rows).

### API: wait on `SetEnvironmentEgressPolicy`

`SetEnvironmentEgressPolicyRequest` gains `bool wait_for_enforcement`. Without it, nothing changes.
With it, the server writes the policy as `EG-7` describes, then waits for two conditions before it
responds:

1. **DNS.** Each removed hostname answers `nxdomain` inside the environment. The server polls
   `ResolveHosts` once a second, in batches of at most 256 hostnames.
2. **Time.** The span has passed since the write, when this call removes any pair. When the stored
   policy came from an update (`metadata.version` above 1), the span has also passed since its last
   update (`metadata.update_time`), whether or not this call removes anything, so that a wait right
   after an earlier write without the wait covers that write too. A version 1 policy comes from
   `CreateActorEgressPolicy`, which only adds access to an actor that had none (no policy denies all
   egress), and its `update_time` equals its `create_time`, so it adds no floor: a call that only
   adds hosts right after `CreateEnvironment` does not sleep. `update_time` comes from `ateapi`'s
   clock, so the server clamps it at its own `now`, as `requestedAt` does in
   `internal/apiservice/server.go`, and an `ateapi` clock ahead of `ate-env-api`'s cannot stretch
   the wait past the span or into the 60-second bound. The server learns the TTL from a new flag,
   `--egress-policy-cache-ttl`, default 10 seconds, the Substrate default. The operator sets it to
   the value of their egress gateway. `ate-env manifest` gains the same flag and renders it on the
   `ate-env-api` container, so an operator who deploys with the manifest sets both in one place.

The two run at the same time. With the defaults, a call that removes access waits about 15 seconds.
A call that removes nothing, on a policy last updated more than 15 seconds ago or created with the
environment, returns after the write.

- Wildcard hostnames are not probed. A change that removes only wildcards waits for the time
  condition only. The 7 second floor of the span keeps that wait at least as long as DNS takes, even
  with a TTL of 0.
- Probes do not wake the environment. Before each poll the server reads the actor's state with
  `GetActor`, and acts on it:
  - `RUNNING`: it calls `ResolveHosts` over a router connection without `ensureAwake`, so a poll
    never enters the wake loop or records `wake_wait`.
  - `RESUMING` or `REVERTING`: it skips this poll and reads the state again at the next one.
  - `SUSPENDING`, `SUSPENDED` (an environment shown as `PAUSED` included) or `CRASHED`: it stops
    probing and waits for the time condition only. A request through the router resumes the actor,
    and on `SUSPENDING` that resume queues behind the suspend's lease and wakes the actor right
    after the suspend. A resume within the span can meet a cached decision, so the time condition
    still holds. A `CRASHED` actor's sandbox is gone, and the gateway refuses it.
  - `DELETING`, or the actor is gone: it returns `NotFound`, as `EG-12` and `ensureAwake` do.
  - A suspend that starts between a `RUNNING` read and the `ResolveHosts` call can still resume the
    actor once. The window is one poll, and the next read stops probing.
- An environment restored from an older golden snapshot can run a guest without `NetworkService`,
  which answers `Unimplemented`. The server then returns `FailedPrecondition` at once, with an error
  that says the guest cannot resolve names and the policy is written. It does not poll until the
  bound.
- The server bounds the wait itself, at 60 seconds or 1 second before the caller's deadline,
  whichever comes first. When a condition does not hold by then, it returns `DeadlineExceeded` with
  an error that names the hostnames that still resolve, or the time condition. The client gets that
  error before its own deadline. The policy stays written.
- The response gains `EgressEnforcement`: the hostnames the server probed and how long it waited.
- `GetServerInfo` lists the feature `egress_policy_wait`. As `EG-15` describes for `egress_policy`,
  the SDKs do not call `GetServerInfo` themselves. An older server refuses the field with
  `InvalidArgument`, and the docs say to check the feature first.

### Added access

The wait does not cover hosts the change adds. A host that is not reachable yet fails closed: a
connection is refused, and nothing leaks. A caller that needs the host before it continues checks it
with `CheckEnvironmentEgress`. `EG-18` checks that an added host resolves within 15 seconds.

### Open connections

The wait does not close connections that were allowed before the change. A harness that must start a
phase with no open connection runs it in a new environment created with the narrower policy. The
Harbor mapping (#47) does this for its verifier environment. The SDK and threat model docs say so.

### SDKs

- Go: `SetEgressPolicy` takes an option `WaitForEnforcement()`, and the client gains `CheckEgress`.
- Python: `set_egress_policy(..., wait=True)` and `check_egress(hostnames)` on `Env` and
  `SandboxHandle`, sync and async.
- The SDKs add no default deadline. The server bounds the wait, and `CheckEgress` can wake an
  environment, which can take longer than any short default.

## Alternatives

3 options were considered for where the wait runs:

- Server: the server diffs the policies, probes through the guest and waits.
- SDK: each SDK diffs the policies and probes with `StartProcess` and a DNS tool.
- Fixed: the SDKs sleep a fixed time after a write.

| **Properties ↓ Option →**                      | **Server**                                     | **SDK**                                      | **Fixed**                       |
| ---------------------------------------------- | ---------------------------------------------- | -------------------------------------------- | ------------------------------- |
| **One implementation for Go and Python?**      | Yes.                                           | No. Each SDK repeats the diff and the probe. | Yes, but trivial.               |
| **Works on images without DNS tools?**         | Yes. The guest resolves.                       | No.                                          | Yes.                            |
| **Knows the old policy?**                      | Yes. `EG-7` already reads it before the write. | Only with an extra get, which can race.      | n/a                             |
| **Shows the caller that the host is blocked?** | Yes, from inside the environment.              | Yes.                                         | No.                             |
| **Time on a change that removes nothing?**     | None.                                          | None.                                        | The full sleep on every change. |

Evaluation: SDK repeats logic in two languages and depends on the task image. Fixed proves nothing
and wastes time on every phase switch.

Decision (proposed, `EG-K`): Server.

## Out of scope

- Waiting for added hosts (see "Added access").
- Closing open connections on a change, for example with a suspend and resume. It is not confirmed
  that a resume on gVisor ends outbound connections.
- Connection checks from inside the environment.
- A Substrate status that reports when a policy version is enforced. We will ask for it upstream. It
  would replace both conditions of the wait.
- The Harbor, E2B and fleet changes that call the wait. Their plans own them.

## Decisions

Proposed, to be decided in the review:

- **EG-K:** The server runs the wait, with a guest call that resolves names.
- **EG-L:** The wait covers removed access only. Added access fails closed, and
  `CheckEnvironmentEgress` checks it.
- **EG-M:** The time condition uses an operator flag that mirrors Substrate's
  `--egress-policy-cache-ttl`, default 10 seconds, plus 5 seconds of slack, with a floor of 7
  seconds for the DNS relay's cache: `max(TTL + 5s, 7s)`. `ate-env manifest` mirrors the flag.
- **EG-N:** A wait that does not finish within its bound returns `DeadlineExceeded` and leaves the
  new policy in place. It does not roll back.
- **EG-O:** The wait does not close open connections. A phase that needs none runs in a new
  environment.

## Testing

Unit tests run against `fakecontrol` and a fake guest whose `ResolveHosts` answers change after a
set number of polls, with a fake clock for the time condition. The Kind E2E job checks DNS only
(`EG-47`). `EG-48` checks connections on a GKE cluster with Substrate's egress gateway, and the pull
request that meets it records the run.

## Pull requests

One change per pull request, after pull requests 2 and 3 of the
[per-environment plan](./README.md#pull-requests):

1. Guest `NetworkService.ResolveHosts`.
2. `CheckEnvironmentEgress`, with its row in `proto/testdata/client-parity/rpc_coverage.json`.
3. `wait_for_enforcement` on `SetEnvironmentEgressPolicy`, `--egress-policy-cache-ttl` on
   `ate-env-api` and on `ate-env manifest`, and the `egress_policy_wait` feature.
4. Go SDK.
5. Python SDK.
6. Threat model and docs.
7. Kind e2e test (DNS), and the live GKE check (connections).
