# Plan: labels and a time to live on an environment

Status: proposal, 2026-11-13, checked against `main` at `456353fd` and Substrate at the pin
(`274605f1`) and at its `main` (`cf5768a0`). Pull request 1, the store, is #407; the rest is not
built. Decided on 2026-11-15: EM-E (the actor first) and EM-F (a read from the API server before
each delete by deadline). Issues: #386 (time to live), #387 (labels; its first part,
`ListEnvironments`, is a pull request of its own). The criteria are in
[acceptance-criteria.md](./acceptance-criteria.md), group `EM`.

## 1. Goal

A caller attaches labels to an environment, lists environments by label, and gives an environment a
time after which it is deleted. The first users are `ate_env_e2b` (`metadata`, `Sandbox.list`,
`timeout`, `set_timeout`: E2B-31 and E2B-32); the OpenHands and NeMo Gym plans list the same gaps.

## 2. The problem: no place for the data

- A Substrate `Actor` has a name, timestamps, a template reference, a worker selector and a status.
  It has no labels, no annotations and no expiry, at the pin and on Substrate `main`.
- `ate-env-api` keeps no state and runs as more than one replica. It talks only to the Substrate
  API. Its pod mounts no Kubernetes API token.

The owner's decision of 2026-11-13: do both of these.

1. Ask Substrate for labels and an expiry on an actor (a comment on
   `agent-substrate/substrate#1331`). When Substrate has them, the store below goes away and the API
   of section 4 stays.
2. Until then, `ate-env-api` keeps the data itself, in Kubernetes. This plan is that store.

## 3. The store

One ConfigMap for each environment that has a label or a deadline, in the namespace of
`ate-env-api`. An environment with neither has no object, thus a caller that uses neither feature
pays nothing.

| Part              | Choice                                                                                                                                                                                               |
| ----------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Name              | `ate-env.<atespace>.<id>`. Both parts are DNS labels, thus the name is a valid object name and two environments never share one.                                                                     |
| Kubernetes labels | `ate-env.dev/atespace`, and `ate-env.dev/deadline: "true"` when the object holds a deadline. They select the objects of one atespace and the ones the sweep reads.                                   |
| Data              | `labels`: the caller's labels as one JSON object. `expire_time`: RFC 3339, UTC. `uid`: the uid of the actor, which tells a new environment from an old one with the same id.                         |
| Read path         | A shared informer in each replica. A read of labels or of a deadline does not call the Kubernetes API. The informer of a replica follows a write on another replica after a delay (open point EM-D). |

The caller's labels are data, not Kubernetes labels: E2B metadata values are free text, and a
Kubernetes label value is 63 characters of a small alphabet. The filter of a list is applied by
`ate-env-api` on the informer's copy.

Limits, checked at the API: at most 32 labels, a key of 1 to 63 bytes of printable ASCII, a value of
at most 256 bytes of UTF-8.

### The actor is the truth

The actor decides that an environment exists. The object only adds to it.

- Create (EM-E, decided): create the actor, then write the object once, with the uid of the actor.
  If that write fails, delete the actor with its uid as the precondition and fail the create.
- Delete: read the object from the informer, delete the actor, then delete that object with its uid
  and resource version as the precondition. An environment with no object in the informer costs no
  call to Kubernetes; an object that the informer did not have yet is an orphan for the sweep.
- A write never deletes. When an object of the name exists, the write returns that object, as the
  API server has it, and the create path decides: an object of the same actor is an earlier write of
  this environment; for an object of another actor the create path reads the actor again. When the
  actor of the write is the one that exists, the object is of an earlier environment with the same
  id, and the create path writes over it. When it is not, the write is late (its environment was
  deleted and the id created again while the write waited), and the create path drops it.
- Replace: the write over an object deletes that object with its uid and resource version as the
  precondition and creates a new one, thus the new object has its own create time for the grace
  period. An object that was written again since stays, and the write fails.
- A crash between the two writes of a create leaves an actor without an object: an environment
  without the labels or the deadline that its caller asked for, which is what each environment is
  today. A crash between the two writes of a delete leaves an object without an actor. The sweep
  deletes an object whose actor is gone, or has another uid, once the object is older than a grace
  period (1 minute).
- An actor without an object is an environment with no labels and no deadline.

Substrate assigns the uid and the create time of an actor in `CreateActor` and ignores both on input
(`ResourceMetadata` in `api/ateapi/v1/ateapi.proto`). The call returns the actor, in the state
`SUSPENDED`. Thus an object that is written before the actor cannot hold the `uid`, or an
`expire_time` that is counted from the actor's create time: it needs a second write after
`CreateActor`, and a crash between the two leaves an object with no `uid` next to a live actor,
which the orphan rule must then not delete. That is why EM-E chose the other order: the actor first,
then one write of the object with the `uid`. Its crash window leaves an actor without an object,
thus an environment without the deadline that its caller asked for.
`internal/ate.Client.CreateActor` returns the actor that `CreateActor` returns (since #392, which
reads its `create_time`); the pull request that adds the first create field keeps its uid.

### The sweep

Each replica runs it on a timer (30 s). It reads the objects from the informer, never from the
Kubernetes API server.

- For each object whose `expire_time` is past: delete the environment, through the code path of
  `DeleteEnvironment`, with the `uid` of the object as the precondition of the delete
  (`DeleteOptions.uid` of Substrate's `DeleteActorRequest`, which the pin has). A suspended
  environment is deleted too, also one that is `PAUSED`. An actor with another uid is a new
  environment with the same id: Substrate refuses the delete with `Aborted`, the actor stays, and
  the object is an orphan. Today `internal/ate.Client.Delete` sends no precondition; a caller's
  `DeleteEnvironment` stays that way.
- For each orphan object: delete the object.

A delete can fail for good. `WarmImage` records one case: a probe that is `CRASHED` after a resume
that failed, which Substrate refuses to delete (`FailedPrecondition`, "DeleteVolumes prerequisite
not met"; see `deleteWarmProbe`). An environment in that state at its deadline is not deleted by any
number of sweeps. What the sweep does then (try again on each interval, stop after a number of tries
and keep the object, report it in a metric) is open point EM-I.

To know that an object is an orphan, the sweep must read the actor, and actors are not in the
informer. That is a read from Substrate in each replica on each interval: one `GetActor` for each
object, or one `ListActors` for each atespace that has an object.

Both replicas can run the same delete. It is idempotent, and "not found" is success. Thus the plan
as it stands has no leader election. The deadline is honoured within one sweep interval.

The uid does not cover one case: a `SetEnvironmentTimeToLive` on one replica moves the deadline of
the same actor, and the sweep of another replica, on an informer that does not have the change yet,
sees the earlier deadline as past. EM-F closes this window: before each delete by deadline, the
sweep reads the object from the API server, and deletes only when that copy is past its deadline
too. Each replica still sweeps, and each reads Substrate for orphans; there is no `Lease`.

### Opt-in, with its own permission

The store is off by default: `--environment-store=configmaps` on `ate-env-api` turns it on.
`ate-env manifest --environment-metadata` sets the flag and adds what it needs: a Role and a
RoleBinding for `list`, `watch`, `create` and `delete` on ConfigMaps in the namespace of
`ate-env-api`, and the Kubernetes API token in the pod. These are the calls the store makes: its
informer lists and watches, and a write creates and deletes. The pull request that reads an object
from the API server before a delete by deadline (EM-F) adds `get`.

The store's Kubernetes client has a rate limit of its own: 50 requests per second with a burst of
100, on each replica (`--environment-store-qps`, `--environment-store-burst`; a negative rate sets
no limit in the client and leaves it to API Priority and Fairness). The default of client-go, 5 per
second with a burst of 10, is for a command-line tool. `DeleteEnvironment` deletes the object inside
the RPC, with 10 s for that call. Measured against a local HTTP server with 200 such deletes at
once: at the default of client-go, 59 objects are deleted in those 10 s and 141 deletes fail at once
and wait for the sweep; at 50 and 100, all 200 end in 2 s. A measurement on a cluster, create and
delete p50 and p95 of a fleet teardown with the store on against the store off, comes with the pull
request that writes objects.

Without the store, a request that carries labels or a time to live is refused with
`FailedPrecondition`, and `GetServerInfo` says that the features are off. It is never accepted and
dropped.

## 4. The API

On `proto/ateenv/v1alpha`:

| Change                                                 | Meaning                                                                                        |
| ------------------------------------------------------ | ---------------------------------------------------------------------------------------------- |
| `CreateEnvironmentRequest.labels`                      | Labels of the new environment.                                                                 |
| `CreateEnvironmentRequest.time_to_live`                | A duration from the create. Unset: no deadline.                                                |
| `Environment.labels`, `Environment.expire_time`        | Returned by get, create and list.                                                              |
| `ListEnvironmentsRequest.label_selector`               | Equality on a set of labels. Applied with the status filter, on each page.                     |
| `SetEnvironmentTimeToLive(id, atespace, time_to_live)` | A new deadline, counted from the call. Zero removes the deadline. This is E2B's `set_timeout`. |
| `GetServerInfoResponse`                                | Says whether labels and the time to live are on.                                               |

Labels are set at create and not changed after it. No user needs an update yet; it is one more RPC
when one does.

## 5. Costs and limits

- One more Kubernetes write for each create and each delete of an environment that uses a feature. A
  fleet that labels each environment puts its create rate on the Kubernetes API server. The default
  client rate limit of `client-go` applies; the plan measures it before it raises it.
- A second source of truth, with the crash window that the sweep closes.
- `ate-env-api` gains a Kubernetes permission and a token, when the operator opts in. The threat
  model gets a row: a caller's label text is stored in a ConfigMap, and whoever reads ConfigMaps in
  that namespace reads the labels of each atespace.
- The label filter of a list is applied after the page is read, as the status filter is. A page can
  be short or empty and still have a next page. Substrate's `ListActors` filters only by atespace,
  and the `ListEnvironments` pull request reads at most 1000 actors for one page. Thus a selector
  that matches a few environments in an atespace of N actors costs the caller about N / 1000 calls,
  most of them with an empty page. Whether the server does more for one call (the matching ids from
  the informer first, or more than one Substrate page up to a budget) is open point EM-G.
- The switch is one for the server, and it turns on labels and the time to live together. With the
  proposal of EM-B, it also changes callers that use neither: today `ate_env_e2b` gives an
  environment of a `Sandbox.create()` with no `timeout` no end (and warns only when a `timeout` is
  given); with the store on, the same call gets E2B's default and the environment is deleted after
  300 s. An operator who turns the store on for the labels of one workload changes the lifetime of
  the environments of each E2B caller of that server. Open point EM-H.
- A read is as fresh as the informer of the replica that serves it. A `GetEnvironment` or a
  `ListEnvironments` on one replica, right after a `CreateEnvironment` or a
  `SetEnvironmentTimeToLive` on another, can find the actor in Substrate before the watch event of
  the object arrives. It then returns no labels, or the earlier deadline. The replica that made the
  write has the same delay on its own informer, unless it also keeps what it wrote. Which guarantee
  a read gives is open point EM-D.
- An environment that was created without the store, or before it, has no deadline. Turning the
  store off stops the sweep: deadlines are then not enforced, and the objects stay.

## 6. Pull requests

| Pull request | Content                                                                                                               | After                 |
| ------------ | --------------------------------------------------------------------------------------------------------------------- | --------------------- |
| this         | This plan and the `EM` criteria                                                                                       |                       |
| 1 (#407)     | The store: the flag, the manifest switch with the Role, the informer, the object for an environment, the orphan sweep | this                  |
| 2            | Labels: the create field, get, the list filter; Go and Python clients                                                 | 1, `ListEnvironments` |
| 3            | The time to live: the create field, `SetEnvironmentTimeToLive`, the deadline sweep; Go and Python clients             | 1                     |
| 4            | `ate_env_e2b`: `metadata`, `Sandbox.list`, `timeout`, `set_timeout` (E2B-31, E2B-32)                                  | 2, 3                  |

## 7. Open points

| ID   | Point                                                                                                                                                                                                                                                                                                                                                                                                       | Proposal                                                                                                                                                          |
| ---- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| EM-A | The object kind: ConfigMap, or a CRD of our own                                                                                                                                                                                                                                                                                                                                                             | ConfigMap: no cluster-scoped install, and the data is small                                                                                                       |
| EM-B | The default of E2B's `timeout` in `ate_env_e2b` when the store is off                                                                                                                                                                                                                                                                                                                                       | Keep today's warning; with the store on, E2B's 300 s default                                                                                                      |
| EM-C | Whether the deadline moves when the environment is used (E2B does not move it; #76 asks for idle)                                                                                                                                                                                                                                                                                                           | No: a fixed deadline. Idle suspend stays #76                                                                                                                      |
| EM-D | What a read on one replica gives right after a write on another: the informer's copy alone (then `EM-4` holds only after the watch delay), or a fallback for a miss on a young actor (a read from the API server's watch cache), with the uid of the object compared with the uid of the actor on each read                                                                                                 | Open                                                                                                                                                              |
| EM-E | The order of the two writes of a create. Object first: a second write for the `uid`, and a rule for an object with no `uid` and a live actor. Actor first: one write, a rollback with `DeleteOptions.uid`, and a crash window that leaves a live environment with no deadline. With it: `expire_time` counted from the actor's create time (`EM-9` today) or from the clock of `ate-env-api` at the request | Decided 2026-11-15: the actor first, one write of the object with the uid, a rollback with `DeleteOptions.uid`; `expire_time` counts from the actor's create time |
| EM-F | How the sweep is safe against a late informer after `SetEnvironmentTimeToLive`, and how many replicas read Substrate for orphans: each replica sweeps from its informer (the plan as it stands), a read of the object from the API server before each delete by deadline, or one sweeper behind a `coordination.k8s.io` `Lease` (one more Role rule)                                                        | Decided 2026-11-15: each replica sweeps from its informer, and reads the object from the API server before each delete by deadline; no `Lease`                    |
| EM-G | A list with a label selector in a large atespace: filter each Substrate page (the plan as it stands; many empty pages), resolve the matching ids from the informer and read those actors, or read Substrate pages inside one call up to a budget of actors                                                                                                                                                  | Open                                                                                                                                                              |
| EM-H | Whether labels and the time to live turn on together. One switch (the plan as it stands) with EM-B gives each E2B caller with no `timeout` a 300 s lifetime when the store is turned on; the other choices are two switches with two answers in `GetServerInfo`, or one switch and a default in `ate_env_e2b` that does not follow it                                                                       | Open                                                                                                                                                              |
| EM-I | An environment whose delete by deadline Substrate refuses for good, such as a `CRASHED` actor after a resume that failed: the sweep tries on each interval without end, or stops after a number of tries; the object stays or goes; which metric or log reports it                                                                                                                                          | Open                                                                                                                                                              |
