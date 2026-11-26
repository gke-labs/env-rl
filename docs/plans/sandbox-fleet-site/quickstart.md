# Quickstart: deploy the fleet site

Status: plan-stage runbook, written 2026-11-04; last checked 2026-11-26 against `7df41c1`. The
Terraform in pull request 6 of the [execution plan](./execution-plan.md) replaces the `gcloud`
commands below with one `apply` per project; `docs/fleet-site.md` then becomes the user-facing
guide. Until then this is the order of operations and the exact resources, with the open parameters
as variables. Steps marked "after PR n" need a component that pull request n adds.

```bash
export CENTRAL_PROJECT=...      # hosts the bucket, the topic, BigQuery and Cloud Run
export REGION=us-central1
export BUCKET=...               # one bucket, uniform access
export IAP_GROUP=group:...      # who may open the site
export CLUSTER_PROJECT=...      # repeat the per-cluster section for each cluster project
export CLUSTER=...              # GKE cluster name, also the reporter's cluster label
export CLUSTER_LOCATION=...     # the cluster's region or zone
export BENCH_BUCKET=...         # optional: the bucket, or comma-separated buckets, bench upload writes bundles to
```

## 1. Central project, once

Enable the services:

```bash
gcloud services enable run.googleapis.com iap.googleapis.com pubsub.googleapis.com \
  bigquery.googleapis.com storage.googleapis.com monitoring.googleapis.com \
  logging.googleapis.com artifactregistry.googleapis.com telemetry.googleapis.com --project "$CENTRAL_PROJECT"
gcloud artifacts repositories create ate-env --repository-format docker \
  --location "$REGION" --project "$CENTRAL_PROJECT"
```

Bucket, with the lifecycle rules from [interfaces.md](./interfaces.md):

```bash
gcloud storage buckets create "gs://$BUCKET" --project "$CENTRAL_PROJECT" \
  --location "$REGION" --uniform-bucket-level-access --public-access-prevention
cat > /tmp/lifecycle.json <<'EOF'
{"rule": [
  {"action": {"type": "Delete"}, "condition": {"age": 30,  "matchesPrefix": ["v1/snapshots/"]}},
  {"action": {"type": "Delete"}, "condition": {"age": 400, "matchesPrefix": ["v1/events/"]}}
]}
EOF
gcloud storage buckets update "gs://$BUCKET" --lifecycle-file /tmp/lifecycle.json
```

`latest.json` lives under `v1/inventory/`, which no rule matches, so a reporter that stops for a
month still leaves its last snapshot in place; the site shows it as stale. Timestamped snapshots
have their own prefix, `v1/snapshots/`, for the 30-day rule.

Topic, dataset and the two subscriptions. Pub/Sub checks the permissions of its service agent when
it creates an export subscription, so the agent and its grants come before the subscriptions:

```bash
gcloud pubsub topics create ate-actor-events --project "$CENTRAL_PROJECT"
bq --project_id "$CENTRAL_PROJECT" mk --dataset --location "$REGION" fleet
# Partitioned by publish_time, so the site's events query, bounded by publish_time, scans only
# the days it asks for, not the whole table.
bq --project_id "$CENTRAL_PROJECT" mk --table --time_partitioning_field publish_time \
  --time_partitioning_type DAY fleet.actor_events \
  subscription_name:STRING,message_id:STRING,publish_time:TIMESTAMP,data:JSON,attributes:JSON
gcloud services identity create --service pubsub.googleapis.com --project "$CENTRAL_PROJECT"
NUMBER=$(gcloud projects describe "$CENTRAL_PROJECT" --format 'value(projectNumber)')
PUBSUB_SA="serviceAccount:service-$NUMBER@gcp-sa-pubsub.iam.gserviceaccount.com"
bq add-iam-policy-binding --member "$PUBSUB_SA" --role roles/bigquery.dataEditor "$CENTRAL_PROJECT:fleet"
gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" --member "$PUBSUB_SA" --role roles/storage.objectCreator
gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" --member "$PUBSUB_SA" --role roles/storage.legacyBucketReader
gcloud pubsub subscriptions create ate-actor-events-bq --topic ate-actor-events \
  --project "$CENTRAL_PROJECT" --bigquery-table "$CENTRAL_PROJECT:fleet.actor_events" \
  --write-metadata --expiration-period never
gcloud pubsub subscriptions create ate-actor-events-gcs --topic ate-actor-events \
  --project "$CENTRAL_PROJECT" --cloud-storage-bucket "$BUCKET" \
  --cloud-storage-file-prefix v1/events/ --cloud-storage-file-suffix .jsonl \
  --cloud-storage-max-duration 1m --expiration-period never
```

The typed view the site queries. `data` holds the Cloud Logging entry, so the keys with dots are
quoted in the JSONPath:

```bash
cat > /tmp/actor_events_v.sql <<'SQL'
CREATE OR REPLACE VIEW fleet.actor_events_v AS
SELECT publish_time,
  JSON_VALUE(data, '$.resource.labels.project_id')                 AS project,
  JSON_VALUE(data, '$.resource.labels.cluster_name')               AS cluster,
  TIMESTAMP(JSON_VALUE(data, '$.timestamp'))                       AS time,
  JSON_VALUE(data, '$.jsonPayload."ate.atespace"')                 AS atespace,
  JSON_VALUE(data, '$.jsonPayload."ate.actor.name"')               AS actor_name,
  JSON_VALUE(data, '$.jsonPayload."ate.actor.uid"')                AS actor_uid,
  JSON_VALUE(data, '$.jsonPayload."ate.template.atespace"')        AS template_atespace,
  JSON_VALUE(data, '$.jsonPayload."ate.template.name"')            AS template_name,
  JSON_VALUE(data, '$.jsonPayload."ate.actor.operation.name"')     AS operation,
  JSON_VALUE(data, '$.jsonPayload."ate.actor.state"')              AS state,
  JSON_VALUE(data, '$.jsonPayload.trace_id')                       AS trace_id,
  JSON_VALUE(data, '$.jsonPayload.span_id')                        AS span_id,
  data
FROM fleet.actor_events;
SQL
bq --project_id "$CENTRAL_PROJECT" query --use_legacy_sql=false < /tmp/actor_events_v.sql
```

Phase 1 writes the Cloud Logging entry into `data`, and the view above projects the typed columns of
[interfaces.md](./interfaces.md) from it. Phase 2 writes the OTLP log record into the same column
and adds a second branch to the view for that shape.

Service accounts and their only roles:

```bash
for sa in ate-fleet-site ate-env-reporter ate-env-collector; do
  gcloud iam service-accounts create "$sa" --project "$CENTRAL_PROJECT"
done
P="$CENTRAL_PROJECT.iam.gserviceaccount.com"
gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" --member "serviceAccount:ate-fleet-site@$P"    --role roles/storage.objectViewer
IFS=, read -ra BENCH_BUCKETS <<<"${BENCH_BUCKET:-}"   # one grant per bucket; FLEET_BENCH_BUCKETS is comma-separated
for b in "${BENCH_BUCKETS[@]}"; do
  [ -n "$b" ] && gcloud storage buckets add-iam-policy-binding "gs://$b" --member "serviceAccount:ate-fleet-site@$P" --role roles/storage.objectViewer
done
gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" --member "serviceAccount:ate-env-collector@$P" --role roles/storage.objectCreator
# The span exporter reads the bucket's attributes before it starts (storage.buckets.get).
gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" --member "serviceAccount:ate-env-collector@$P" --role roles/storage.legacyBucketReader
gcloud projects add-iam-policy-binding "$CENTRAL_PROJECT" --member "serviceAccount:ate-fleet-site@$P" --role roles/monitoring.viewer
gcloud projects add-iam-policy-binding "$CENTRAL_PROJECT" --member "serviceAccount:ate-fleet-site@$P" --role roles/bigquery.jobUser
bq add-iam-policy-binding --member "serviceAccount:ate-fleet-site@$P" --role roles/bigquery.dataViewer "$CENTRAL_PROJECT:fleet"
gcloud pubsub topics add-iam-policy-binding ate-actor-events --project "$CENTRAL_PROJECT" \
  --member "serviceAccount:ate-env-collector@$P" --role roles/pubsub.publisher
```

The site account has no write role anywhere; that is the read-only guarantee of
[acceptance criterion FS-3](./acceptance-criteria.md). Multi-project metrics need a metrics scope on
`CENTRAL_PROJECT` that includes each cluster project; add them in Cloud Monitoring settings.

## 2. Each cluster project

Events, phase 1: a log sink from `ateapi`'s stdout to the central topic.

```bash
gcloud logging sinks create ate-actor-events \
  "pubsub.googleapis.com/projects/$CENTRAL_PROJECT/topics/ate-actor-events" \
  --project "$CLUSTER_PROJECT" --log-filter \
  'resource.type="k8s_container" AND resource.labels.namespace_name="ate-system" AND resource.labels.container_name="ate-api-server" AND (jsonPayload.msg="Actor state changed" OR jsonPayload.msg="Actor crashed")'
WRITER=$(gcloud logging sinks describe ate-actor-events --project "$CLUSTER_PROJECT" --format 'value(writerIdentity)')
gcloud pubsub topics add-iam-policy-binding ate-actor-events --project "$CENTRAL_PROJECT" \
  --member "$WRITER" --role roles/pubsub.publisher
```

Workload Identity for the two in-cluster components (after PR 2 and PR 3, which add the Deployments;
the namespaces and Kubernetes service accounts are `ate-env-reporter/ate-env-reporter` and
`ate-env-collector/ate-env-collector`):

```bash
for pair in "ate-env-reporter:ate-env-reporter:ate-env-reporter" "ate-env-collector:ate-env-collector:ate-env-collector"; do
  IFS=: read -r sa ns ksa <<<"$pair"
  gcloud iam service-accounts add-iam-policy-binding "$sa@$P" --project "$CENTRAL_PROJECT" \
    --role roles/iam.workloadIdentityUser --member "serviceAccount:$CLUSTER_PROJECT.svc.id.goog[$ns/$ksa]"
done
gcloud projects add-iam-policy-binding "$CLUSTER_PROJECT" --member "serviceAccount:ate-env-collector@$P" --role roles/cloudtrace.agent
# The collector exports through the Telemetry API; enable it here and in the central project, which owns its account.
gcloud services enable telemetry.googleapis.com cloudtrace.googleapis.com --project "$CLUSTER_PROJECT"
```

The reporter writes under two prefixes of its own cluster. Overwriting `latest.json` needs
`storage.objects.delete` as well as `storage.objects.create`, so the grant is
`roles/storage.objectUser` with an IAM condition on those prefixes. The condition needs the uniform
bucket-level access that section 1 sets; the Terraform can tighten the role to a custom one with the
two permissions:

```bash
gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" --member "serviceAccount:ate-env-reporter@$P" \
  --role roles/storage.objectUser \
  --condition "title=reporter-$CLUSTER_PROJECT-$CLUSTER,expression=resource.name.startsWith(\"projects/_/buckets/$BUCKET/objects/v1/inventory/$CLUSTER_PROJECT/$CLUSTER/\") || resource.name.startsWith(\"projects/_/buckets/$BUCKET/objects/v1/snapshots/$CLUSTER_PROJECT/$CLUSTER/\")"
```

Managed Prometheus, on a Standard cluster (Autopilot has it on):

```bash
gcloud container clusters update "$CLUSTER" --project "$CLUSTER_PROJECT" --location "$CLUSTER_LOCATION" --enable-managed-prometheus
```

In the cluster, after the components exist:

- PR 3:
  `ate-env manifest collector --bucket "$BUCKET" --project "$CLUSTER_PROJECT" --gcp-service-account "ate-env-collector@$P" | kubectl apply -f -`,
  and in each `ate-env` namespace the `PodMonitoring` that
  `ate-env manifest --metrics-port 9090 --pod-monitoring` renders, with `--enable-tracing`,
  `--otlp-endpoint http://ate-env-collector.ate-env-collector:4317` and `--otlp-insecure` so
  `ate-env-api` sends its spans to the collector.
- PR 2 (no reporter image is published yet; build one with
  `KO_DOCKER_REPO=... ko build -B ./cmd/ate-env-reporter`):
  `ate-env manifest reporter --image "$REPORTER_IMAGE" --bucket "$BUCKET" --project "$CLUSTER_PROJECT" --cluster "$CLUSTER" --gcp-service-account "ate-env-reporter@$P" | kubectl apply -f -`
  (from PR 2c, add `--metrics-port 9090 --pod-monitoring` for its tick durations and
  `--enable-tracing --otlp-endpoint http://ate-env-collector.ate-env-collector:4317 --otlp-insecure`
  for its tick spans), then merge `user:system:serviceaccount:ate-env-reporter:ate-env-reporter`
  into the global AccessPolicy's `viewer` binding with
  `kubectl ate get access-policy --global -o yaml` and
  `kubectl ate update access-policy --global -f <file>` (both commands refuse to run without `-a` or
  `--global`), the way the README does it for `ate-env-api`'s `ListPlacementDomains`
  (`ate-env manifest access-policy --global` prints that binding). The global policy is one object;
  never create a second one.
- Check:
  `gcloud storage cat "gs://$BUCKET/v1/inventory/$CLUSTER_PROJECT/$CLUSTER/latest.json" | head`
  shows `generated_at` within one interval of now, and
  `gcloud storage objects describe "gs://$BUCKET/v1/inventory/$CLUSTER_PROJECT/$CLUSTER/latest.json" --format 'value(generation)'`
  returns a new value one interval later. A grant without `storage.objects.delete` passes the first
  write and fails the second with HTTP 403.

## 3. The site, once (after PR 4)

Build and deploy with IAP on the service itself, which covers the `run.app` URL as well as any load
balancer:

```bash
KO_DOCKER_REPO="$REGION-docker.pkg.dev/$CENTRAL_PROJECT/ate-env" ko build -B ./cmd/ate-env-fleet-site
gcloud services identity create --service iap.googleapis.com --project "$CENTRAL_PROJECT"
NUMBER=$(gcloud projects describe "$CENTRAL_PROJECT" --format 'value(projectNumber)')
# The site refuses a request whose IAP assertion is not for this audience.
AUDIENCE="/projects/$NUMBER/locations/$REGION/services/ate-fleet-site"
gcloud run deploy ate-fleet-site --project "$CENTRAL_PROJECT" --region "$REGION" \
  --image "$REGION-docker.pkg.dev/$CENTRAL_PROJECT/ate-env/ate-env-fleet-site:latest" \
  --service-account "ate-fleet-site@$P" --no-allow-unauthenticated --iap --min-instances 1 \
  --memory 2Gi --concurrency 20 \
  --set-env-vars "^;^FLEET_BUCKET=$BUCKET;FLEET_EVENTS_TABLE=$CENTRAL_PROJECT.fleet.actor_events_v;FLEET_IAP_AUDIENCE=$AUDIENCE;GOMEMLIMIT=1600MiB;FLEET_METRICS_PROJECT=$CENTRAL_PROJECT;FLEET_BENCH_BUCKETS=${BENCH_BUCKET:-}"
gcloud run services add-iam-policy-binding ate-fleet-site --project "$CENTRAL_PROJECT" --region "$REGION" \
  --member "serviceAccount:service-$NUMBER@gcp-sa-iap.iam.gserviceaccount.com" --role roles/run.invoker
gcloud iap web add-iam-policy-binding --project "$CENTRAL_PROJECT" --region "$REGION" \
  --resource-type cloud-run --service ate-fleet-site --member "$IAP_GROUP" --role roles/iap.httpsResourceAccessor
```

The `^;^` prefix makes `;` the pair separator of `--set-env-vars`, whose default is `,`;
`FLEET_BENCH_BUCKETS` is itself comma-separated ([interfaces.md](./interfaces.md)), so a
`BENCH_BUCKET` with more than one bucket would otherwise split into bogus pairs. Section 1 grants
the site account `roles/storage.objectViewer` on each bucket of that list; pull request 6 checks
both ends with a two-bucket `BENCH_BUCKET`.

IAP on Cloud Run authenticates users of the organization with a Google-managed OAuth client; a
project without an organization needs the console once. `FLEET_EVENTS_TABLE` names the typed view of
section 1, not the raw table. The site reads `FLEET_METRICS_PROJECT` from PR 5a on, for the metrics
API; `FLEET_BENCH_BUCKETS` from PR 5b on, for the Runs page; add `FLEET_TRACE_PROJECT=<project>`
when trainers' span files name no `gcp.project_id`, so their runs still get a Cloud Trace link.
`--memory 2Gi` holds the decoded snapshots of the cluster list and a refresh: one snapshot of
100,000 actors is about 40 MB as JSON and takes some 600 MB once decoded. Memory grows with the sum
of the clusters' decoded snapshots, plus a second copy of the changed ones during a refresh.
`GOMEMLIMIT=1600MiB` makes the Go runtime collect before the instance reaches its limit when many
large downloads run at once; raise both together for a larger fleet.

For a look before the deployment, run the site on a workstation over a reporter's `-out-dir` output:
`go run ./cmd/ate-env-fleet-site -dir /tmp/snap -insecure-no-iap -listen 127.0.0.1:8080`.

## 4. Verify

- `curl -i "$(gcloud run services describe ate-fleet-site --region "$REGION" --format 'value(status.url)')"`
  without credentials redirects to sign-in; a group member gets the Fleet page (FS-7).
- The Fleet page lists each cluster with a `generated_at` under one interval old (FS-1), and marks a
  cluster stale three intervals after its reporter stops (FS-9).
- `bq --project_id "$CENTRAL_PROJECT" query --use_legacy_sql=false 'SELECT COUNT(*) FROM fleet.actor_events WHERE publish_time > TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 10 MINUTE)'`
  grows while a fleet runs (FS-2).
- The IAM policy of `ate-fleet-site@` shows viewer roles only (FS-3).

## 5. Remove

Delete the Cloud Run service, the sinks in each cluster project, the subscriptions, the topic, the
dataset and the bucket, in that order; the reporters and collectors go with `kubectl delete -f` of
the same manifests. Nothing else was created.

References:
[IAP for Cloud Run](https://docs.cloud.google.com/run/docs/securing/identity-aware-proxy-cloud-run),
[Pub/Sub subscriptions create](https://docs.cloud.google.com/sdk/gcloud/reference/pubsub/subscriptions/create),
[Cloud Storage subscriptions](https://docs.cloud.google.com/pubsub/docs/create-cloudstorage-subscription),
[BigQuery subscriptions](https://docs.cloud.google.com/pubsub/docs/create-bigquery-subscription).
