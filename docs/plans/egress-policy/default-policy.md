# Plan: a default egress policy

Status: proposal against `main` at `d909f65d` and Substrate `v1.0.0`. It extends the
[per-environment plan](./README.md), which has no code yet. The criteria are rows `EG-20` to `EG-30`
of [acceptance-criteria.md](./acceptance-criteria.md). Decisions `EG-F` to `EG-J` are proposals
until the review. Issue: #48.

## Goal

An operator configures one egress policy on `ate-env-api`. An environment created without
`egress_policy` gets that policy. No client changes.

- CUJ 1: As an operator, I should be able to allow the hosts my tasks install from in one place, so
  that every environment gets them and no harness changes.
- CUJ 2: As a caller, my explicit policy should replace the default, including a policy that denies
  all egress, so that I can still lock down one task.
- CUJ 3: As a caller, I should be able to tell whether my environment runs with the default, so that
  I know what its code can reach.

## Background

The [per-environment plan](./README.md) lets a caller pass `egress_policy` on `CreateEnvironment`.
However, many callers never build the create request themselves:

- A harness on the Python fleet acquires a sandbox for a task. The task and the fleet configuration
  have no egress field.
- The fleet creates warm-pool environments on its own.
- The R2E-Gym adapter creates a second environment to grade a rollout.

Currently, these environments get no policy, and Substrate denies all their egress. R2E-Gym is the
concrete case: its runtime installs a package from PyPI in every sandbox and assumes an open
network. On Substrate the install fails, and on most task images one of its tools then fails. #48
has the details.

All three paths end in the same `CreateEnvironment` call of the Python client. This means that a
default applied in the server reaches them with no client change.

## Design

### Configuration

`ate-env-api` gains one flag, `--default-egress-policy-file`. The file holds one `EgressPolicy` of
the per-environment plan, as protobuf JSON. For example, a default that lets `pip` reach PyPI:

```json
{
  "rules": [
    {
      "tls_passthrough": {
        "hostnames": ["pypi.org", "files.pythonhosted.org"],
        "ports": { "numbers": [443] }
      }
    }
  ]
}
```

- The server reads the file once, at start. It checks the policy with the rules of `EG-3`.
- A file that is missing, that does not parse, or that fails a check ends the process before it
  serves. We do this because a default that is silently dropped leaves every environment without
  network.
- A policy with no rules is valid. Every environment then gets an explicit deny-all policy.
- At start, the server logs the hostnames and ports the default allows.
- Without the flag, the server behaves as the per-environment plan describes.

`ate-env manifest` gains a flag of the same name. It reads a local file, runs the same checks, and
fails without output when the file is invalid. It renders the file into a ConfigMap, mounts it
read-only into `ate-env-api`, and passes the path to the server. It also puts a hash of the content
in a pod annotation. This means that applying a changed default restarts the Deployment. The mount
needs no Kubernetes API token and no new permission.

### Create

- `CreateEnvironment` without `egress_policy` writes the default as the actor's policy. The order is
  the one of an explicit policy: the actor, then the policy, then the environment store's object.
  The actor is still suspended, so the default applies from the first boot.
- A failed write rolls back as `EG-4` and `EG-5` describe.
- `CreateEnvironment` with `egress_policy` is unchanged, including a policy with no rules. The
  default is never merged into an explicit policy.
- `WarmImage` probe actors get no policy, as `EG-C` decides.
- The server copies the default into the actor's policy at create. After that, the policy belongs to
  the environment. `SetEnvironmentEgressPolicy` replaces it as it replaces any other policy.

### What applies to an environment

- `GetServerInfo` lists the feature `default_egress_policy` when the flag is set.
- The response of `GetEnvironmentEgressPolicy` gains a boolean, `server_default`.
- `GetEnvironmentEgressPolicy` on an environment with no policy still returns `NotFound` (`EG-12`).
  Such an environment was created before the default was configured.

How does the server know that a policy is the default? Substrate's `EgressPolicy` has rules and the
common metadata. It has no labels and no annotations, so the policy itself cannot carry a mark.

3 options were considered:

- Compare: The server compares the stored rules with its default when it answers a get.
- Store: The server records the source in the environment store's object at create.
- None: The server only lists the feature. The client cannot tell a default from an explicit policy.

| **Properties ↓ Option →**                                     | **Compare**                                                                   | **Store**                                      | **None** |
| ------------------------------------------------------------- | ----------------------------------------------------------------------------- | ---------------------------------------------- | -------- |
| **Works without the environment store?**                      | Yes.                                                                          | No. It needs `--environment-store=configmaps`. | Yes.     |
| **Extra writes on a create that uses the default?**           | None.                                                                         | One ConfigMap for each environment.            | None.    |
| **Still correct after the operator changes the default?**     | No. An environment created under the old default reports `false`.             | Yes.                                           | n/a      |
| **Tells a default from an explicit policy with equal rules?** | No. Both report `true`. The environment reaches the same hosts in both cases. | Yes.                                           | No.      |
| **A caller can tell what its code can reach?**                | Yes.                                                                          | Yes, where the store is on.                    | No.      |

Evaluation: Store is exact, but it makes every environment pay for a ConfigMap, which the
environment metadata plan avoids. None does not meet CUJ 3.

Decision (proposed, `EG-J`): Compare. `server_default` means "the rules equal the server's default
now". The server compares the rules as a set, after the port defaults of `EG-13` are applied.

This document reflects the Compare option.

### A change of the default

Suppose the default allows `pypi.org`, and a caller creates environment `a` without a policy. The
operator then changes the file to also allow `github.com` and applies the manifest. The Deployment
restarts. The caller creates environment `b` without a policy.

- `b` gets the new default. Its get reports `server_default` as true.
- `a` keeps the policy of its create. It cannot resolve `github.com`, and its get now reports
  `server_default` as false.
- In order to move `a` to the new rules, a caller sends them with `SetEnvironmentEgressPolicy`.

The server does not rewrite the policies of existing environments. It cannot tell an old default
from an explicit policy, and a rewrite would change the network of a running episode.

### Harbor and other integrations

The `public` mode of the Harbor mapping (#47) creates an environment without `egress_policy`, so it
gets the default. On a server that does not list `default_egress_policy`, it gets no policy, and
Substrate denies its egress. #47 decides whether that fails the task. This plan gives it the feature
name to check.

### Security

A default that allows hosts applies to every environment the server creates, in every atespace, for
the whole life of the environment. Code in an environment is untrusted. It can reach every host the
default allows, and it can send data there.

- The default is off unless the operator sets the flag.
- An explicit policy narrows one environment, down to deny-all.
- The help text of both flags and the threat model say that the default should name the hosts tasks
  install from, and should avoid `*`.
- The pull request that adds the flag updates the egress rows of the
  [threat model](../../security/threat-model.md).

## Out of scope

- A default for each atespace or for each caller. It can come with per-caller authorization (known
  gap 2), which defines who may act in which atespace.
- A default on the Python fleet configuration. It needs the Python SDK of `EG-16` first, and it
  belongs to the fleet SDK plan.
- A reload of the file without a restart.
- An RPC that returns the default itself.
- Paths that create actors without `ate-env-api`, such as the benchmark driver that calls Substrate
  directly.

## Decisions

Proposed, to be decided in the review:

- **EG-F:** One default for the service. The server has no notion of who may use which atespace
  until per-caller authorization lands.
- **EG-G:** A changed default applies to new environments only.
- **EG-H:** A default that allows hosts is an operator choice. It is off by default, and the threat
  model documents what it exposes.
- **EG-I:** No default on the fleet configuration in this plan.
- **EG-J:** `server_default` is computed by comparing the stored rules with the default.

## Testing

Unit tests run against `fakecontrol`, as in the per-environment plan. The Kind E2E job deploys
`ate-env-api` with a default that allows one name. `EG-30` checks DNS only, for the reason the
[per-environment plan](./README.md#testing) gives. The other e2e tests do not depend on egress, so
the default does not change them.

## Pull requests

One change per pull request, after pull requests 2 and 3 of the
[per-environment plan](./README.md#pull-requests):

1. `--default-egress-policy-file` on `ate-env-api`, the default on create, and the
   `default_egress_policy` feature.
2. `server_default` on `GetEnvironmentEgressPolicy`.
3. `ate-env manifest --default-egress-policy-file`.
4. Go and Python SDKs: `server_default`.
5. Threat model and deployment docs.
6. Kind e2e test.
