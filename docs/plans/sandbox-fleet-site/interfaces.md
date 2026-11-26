# Interface contracts: sandbox fleet site

Status: accepted (#223); last checked 2026-11-26 against `7c04afa`. The snapshot schema in the next
section is code since pull request 1 (#226, `internal/inventory`): its object kinds, field names and
rules are fixed, and a change to them is a schema change. The names in the other sections (bucket
layout, reporter flags and identity, event stream and tables, collector pipelines, site routes and
environment variables, named queries, Terraform) are proposals until their pull request lands; the
lead confirms them and records changes in [decisions.md](./decisions.md).

## Bucket layout

```
gs://<bucket>/v1/inventory/<project>/<cluster>/latest.json          rewritten every interval by that cluster's reporter, its only writer
gs://<bucket>/v1/snapshots/<project>/<cluster>/<ts>.json            hourly, lifecycle 30 days (its own prefix, so no rule can touch latest.json)
gs://<bucket>/v1/events/<dt=YYYY-MM-DD>/...                          Cloud Storage subscription output, lifecycle 400 days
gs://<bucket>/v1/traces/year=<YYYY>/month=<MM>/day=<DD>/spans_<uuid>.gz   google_cloud_storage exporter output, one OTLP/JSON export request per object, gzip
gs://<bucket>/v1/runs/<run_id>/                                      RunReport .txt and .json, spans.jsonl, run.json and report.html (FS-11 lists the three steps)
gs://<bench bucket>/<YYYY-MM-DD>_<model-family>-<size>_<dataset>_<nickname>[_traced]/   benchmark bundles from bench upload (#270)
```

The site discovers clusters by listing `v1/inventory/*/*/latest.json`. There is no shared index
object, so no two writers touch one object. `<project>` and `<cluster>` are one path segment each:
the reporter, the snapshot schema and the decoder refuse a value that holds `/` or `\` or is `.` or
`..`, so the name always has this depth and stays under the cluster's own prefix.

## Snapshot schema, `internal/inventory`

```json
{
  "schema_version": "v1",
  "generated_at": "2026-11-03T12:00:05Z",
  "project": "example-project",
  "cluster": "bench-1",
  "location": "us-central1",
  "reporter": {
    "version": "v0.1.0",
    "commit": "abc1234",
    "interval_s": 5
  },
  "atespaces": [
    {
      "name": "ate-env",
      "create_time": "2026-11-03T12:00:00Z"
    }
  ],
  "templates": [
    {
      "atespace": "ate-env",
      "name": "default-template",
      "guest_image": "example.com/ate-env-guest@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "workerpool": "default-template-workerpool",
      "golden_state": "none",
      "create_time": "2026-11-03T12:00:00Z"
    },
    {
      "atespace": "ate-env",
      "name": "default-template-1a2b3c4d5e6f",
      "base": "default-template",
      "rootfs_image": "example.com/python@sha256:1a2b3c4d5e6f0000000000000000000000000000000000000000000000000000",
      "guest_image": "example.com/ate-env-guest@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "workerpool": "default-template-workerpool",
      "golden_state": "ready",
      "create_time": "2026-11-03T12:00:01Z"
    }
  ],
  "tags": [
    {
      "atespace": "ate-env",
      "name": "golden-default-template-1a2b3c4d5e6f",
      "source_actor": "golden-7f3e",
      "create_time": "2026-11-03T12:00:02Z"
    }
  ],
  "workerpools": [
    {
      "namespace": "ate-env",
      "name": "default-template-workerpool",
      "replicas": 4,
      "ready_replicas": 4,
      "sandbox_class": "gvisor",
      "sandbox_config": "gvisor-default",
      "worker_image": "example.com/ateworker-gvisor@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    }
  ],
  "workers": [
    {
      "name": "w-0a1b2c",
      "namespace": "ate-env",
      "pool": "default-template-workerpool",
      "pod": "default-template-workerpool-7c9d8-x2k4q",
      "node": "gke-bench-1-pool-1-abcd",
      "state": "active",
      "capacity": {
        "cpu_m": 8000,
        "memory_bytes": 34359738368
      },
      "allocated": {
        "cpu_m": 2000,
        "memory_bytes": 4294967296
      },
      "labels": {
        "ate-env/workerpool": "default-template-workerpool"
      }
    }
  ],
  "actors": [
    {
      "uid": "8f2a0001",
      "atespace": "ate-env",
      "name": "env-001",
      "template": "default-template-1a2b3c4d5e6f",
      "state": "running",
      "assigned_node": "gke-bench-1-pool-1-abcd",
      "worker": "w-0a1b2c",
      "worker_selector": {
        "ate-env/workerpool": "default-template-workerpool"
      },
      "create_time": "2026-11-03T12:00:03Z",
      "update_time": "2026-11-03T12:00:04Z",
      "labels": {
        "example.com/run-id": "r-1"
      }
    },
    {
      "uid": "8f2a0002",
      "atespace": "ate-env",
      "name": "env-002",
      "template": "default-template-1a2b3c4d5e6f",
      "source_tag": "golden-default-template-1a2b3c4d5e6f",
      "state": "crashed",
      "create_time": "2026-11-03T12:00:03Z",
      "update_time": "2026-11-03T12:00:05Z",
      "crash": {
        "message": "atelet: restore failed: snapshot download timed out",
        "crash_time": "2026-11-03T12:00:05Z"
      }
    }
  ],
  "deployments": [
    {
      "namespace": "ate-env",
      "name": "ate-env-api",
      "image": "example.com/ate-env-api@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      "version": "v0.1.0",
      "ready": 1,
      "tracing": true,
      "metrics_port": 9090
    }
  ],
  "counts": {
    "actors_by_state": {
      "crashed": 1,
      "running": 1
    },
    "actors_by_atespace": {
      "ate-env": 2
    }
  }
}
```

Added in pull request 2, additive to v1: `workers[].labels`, the labels `ListWorkers` returns,
because a placement domain is the distinct label set of a pool's workers, as `ListPlacementDomains`
computes it; and `actors[].worker_selector`, the per-environment selector from #207. Both are
`$defs/Labels` in the schema, as `actors[].labels` is.

Rules: timestamps RFC 3339 UTC; `state` is the `ateapi.ActorState` value lowercased, the same
vocabulary as the event stream; `worker` and `name` of workers are opaque control-plane names, never
parsed; a label or selector key that names a credential (`token`, `password`, `authorization`,
`secret`, `credential`, in any letter case) is refused by both validators, and the reporter leaves
such a key out. Substrate's actors carry no labels at the version `go.mod` requires, so
`actors[].labels` stays empty until they do. The environment labels of
[environment-metadata.md](../ate-env-api/environment-metadata.md) are a second possible source: its
store is merged (#407), its label field on `CreateEnvironment` is not, and no pull request of this
plan reads the store.

## Reporter, `cmd/ate-env-reporter`

| Flag                                | Default                                 | Meaning                                                                                                                                           |
| ----------------------------------- | --------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-bucket`                           | required unless `-out-dir` is set       | destination bucket; exactly one of `-bucket` and `-out-dir` is set                                                                                |
| `-project`, `-cluster`, `-location` | from the GKE metadata server when empty | snapshot identity; off GKE, `-project` and `-cluster` are required (`DecodeSnapshot` rejects a snapshot without them) and `-location` stays empty |
| `-interval`                         | `5s`                                    | time from one tick's start to the next, at least 1 s; the wait doubles with each failed tick, up to 60 s                                          |
| `-snapshot-interval`                | `1h`                                    | how often `latest.json` is also kept under `snapshots/`, as `<YYYYMMDD>T<HHMMSS>Z.json`; the first tick keeps one                                 |
| `-out-dir`                          | empty                                   | write the bucket layout (`v1/inventory/`, `v1/snapshots/`) under a local directory (unit tests, the Kind end-to-end case)                         |
| `-ateapi`, `-ateapi-token-file`     | as `ate-env-api`                        | Substrate control plane                                                                                                                           |
| `-skip-verify`                      | `true`, as `ate-env-api`                | skip TLS verification of the control plane                                                                                                        |
| `-health-listen-addr`               | `:9091`                                 | `/healthz`, and `/readyz`, which answers 200 while the last tick wrote its snapshot                                                               |
| `-enable-tracing`                   | `false`, as `ate-env-api`               | one root span `ate-env-reporter.tick` per tick over OTLP/gRPC (`OTEL_EXPORTER_OTLP_*`), with the `ateapi` calls under it                          |
| `-metrics-listen-addr`              | empty: off, as `ate-env-api`            | `/metrics` on its own listener, no authentication: `ate_env_reporter_poll_duration_seconds{status}`, `ok` or `error`                              |

Pull request 2c added `-enable-tracing` and `-metrics-listen-addr`. The histogram uses the bucket
set of every `internal/telemetry` histogram, 50 ms to 10 min, and observes each tick that ends
before shutdown; a tick that times out at 60 s counts as `error`. `ate-env manifest reporter`
renders them with `--enable-tracing`, `--otlp-endpoint`, `--otlp-insecure`, `--metrics-port`
(container port `metrics`; the health port 9091 is refused) and `--pod-monitoring`, which needs
`--metrics-port`; `--otlp-endpoint` and `--otlp-insecure` need `--enable-tracing`. FS-15 covers the
flags. The `PodMonitoring` is the one of `ate-env manifest --pod-monitoring` with the reporter's
name and namespace: selector `app: ate-env-reporter`, port `metrics`, path `/metrics`, interval
`5s`. See [decisions.md](./decisions.md).

Identity (pull request 2b, `ate-env manifest reporter`): Kubernetes ServiceAccount
`ate-env-reporter` in namespace `ate-env-reporter` labeled `ate.dev/api-access=true`; projected
token with audience `api.ate-system.svc`; Workload Identity to
`ate-env-reporter@<project>.iam.gserviceaccount.com` with `roles/storage.objectUser` on the bucket,
restricted by an IAM condition to `v1/inventory/<project>/<cluster>/` and
`v1/snapshots/<project>/<cluster>/` (an overwrite needs `storage.objects.delete` next to `create`);
the principal `user:system:serviceaccount:ate-env-reporter:ate-env-reporter` merged into the global
AccessPolicy's `viewer` binding (one object for the whole control plane:
`ate-env manifest reporter --access-policy` prints the binding and the operator merges it with
`kubectl ate get access-policy --global -o yaml` and
`kubectl ate update access-policy --global -f <file>`, as the README describes for `ate-env-api`'s
`ListPlacementDomains`, or creates the policy from that output with
`kubectl ate create access-policy --global -f -` when the control plane has none); ClusterRole with
`list` on `workerpools.ate.dev` and `deployments.apps`, the two kinds the reporter reads.

Behavior: one `ListActors` across atespaces and one `ListWorkers` per tick, paged at 1000;
`ListAtespaces`, `ListActorTemplates` and `ListTags` every tenth tick, or at once when an actor
names an atespace or a template the last read lacks (one that the read does not resolve, a template
deleted under its actors, does not force another); one list of WorkerPools and one of Deployments
per tick, from the API server's cache. A Deployment is taken when it has a container named
`ate-env-api`. Each tick rewrites `latest.json` with one atomic object write, even when nothing but
`generated_at` changed, so the object generation and the JSON `generated_at` move together and the
site revalidates by generation. A tick that cannot read a source writes nothing and is retried. A
tick whose kept copy fails has written `latest.json` and still counts as failed. The reporter never
calls `ate-env-api`.

Tests: unit tests against `internal/internaltest/fakecontrol`, which has `ListAtespaces`,
`ListActorTemplates`, `ListTags` and `AddTag` since pull request 2, and against the fake Kubernetes
clientsets; pull request 2b adds one end-to-end case in `hack/ci/e2e.py` that runs the reporter with
`-out-dir`, `-project` and `-cluster` against the Kind Substrate and accepts the output with
`DecodeSnapshot` (FS-14).

## Event stream

Substrate's records as documented in its `docs/observability.md`:

| Record                | `event.name`              | Attributes                                                                                                                                                            |
| --------------------- | ------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Actor state changed` | `ate.actor.state_changed` | `ate.atespace`, `ate.actor.name`, `ate.actor.uid`, `ate.template.atespace`, `ate.template.name`, `ate.actor.operation.name`, `ate.actor.state`; `trace_id`, `span_id` |
| `Actor crashed`       | `ate.actor.crashed`       | the same                                                                                                                                                              |
| `Actor usage sampled` | `ate.actor.usage_sampled` | identity keys, `ate.workerpool.*`, `ate.sandbox.class`, `ate.stats.*`, `ate.actor.epoch`                                                                              |

Phase 1 sink filter, one sink per cluster project, destination the central topic:

```
resource.type="k8s_container"
resource.labels.namespace_name="ate-system"
resource.labels.container_name="ate-api-server"
(jsonPayload.msg="Actor state changed" OR jsonPayload.msg="Actor crashed")
```

BigQuery table `fleet.actor_events` from the Pub/Sub BigQuery subscription without a topic schema:
`subscription_name`, `message_id`, `publish_time`, `data` (JSON, the raw payload: the Cloud Logging
`LogEntry` in phase 1, the OTLP log record in phase 2) and `attributes` (JSON). A topic schema maps
top-level fields only and cannot flatten either payload. The typed columns `project`, `cluster`,
`time`, `atespace`, `actor_name`, `actor_uid`, `template_atespace`, `template_name`, `operation`,
`state`, `trace_id` and `span_id` are the view `fleet.actor_events_v`, projected from `data` with
`JSON_VALUE`; project and cluster come from the log entry resource labels. The site queries the
view. Phase 2 adds a second branch to the view for the OTLP record shape.

Phase 2: the collector receives the OTLP log events and routes `event.name` in
`{ate.actor.state_changed, ate.actor.crashed}` to `googlecloudpubsub` with no sampling and no
filtering processor on that pipeline, and `ate.actor.usage_sampled` to GCS only.

## Collector pipelines, `ate-env manifest collector`

Pull request 3 renders the traces row as two pipelines, `traces/cloudtrace` with the default batch
and `traces/files` with a 30 s batch, both after `memory_limiter` and `resource/gcp_project_id`
(`upsert`); the metrics row is not rendered, because PodMonitoring covers `ate-env-api`; the logs
pipeline comes with phase 7. The image is `otel/opentelemetry-collector-contrib` 0.165.0, pinned by
digest, whose component names are the ones below. Flags: `--bucket` and `--project` (required; the
project is `gcp.project_id` and the project of `google_client_auth`), `--namespace`
(`ate-env-collector`), `--image`, `--gcp-service-account`. Kubernetes names: ServiceAccount,
ConfigMap, Deployment and Service `ate-env-collector`; endpoint
`http://ate-env-collector.ate-env-collector:4317`.

| Pipeline | Receivers                                                                  | Exporters                                                                                                                                                                                                |
| -------- | -------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| traces   | `otlp` (4317, 4318)                                                        | `otlp_http/cloudtrace` to `https://telemetry.googleapis.com` with `google_client_auth`, `google_cloud_storage` (`reuse_if_exists`, partition prefix `v1/traces`, format `year=%Y/month=%m/day=%d`, gzip) |
| metrics  | `prometheus` scraping `ate-env-api` only when PodMonitoring is unavailable | `googlemanagedprometheus` or none                                                                                                                                                                        |
| logs     | `otlp`                                                                     | `googlecloudpubsub` for the two lifecycle events; `google_cloud_storage` for usage samples                                                                                                               |

The collector's service account holds `roles/cloudtrace.agent`, `roles/storage.objectCreator` and
`storage.buckets.get` (for example `roles/storage.legacyBucketReader`) on the bucket, because the
exporter reads the bucket's attributes before it starts, and in phase 7 `roles/pubsub.publisher` on
the topic.

PodMonitoring, `ate-env manifest --metrics-port <port> --pod-monitoring`:
`monitoring.googleapis.com/v1`, name `ate-env-api` in the deployment's namespace, selector
`app: ate-env-api`, one endpoint on port `metrics`, path `/metrics`, interval `5s`.

## Site, `cmd/ate-env-fleet-site`

Go, standard library HTTP, server-rendered HTML (`html/template`) with inline SVG, one stylesheet
served by the site, no script and no external assets, the same zero-dependency style as the explorer
plan and `build_report.py`. The code is `internal/fleetsite`; pull request 4 shipped the Fleet and
Cluster pages and the first three API routes, pull request 5a the Actor and Live pages, the Live API
and the metrics API, and pull request 5b the Runs pages and API.

Configuration, each flag with an environment variable for Cloud Run:

| Flag               | Environment             | Meaning                                                                                        |
| ------------------ | ----------------------- | ---------------------------------------------------------------------------------------------- |
| `-bucket`          | `FLEET_BUCKET`          | the inventory bucket; exactly one of `-bucket` and `-dir`                                      |
| `-dir`             |                         | the bucket layout under a local directory, as `ate-env-reporter -out-dir` writes it            |
| `-events-table`    | `FLEET_EVENTS_TABLE`    | the view `<project>.fleet.actor_events_v`; required with IAP; the query runs in `<project>`    |
| `-iap-audience`    | `FLEET_IAP_AUDIENCE`    | `/projects/<number>/locations/<region>/services/<service>`; required unless `-insecure-no-iap` |
| `-insecure-no-iap` |                         | serve without an IAP assertion, for a local run; `-listen` must then be a loopback address     |
| `-listen`          | `PORT`                  | `:$PORT`, else `:8080`                                                                         |
| `-bench-buckets`   | `FLEET_BENCH_BUCKETS`   | comma-separated benchmark buckets for the Runs page; with `-dir`, directories under it         |
| `-trace-project`   | `FLEET_TRACE_PROJECT`   | the project of a trainer run's Cloud Trace link when its span file names none                  |
| `-metrics-project` | `FLEET_METRICS_PROJECT` | the project whose metrics scope holds the clusters' projects; empty turns the metrics API off  |

Runs page sources (pull request 5b): `v1/runs/<run_id>/` in the inventory bucket for trainer runs,
where a run id is one path segment of `[A-Za-z0-9._-]`, at most 128, and the benchmark buckets in
`FLEET_BENCH_BUCKETS` (comma-separated), where a run is a top-level folder whose name passes
`schema.CheckFolderName` (`internal/benchmarking/schema`, the form `bench`'s `ValidateGCSFolderName`
checks) and is at most 128 characters. A run's URL names its source, `/runs/trainer/<id>` or
`/runs/bench/<bucket>/<id>`, so a bundle name in two buckets or equal to a trainer run id stays
reachable; a run page reads its folder from the store, not from the index. The index lists the first
500 of each source by name, descending (bundle names start with their date, so they list newest
first; a trainer run id need not), is cached 60 s per instance from the end of its listing, and
shows a source it cannot list as an error row. The listing runs apart from the request that started
it, under a 20 s listing timeout, shorter than the 30 s request timeout, so a viewer who closes the
tab neither fails it nor caches the cancellation; concurrent requests share one listing,
`?nocache=1` waits for one that starts after it, and a listing whose own timeout expired is shown
but not cached. A request whose own deadline ends its wait first (a `?nocache=1` behind a listing in
flight) answers 502. A trainer run page shows each RunReport JSON (the files named as
`RunReport.write` names them, at most 20, each at most 1 MiB; one that is not JSON, such as Python's
`NaN`, is a note), a link to `report.html`, the Cloud Trace link of the latest `ate_env_fleet.run`
span in `spans.jsonl` (the project is that span's `gcp.project_id` resource attribute, else
`cloud.account.id`, else `FLEET_TRACE_PROJECT`; without one the trace id shows as text) and a
waterfall of that trace's first 300 spans: each bar is labeled with its span name after the last
slash, colored by service with a legend, and an error bar is hatched and its label starts with a
cross (FS-19). A bundle page shows `run_window.json`, `report.json` and `system_description.json` as
indented JSON (at most 256 KiB each) and marks a `_traced` folder. What a run page shows is built
once per change of the run's file listing, cached 60 s, shared by concurrent views, and at most two
are built at a time. A run file is served from `/runs/<source>/files/<name>` only when the run's
listing has it, always as a download, `report.html` too, so no script from a run folder runs on the
site's origin. A run file is read up to 64 MiB.

| Route                                                                                       | Returns                                                                                                                                                                                                    | Pull request |
| ------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------ |
| `GET /`                                                                                     | Fleet page; per cluster the WorkerPool totals and at most 10 pools, short ones first, with a link to the Cluster page for the rest (FS-17)                                                                 | 4, 4b        |
| `GET /clusters/{project}/{cluster}`                                                         | Cluster page; `?atespace=&template=&state=&node=` filter the actors, the first 1000 shown                                                                                                                  | 4            |
| `GET /actors/{uid}`                                                                         | Actor page: the actor in the snapshots, its transitions since `?since=` (default 24 h) with a Cloud Trace link each, its crash record; the events route's limit                                            | 5a           |
| `GET /runs`, `GET /runs/trainer/{id}`, `GET /runs/bench/{bucket}/{id}`                      | the run index; one trainer run or one bundle (FS-11)                                                                                                                                                       | 5b, 5c       |
| `GET /runs/trainer/{id}/files/{name}`, `GET /runs/bench/{bucket}/{id}/files/{name}`         | one listed file of the run, as a download                                                                                                                                                                  | 5b           |
| `GET /api/v1/runs`, `GET /api/v1/runs/trainer/{id}`, `GET /api/v1/runs/bench/{bucket}/{id}` | the index `{"runs", "truncated", "errors"}`; one run with its files, reports, trace and bundle files                                                                                                       | 5b           |
| `GET /live`                                                                                 | Live page: the newest 200 transitions of the last hour, newest first; reloads every 10 s with `<meta http-equiv="refresh">`, no script                                                                     | 5a           |
| `GET /api/v1/live`                                                                          | `{"queried_at", "events"}`: the Live page's transitions                                                                                                                                                    | 5a           |
| `GET /api/v1/clusters`                                                                      | `{"clusters": [...]}`, one row per `latest.json`: project, cluster, location, `generated_at`, `age_s`, `stale`, reporter, counts, deployments, WorkerPools, `error`                                        | 4            |
| `GET /api/v1/clusters/{project}/{cluster}`                                                  | `{"project", "cluster", "generation", "age_s", "stale", "error", "snapshot"}`, where `snapshot` is the object as the reporter wrote it                                                                     | 4            |
| `GET /api/v1/actors/{uid}/events?since=`                                                    | `{"uid", "since", "events", "truncated"}`: rows of `actor_events_v`, oldest first, the newest 1000; `since` RFC 3339, default 24 h ago, at most 30 days ago                                                | 4            |
| `GET /api/v1/metrics/{name}?project=&cluster=&namespace=&window=`                           | `{"name", "project", "cluster", "namespace", "window", "query", "data"}`: a named query of the allow-list as a range query, `data` as the Prometheus API returns it; `window` 15m, 1h (default), 6h or 24h | 5a           |
| `GET /healthz`                                                                              | 200, without IAP                                                                                                                                                                                           | 4            |

`?nocache=1` on the cluster routes and the Actor page lists the bucket at once (FS-1). A cluster is
stale when its write time was older than three of its reporter's `interval_s` at the listing that
read it, is older than that plus 60 s now (the listing itself is old), is more than a minute ahead
of the site's clock, or when its object cannot be read or decoded; such a row carries `error` and no
snapshot (FS-9). The write time is the object's update time from the bucket, so a reporter whose
clock is off neither looks stale nor hides; a local `-dir` has none and uses `generated_at`. `age_s`
is the age by the same time. The site ignores a field it does not know and checks the rest as the
schema and `Snapshot.Validate` do, so a later `v1` field can reach the reporters before the site is
deployed again; the reporter's side keeps the strict `inventory.DecodeSnapshot`. A snapshot whose
`project` or `cluster` differs from its object path is such an error.

Auth: IAP in front; the service verifies `x-goog-iap-jwt-assertion` on every request but `/healthz`:
`ES256`, IAP's keys, the audience, issuer `https://cloud.google.com/iap`, `exp`, `iat`, and an
`email`, which the pages show. Any failure answers 403. Methods other than `GET` and `HEAD` answer
405; each user gets 10 requests a second per instance, burst 40, and on the events route 1 a second,
burst 5. Service account roles: `roles/storage.objectViewer` on the bucket,
`roles/bigquery.dataViewer` on the events dataset, `roles/bigquery.jobUser` in the central project
(it runs the query jobs), `roles/monitoring.viewer` on the metrics scope. No other role.

Caching: snapshots cached in memory per instance for 30 s and revalidated by object generation; a
refresh runs apart from the request that started it, other requests get the last listing meanwhile
if it is younger than 60 s and wait for the refresh otherwise, `?nocache=1` waits for a refresh that
started after it, and a snapshot whose read failed is read again on the next refresh. PromQL results
and errors are cached 10 s per request (name, project, cluster, namespace, window), counted from the
end of the upstream call, so a call slower than 10 s is cached too; concurrent requests for one
query share one upstream call, and each user gets 2 metrics requests a second, burst 10. The Live
query (`publish_time` in the last hour, 200 rows) runs one at a time and its result or its error is
cached 10 s from the end of the query for every viewer of an instance, so the self-reloading page
makes one BigQuery job per 10 s, not one per viewer, also while BigQuery fails. A trace link is
`https://console.cloud.google.com/traces/list?project=<project>&tid=<trace_id>`, made only for a
well-formed project and a 32-hex trace id. Event queries: `publish_time >= @since`, 24 h by default;
`fleet.actor_events` is partitioned by `publish_time`, so the query scans only those days; each job
may bill 10 GiB at most and returns the newest 1000 rows, oldest first.

## Named PromQL queries

| Name                  | Query                                                                                                                                                                                                                                                                                                                          |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `requests_rate`       | `sum by (project_id, location, cluster, namespace, method) (rate(ate_env_api_requests_total[5m]))`                                                                                                                                                                                                                             |
| `create_to_ready_p50` | `histogram_quantile(0.5, sum by (project_id, location, cluster, namespace, le) (rate(ate_env_api_create_to_ready_seconds_bucket[5m])))`                                                                                                                                                                                        |
| `wake_wait_p95`       | `histogram_quantile(0.95, sum by (project_id, location, cluster, namespace, le) (rate(ate_env_api_wake_wait_seconds_bucket[5m])))`                                                                                                                                                                                             |
| `environments`        | `max by (project_id, location, cluster, namespace, atespace, state) (ate_env_api_environments)` (each replica reports the whole count)                                                                                                                                                                                         |
| `workers`             | `sum by (project_id, location, cluster, ate_workerpool_namespace, ate_workerpool_name) (avg by (project_id, location, cluster, ate_workerpool_namespace, ate_workerpool_name, ate_worker_state, ate_sandbox_class) (<copy>({"ate.workerpool.workers", "ate.worker.state"!="unschedulable", "ate.sandbox.class"!="unknown"})))` |
| `crashes_rate`        | `sum by (project_id, location, cluster, ate_template_name) (<copy>(rate({"ate.actor.crashes"}[5m])))`                                                                                                                                                                                                                          |

`<copy>` is one `label_replace(e, "a_b_c", "$1", "a.b.c", "(.*)")` for each dotted label the query
groups by: Managed Service for Prometheus rejects a dotted label inside `by (...)`, as Substrate's
`docs/examples/gmp-alerting-rules.yaml` notes.

Two paths reach Cloud Monitoring, with two naming schemes. `ate-env-api` is scraped by its
`PodMonitoring` in the Prometheus exposition format, so its series have Prometheus names
(`ate_env_api_requests_total`). Substrate's `ateapi` pushes OTLP through GKE Managed OpenTelemetry,
and Cloud Monitoring keeps the OpenTelemetry names as they are: dotted, without a `_total` suffix,
with dotted attribute keys (`ate.workerpool.namespace`, `ate.worker.state`, `ate.sandbox.class`,
`ate.template.name`). A query selects such a series by its quoted name, `{"ate.workerpool.workers"}`
(Substrate `docs/dev/best-practices/metrics.md`); with the Prometheus-style name the query returns
an empty result, not an error.

`workers` is the workers per pool that can take actors. Every `ateapi` replica reports the whole
fleet from its own worker cache, and two caches can disagree for a few seconds on a worker's state.
The query averages across the replicas first, keeping the state and the sandbox class, then adds, as
Substrate's metrics registry says for `ate.workerpool.workers`: a `max` would count a worker that
changed state under both states. It leaves out the `unschedulable` state and the `unknown` sandbox
class, which are not capacity.

Every rate range is 5 m, no shorter than the longest step of the windows below, so a range query
sees every sample. Pull request 5a ships the allow-list in `internal/fleetsite/metrics.go`
(`NamedQueries`, `MetricsWindows`); the names are not yet confirmed against a metrics explorer,
which needs the first deployment (FS-6).

Every query groups by `project_id` and `location`, the labels Managed Service for Prometheus adds,
next to `cluster`, so two clusters of one name in two projects stay two series. Filters become label
matchers:

- `project` on `project_id`, a Google Cloud project id, also a domain-scoped one
  (`example.com:my-project`), as `ate-env manifest collector` takes it;
- `cluster` on `cluster`;
- `namespace` on `namespace` for the four `ate_env_api_*` queries, the namespace of the
  `ate-env-api` deployment; on `"ate.workerpool.namespace"` for `workers` and `crashes_rate`, the
  namespace of the WorkerPool. A crash before the actor has a worker carries an empty WorkerPool
  namespace, so a namespace filter on `crashes_rate` leaves it out.

A cluster or namespace value must be a Kubernetes or GKE name (`[a-z0-9]`, `-` inside, at most 63),
so it cannot leave its matcher. The windows are 15m, 1h, 6h and 24h with steps of 15 s, 30 s, 3 m
and 5 m: 60 to 288 points, each step no longer than the 5 m rate range. A result or an upstream
error is cached 10 s per request from the end of the call; the per-user limit counts every metrics
request, a cached one too.

## Terraform, `deploy/fleet-site/`

Modules: `central` (bucket, lifecycle, topic, subscriptions, dataset, Cloud Run, IAP, service
accounts) applied once; `cluster` (sink, reporter and collector service accounts, Workload Identity
bindings) applied per cluster project. Inputs: `central_project`, `bucket`, `iap_group`,
`clusters = [{project, name, location}]`.
