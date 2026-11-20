## Why

<!--
What problem or motivation does this PR address?
- Why is this change needed now?
- Which issue(s) does it resolve or relate to? (Write `Fixes #<num>` only if this PR fully resolves the issue, or `Ref #<num>` otherwise.)
-->

## What

<!--
What changes in this PR, and what intentionally stays the same?
- Which components, RPCs, CLI flags, or SDK methods are added or modified?
- Are there any behavior, configuration, or compatibility changes callers should know about?
-->

## Criteria

<!--
Which acceptance criteria does this PR meet, change or add? One per line, as IDs from
docs/plans/*/acceptance-criteria.md with what changed, e.g. `AE-4 met`, `NG-12 partial`,
`PP-2 added`. Write `None: <reason>` for a chore, a dependency bump, a docs-only or a test-only
change. A PR that changes behavior under a plan's scope must name a criterion or say why none
applies (see AGENTS.md, Workflow).
-->

## Tests

<!--
How was this change verified?
- Which unit, integration, or end-to-end tests did you run, and what were the results?
- If tested against a live cluster (Kind or GKE), which scenarios were exercised?
-->

## Release note

<!--
If this PR has user-visible changes (features, fixes, image/layer updates, or breaking changes),
summarize them below for users upgrading `sandboxenv-rl`. Write `NONE` if no release note is needed.
For breaking changes, describe the required upgrade steps clearly.
-->
```release-note

```

## Security impact

Does this change add or widen any of these?

- [ ] Network surface: a listener, a route, a proxy, a new caller of a port
- [ ] Privileges: of `ate-env-api`, of the guest, or in the access policy or the manifest
- [ ] Wire or API fields: a protobuf field, an RPC, a header, a CLI flag that changes defaults
- [ ] Dependencies: a new module, package, base image or GitHub Action
- [ ] None of the above

If you checked one of the first four boxes, link a short design note with the threat it adds and
its mitigation, and update
[`docs/security/threat-model.md`](https://github.com/gke-labs/env-rl/blob/main/docs/security/threat-model.md)
in this pull request.
