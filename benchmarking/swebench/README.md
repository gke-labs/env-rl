# Simulated RL benchmark on Substrate (swebench-loader)

This page is about one tool, `swebench-loader`: a Go program that plays the
part of an RL trainer, with real task images and no model. By default it
drives the ate-env API (`-access ate-env-api`; `-access sdk` takes the same
path); `-access direct` dials Substrate's control plane and router instead,
and `-backend agent-sandbox` drives an Agent Sandbox gateway. It creates
sandboxes, runs commands in them, parks and wakes them, deletes them, and
writes one event per operation with the time it took. It never talks to
Kubernetes and it does not use Ray. The real-RL benchmark, which `bench`
measures around a verl trainer that runs on Ray, is a different page:
[`cmd/benchmarking/bench/README.md`](../../cmd/benchmarking/bench/README.md).

## What this benchmark tests

Four costs of a sandbox platform, each isolated by a run shape. The numbers
are wall time as a trainer sees it, API server and network included.

| Question | Run shape | The numbers it produces |
|---|---|---|
| Cold restore: how long until a sandbox whose image is on no node can run a command | `-mode eval`, one task per image or many, on nodes that have not seen the images | `ttfic` (the ask to the first command's answer), `create`, the restore phases from atelet (download, unpack, restore), pulls per start from the node cache samples |
| Warm restore: the same when the image is already on the node | `-mode eval` with several tasks per image on a fixed pool, or a second pass | `ttfic` with unpack near zero; the gap between it and atelet's restore is the control plane |
| Park and wake: what it costs to suspend a fleet and resume it for the next step | `-mode steps -park suspend`, with `-park-each` for a fleet larger than the running slots | `suspend`, `wake` p50/p95, `step_barrier` (first task start to last task end in a step), and with `-restore-log` the restore phases of each wake; a `download` phase in a wake's record is the only trace of a move to another node |
| Image delivery: pull against streaming or another loader | `-mode eval` with the `workload-*` trajectories | `ttfic` per start and the read step's time, under the same command per mechanism |

`-park suspend` and `suspend-sync` are durable suspends: the call returns once the snapshot is in
object storage. `-park suspend-async` is a local suspend. The call returns once the snapshot is on
the node, and the upload goes on in the background, so the `suspend` event measures the checkpoint
without the upload; so do the `park` event and the `park_s` column, the wall time of a step's
suspend calls, and with `-park-each` the barrier, which includes them. Until the upload lands, the
environment resumes on that node only. Through ate-env-api the local suspend is the Go SDK's
`SuspendLocal`, which needs a server that serves `suspend_mode`; the run checks that before it
starts, for `suspend-async` and for `fork`, whose park is a local suspend too. With `-access direct`
the loader sends Substrate's asynchronous durability mode itself.

Every run also gives throughput, failures by cause, and the per-step table
in steps mode. `eval` and `steps` are closed-loop: tasks per second follow
from the width and the task time. `-mode open-loop -arrival-rate` sets a
Poisson rate in tasks per second instead.

The sandbox command in every shape is the trajectory. The built-in
`workload-file-read-one` runs `true` (what `ttfic` and `wake` measure: the
first instant the sandbox executes anything) and reads one file, thus the
numbers are the platform's. `swebench-gold` replays a
SWE-bench solution and grades it, for correctness regressions.

## The two standard runs

`SKILL.md` in this directory names them with their manifests, flags, labels
and current baselines; this page explains the flags. In short: the standard
run is SWE-bench Verified (445 images) in two arms, fresh sandboxes every
step and a parked fleet woken every step; the image-delivery standard is a
94-image manifest at 1.3 to 1.5 starts in flight per node (width 3 on two
nodes, 30 on hundreds), a start arm and a read arm. A quick run is:

```bash
kubectl -n ate-env port-forward svc/ate-env-api 7777:7777 &
export SUBSTRATE_ENV_AUTH_TOKEN="$(kubectl -n ate-env get secret ate-env-api-token -o jsonpath='{.data.token}' | base64 -d)"
go run ./cmd/benchmarking/swebench-loader run -dataset /tmp/swebench-verified.jsonl \
  -exclude-file benchmarking/swebench/offline-exclusions.txt \
  -images 5 -tasks-per-image 1 -width 5 -run-id smoke -out /tmp/smoke.jsonl
go run ./cmd/benchmarking/swebench-loader report /tmp/smoke.jsonl
```

## Where things are

- `cmd/benchmarking/swebench-loader`: the binary (`run`, `render-plan`, `report`, `compare`, `resolve`, `plan-placement`, `convert-trace`, `trajectory-assets`).
- `internal/benchmarking/swebench`: the library, tested against an in-memory backend.
- `internal/benchmarking/swebench/trajectories/`: the built-in trajectories (YAML).
- This directory: `run-in-cluster.sh` (runs the loader as a Job next to the
  API), `make-dataset.py` (builds a manifest), `mirror-images.sh`,
  `sample-node-cache.sh` and `harvest-restore-timings.sh` (the two Substrate-side
  measurements), `offline-exclusions.txt`; `run-r2e-swebench.sh` and
  `run_fleet_bench.py` (the mixed R2E-Gym and SWE-bench sweeps, Go and Python
  `SandboxFleet` arms; see the last section and [`r2e-swebench/SKILL.md`](./r2e-swebench/SKILL.md)).

The rest of this page, by heading: Workloads; What is measured; Cold boot,
cold and warm restore, suspend and wake: which shape measures what; How many
actors, and on which clusters; Prepare once per project; Run; Image-delivery
workloads; Network and the offline exclusion list; Two things the first runs
showed; Record with every run; R2E-Gym and SWE-bench sweeps.

## Workloads

Images, fan-out, width, steps, strategy, and trajectory are all flags, so a
smoke test and a 500-wide run differ only in numbers.

| Shape | Command / flags | What happens |
|---|---|---|
| mixed R2E-Gym + SWE-bench strategy & client sweep | `benchmarking/swebench/run-r2e-swebench.sh run --r2e-dataset r2e.jsonl --swe-dataset swebench.jsonl --concurrency 200 --images 20 --rollouts-per-image 5 --client all --strategy all` | Drives concurrent `job=r2e` (`r2e-gold.yaml`) and `job=swe` (`swebench-gold`) shards across `--client <go\|go-direct\|python\|all>` and `--strategy <none\|naive\|sliding\|pipelined\|all>`, emitting per-arm `bench report` bundles under `$ARTIFACT_DIR/arms/<arm>/` and `comparison.{txt,json,md}`. |
| single-shard strategy sweep (`none`, `naive`, `sliding`, `pipelined`) | `-mode steps -steps 1 -images 20 -rollouts 5 -width 100 -strategy pipelined -window-size 8` | Single-pass sweep of the four warm-pool orchestration strategies on `swebench-loader`: `none` (on-demand create and delete per rollout), `naive` (prewarm and suspend all `images * rollouts` sandboxes upfront), `sliding` (warm `window-size` distinct images, execute, retire, advance), or `pipelined` (double-buffer window `N+1` prefetch behind window `N` execution while sharing `-width`). |
| eval (SWE-bench Verified evaluation) | `-mode eval -images 500 -tasks-per-image 6 -width 500` | 500 in flight. Each task: create from the image, replay the trajectory, record the reward, delete. With the offline exclusion list the 500 become 445 images and 2,670 tasks; the loader prints the reduced count. |
| steps (training loop) | `-mode steps -images 500 -rollouts 2 -steps 8 -park suspend` | 1,000 environments created once. Each step replays the trajectory on all of them, waits for the last (the barrier), then parks the fleet. `-park` is `suspend`, `suspend-sync`, `suspend-async`, `fork`, `delete` or `none`. |
| steps, parked one by one (fleet larger than the slots) | `-mode steps -images 500 -rollouts 16 -steps 8 -park suspend -width 64 -park-each` | 8,000 environments, created on their first step. Each environment is parked as soon as its own step ends instead of after the whole fleet's, so at most 64 are awake at once and the fleet can be many times the running slots: the shape of a trainer that releases a sandbox at the end of its episode, and the way to measure how many parked environments a node can hold. The barrier includes the parking and there is no `park` event. On the last step each environment is deleted right after its replay instead of parked (unless `-keep`), so the run ends with no cleanup burst. With `-park suspend-async` each parked environment stays pinned to its node until its upload lands; a wake that finds no room on that node pays the detach (Substrate issue #3482; `docs/plans/preflight-warmer/README.md`). |
| steps with a window (pools per step) | `-mode steps -images-per-step 32 -rollouts 16 -steps 6 -park delete` | Each step takes the next 32 images (wrapping around the dataset), creates 16 fresh environments per image, 512 in all, runs the step behind a barrier, and retires them. Nothing survives a step, so every task pays the cold path, and a new image is a new derived template and a golden bake on first use: the shape of a trainer that creates and retires pools every step. Run several loaders at once, one run id each, for several jobs on one cluster. |

The trajectory is YAML (`internal/benchmarking/swebench/trajectories/`,
or `-trajectory FILE`). The built-in `swebench-gold` does what a SWE agent
does on a task, with the gold patch as the model's edit: explore, read, write
the patch, `git apply`, write the harness eval script, run it. The reward is
the SWE-bench verdict: `swebench-grade.py` (written into the sandbox by the
trajectory) parses the log with the repo's harness parser and checks the
instance's FAIL_TO_PASS and PASS_TO_PASS tests, the same rules the harness
applies. Neither the script's nor the test command's exit code is the
verdict: unrelated tests in the same files fail routinely. A healthy run
rewards every task and a drop in reward rate is a correctness regression.
`python3 -m pytest benchmarking/swebench/tests` covers the grader. Steps carry think times, the
simulated policy latency between commands.

## What is measured

One JSONL event per operation, wall time as the loader sees it (network and
API server included, which is what a trainer sees):

| Event | Meaning |
|---|---|
| `create` | CreateEnvironment call |
| `ttfi` | the ask for a sandbox to the first command being issued (time to first instruction) |
| `ttfic` | the ask to the first command's return (time to first instruction complete): the boot or restore wait lives here |
| `wake` | first command after a suspend park, including the wake it triggers |
| `step` (by name) | each trajectory step, with exit code; the reward step carries the reward and an output tail |
| `task` | one replay end to end, ok and reward |
| `suspend`, `delete` | the calls. The step loop's park (`-mode steps` without `-strategy`, `-schedule`, `-tasks-per-step`, `-group-size`, `-step-pause` or `-prefetch`) sends a `suspend` again when it fails with a transient error (gRPC `Unavailable`, `DeadlineExceeded`, `Unknown`, `Internal` or `Aborted`), after the `-wake-backoff` pauses and until the next pause would end after `-wake-budget`; the event covers every attempt and the pauses, carries `attempts`, and counts as `retried` in the report (and as `recovered` under `snapshot_restore_failure` in the fault table). There is no run-wide limit: each failing suspend keeps its `-width` slot for up to `-wake-budget`, so in a control-plane outage parking `N` environments at width `W` takes about `ceil(N / W)` times that budget (about `N / W * 174 s` with the defaults, as the first-command retry does). Through ate-env-api a `delete` also includes its wait in the `-delete-rate` queue and any retry backoff, and so does the `park_s` of a `-park delete` run: the standard run's startup arm retires 890 environments per step, and at 200 per second with a burst of 200 the queue alone adds at least 3.4 s per step |
| `step_barrier` | steps mode: first task start to last task end in a training step |
| `park` | steps mode: parking the whole fleet; absent with `-park-each`, where the barrier includes the parking |
| `run` | the whole run |

`swebench-loader report RUN.jsonl -json summary.json` prints count, failed,
p50, p95 and max per event, a per-step table (tasks, barrier, park, wake
p50/p95) and the most frequent errors. The JSON is the artifact to diff
between runs. Given several files, `report a.jsonl b.jsonl` prints one table
with a column per run: p50/p95 per event, and per step the live environments,
tasks, failures, barrier, park and wake p50/p95. `-json` then writes the
summaries as an array. Files go before the flags.

Every run event carries labels: the loader's own (mode, width or domain
width, images, trajectory, dataset, the shape flags, connections, delete rate, base
template, exclude file, instances and the timeouts) plus any
`run -label KEY=VALUE`, which is where the Substrate commit, the cluster and
the arm go. A `-label` with a key the loader records itself is refused, so
the same-workload check below cannot be overridden. The report prints
them under its header, and the comparison shows one row per label key, so a
column's provenance is in the table. `report -arm pull=p1.jsonl,p2.jsonl
-arm stream=s1.jsonl,s2.jsonl` gives one column per arm, in flag order. Two
rules pool an arm's reps: counts add up (tasks, failed, rewarded, a step's
live environments, and the `n` of every event row, whose percentiles are
computed over the merged raw events, never over per-run percentiles), and
durations take the median across the reps (a step's barrier and park, and
the run), the mean of the two middle values for an even count. A `reps` row
says how many runs each column pools, and the JSON lists their run ids. The
reps of an arm must be complete runs of one workload: each with a run event
and the loader's labels, no run id twice or in two arms, and the loader's
own labels equal (`exclude_file` and `instances` compare by name or count
plus a digest of their ids); a `-label` value that differs across them shows
every value `|`-joined, so a column that mixes commits says so. An operator
label named like a built-in row (`tasks`, say) prints as `label.tasks`. A
path in `-arm` may not contain a comma. `report RUN.jsonl
-top 10` lists the ten slowest successful first commands and the ten slowest
successful wakes by environment, for finding the straggler.

## Cold boot, cold and warm restore, suspend and wake: which shape measures what

Four costs hide behind the events, and the run shape decides which ones a
run can see. Substrate boots an image once per cluster, when the first actor
of that image is created: that boot is the golden bake, and its snapshot is
what every later actor of the image restores from. The bake takes no
warmup: every template the env API derives has a wakeup probe on the guest,
so the golden is taken as soon as the guest answers `/readyz`, and nothing
of the trajectory runs inside it. A restore lands on whichever node has
room; if that node has never pulled the image, the restore pulls it first.

| Cost | What it is | Where it shows | How to make a run measure it |
|---|---|---|---|
| Cold boot | the golden bake, the first boot of an image on a cluster | the first `ttfic` of each image when no golden exists yet; the first command is retried until the bake is done | fresh cluster, or delete the derived templates, then `-mode eval -tasks-per-image 1`; a second pass of the same command is restore only, and the difference is the bake |
| Cold restore | restore from the golden onto a node that has not pulled the image | `ttfic` with a long `oci_unpack` phase in the restore record; a miss in the node cache delta | a fresh node pool and `-tasks-per-image 1`, so no node has seen any image |
| Warm restore | restore from the golden onto a node that has the image | `ttfic` with `oci_unpack` near zero; a cache hit | a second pass, or `-tasks-per-image 2` and more: later creates of an image tend to land where it already is |
| Suspend and wake | park the fleet, resume it on the next step's first command | `suspend` and `park`, then `wake`; the restore record of a wake has no download phase when the node still holds the snapshot | `-mode steps -park suspend`; eval never suspends |

By shape:

- `-mode eval` is create, replay, delete: cold boot on the first create per
  image, cold or warm restore on every create, never suspend or wake.
- `-mode steps -park suspend` is one create per environment, then suspend and
  wake at every step boundary: the restore cost once, the wake cost per step.
- `-mode steps -park delete`, and `-images-per-step`, recreate every step:
  the restore cost per step, cold or warm by the node caches, never a wake.

Warm-up is a property of the node caches, not of the mode. On a fixed node
pool, the share of cold restores falls as the nodes accumulate images,
whatever the shape: in a 50-step window run over 445 images and 40 nodes,
the step time fell from 118 s to about 20 s by step 33. To hold a comparison
to one state, recreate the node pool before each repetition for cold, or run
a warm-up pass first for warm, and read the cache delta and the restore
phases (`-cache-before`, `-cache-after`, `-restore-log`) to confirm which
state the run was in rather than assuming it.

What the loader cannot do: run the workload inside the golden bake. A golden
that already performed a read, so that later actors restore with a warm page
cache, is a template whose sidecar does the read before reporting ready
(`ate-env manifest template --sidecar` and `--sidecar-readyz`), not a loader
flag.

## How many actors, and on which clusters

Every environment the loader creates is one Substrate actor. Four flags
set how many, and one paces their deletes:

| Flag | Controls |
|---|---|
| `-width` | actors in flight at once, and by default the cap on parallel creates |
| `-create-width` | steps: cap on concurrent cold starts (each create through its first command, released before `Step.Think`) when lower than `-width` (`0` uses `-width`). Lets a parked fleet pull images at `-create-width` while keeping `-width` environments awake during think time and wakes. Recorded as `create_width` |
| `-domain-width` | eval with `-placement`: actors in flight at once in each placement domain, in place of `-width`; the run has up to N times the number of domains in flight |
| `-delete-rate` | most environment deletes per second through ate-env-api, per loader process (several loaders at once add their caps up), 200 by default; 0 removes the cap. The bucket rate-limits snapshot deletes, and a delete it refuses (429, `ResourceExhausted`) is retried with backoff for up to two minutes. The queue wait and the backoff count in the `delete` event and in the `park_s` of a `-park delete` run; the run records the cap as the `delete_rate` label |
| `-tasks-per-image` | eval: actors created per image, each a full create, replay, delete |
| `-task-order` | eval: `image` (default) runs an image's replays back to back, which with `-placement` and no `-domain-width` sends them all to one node at once; `round` runs replay k of every image before replay k+1, spreading one image's creates over the run (not with `-strategy`). With `-domain-width` the order applies in each domain's queue. An image with no more replays than domains has at most one replay in each domain, and `round` then spreads nothing for it. A `round` run records the `task_order` label |
| `-rollouts` | steps: actors per image kept alive across the training steps |

So `-images 445` is many images with one actor each, `-images 1
-tasks-per-image 2000 -width 500` is one image with 2,000 actors 500 at a
time, and `-mode steps -rollouts 2 -steps 8` is both with a park between
steps. Above a few hundred in flight, raise the API server's database
connection pool: a burst of about 900 creates exhausted the default.

The loader does not decide placement. Whether several actors share one
worker pod is the WorkerPool's capacity on the cluster; configure it there
and read the packing from Substrate's metrics, not from the report.

The loader never talks to Kubernetes. `-endpoint` is an ate-env API address,
so "which cluster" is "which API". To compare clusters (builds, node images,
image delivery) run the same manifest, trajectory and flags once per
cluster, with `CONTEXT=<kubectl context>` pointing the in-cluster script at
each cluster and the cluster's name in the run id, then put the events files
side by side with `swebench-loader report a.jsonl b.jsonl`. Each cluster
needs the full setup below: Substrate and the ate-env API installed, the
images reachable from it, and the golden tags baked there (a golden tag is
per cluster, so the first run on a fresh cluster pays the bake for every
image).
One run never spans clusters.

The JSONL events file is the source of truth so a run works without a
collector. Passing `-enable-tracing` also exports every event as an
OpenTelemetry span over OTLP/gRPC under one `swebench-loader.run` span per run,
configured by `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_EXPORTER_OTLP_INSECURE`.

## Prepare once per project

1. Install workstation tools and verify `ate-env` on the cluster. On the
   workstation, install Go (`1.27` or later, matching `go.mod`), `kubectl`,
   `kubectl-ate`, `ko`, `crane`, Docker registry credentials, `docker`
   ([`./run-r2e-swebench.sh`](./run-r2e-swebench.sh) uses `docker` to build the
   `in-cluster` `--client python` image when `--python-image` is omitted), and a
   Python `3.10+` virtual environment outside the git checkout so generated
   files keep the working tree clean for `ko build`:

   ```bash
   go install github.com/google/ko@latest
   go install github.com/google/go-containerregistry/cmd/crane@latest
   gcloud auth configure-docker us-central1-docker.pkg.dev
   python3 -m venv /tmp/swe-venv
   SETUPTOOLS_SCM_PRETEND_VERSION=0.1.0 /tmp/swe-venv/bin/pip install \
     -e ./clients/python datasets 'swebench>=5'
   ```

   `./clients/python` installs `ate-env-rl` (`ate_env_rl`, `grpcio>=1.85.1`,
   `protobuf>=7.35`) for `dry-run` and `--client python`.
   `SETUPTOOLS_SCM_PRETEND_VERSION=0.1.0` supplies a release version in shallow
   clones without git tags. `datasets` and `swebench>=5` drive
   [`./make-dataset.py`](./make-dataset.py) and
   [`./make-r2e-dataset.py`](./make-r2e-dataset.py).

   On the cluster (see [`../../README.md#deployment-guide`](../../README.md#deployment-guide)),
   ensure Agent Substrate includes `spiffe://cluster.local/ns/ate-env/sa/ate-env-api`
   in `--ingress-allowed-spiffe-ids` and runs worker images from
   `go run ./cmd/ate-setup publish worker-images` in the Substrate checkout.
   Ensure the `ate-env` atespace has its editor policy
   (`ate-env manifest access-policy`), plus the global viewer binding
   (`ate-env manifest access-policy --global` merged into the global policy)
   when calling `ListPlacementDomains` (`ate-env placement-domains`) or
   `WarmImage` (`ate-env warm-image` with `--nodes 0`). Deploy `ate-env-api`
   with its `ate-env-api-token` `Secret` (`ate-env manifest auth-secret`) and
   register a base `ActorTemplate` (`ate-env manifest template`) with CPU and
   memory limits. Per task image, `ate-env-api` derives one template from that
   base on first use and bakes its golden snapshot.

2. Set up an image mirror source in the cluster's region. Worker nodes pull
   task images under their node service account. Public Docker Hub rate-limits
   unauthenticated pulls, and cross-region pulls add latency to cold starts and
   golden bakes. Store or proxy task images in a regional Artifact Registry
   repository and grant the node service account
   `roles/artifactregistry.reader`:
   - **Existing single-repository mirror**: when a source repository already
     stores every instance as a `sweb.eval.x86_64.<instance_id>` tag in one
     repository, list its tags to `/tmp/tags.txt` (excluding `-zran` variants)
     and copy them by digest into a regional standard repository with
     [`./mirror-images.sh`](./mirror-images.sh):

     ```bash
     SRC=SOURCE_REGISTRY/swebench-verified
     DST=us-central1-docker.pkg.dev/PROJECT/swebench/swebench-verified
     crane ls "${SRC}" | grep -v -- '-zran$' > /tmp/tags.txt
     benchmarking/swebench/mirror-images.sh "${SRC}" "${DST}" /tmp/tags.txt 8
     ```

   - **Public Docker Hub source**: upstream SWE-bench Verified publishes 500
     separate repositories
     (`docker.io/swebench/sweb.eval.x86_64.<instance_id_1776>:latest`, one
     repository per instance), so `crane ls` on one repository cannot list
     them. Create a regional Artifact Registry remote repository backed by
     Docker Hub:

     ```bash
     gcloud artifacts repositories create dockerhub-remote \
       --repository-format=docker \
       --mode=remote-repository \
       --location=us-central1 \
       --remote-docker-repo=docker-hub
     ```

     Attach upstream Docker Hub credentials (`--remote-username` and
     `--remote-password-secret-version`) before pulling hundreds of distinct
     images. The remote repository resolves digests on demand and caches layers
     in the region on first pull. For R2E-Gym tags,
     [`../../integrations/uni-agent/scripts/resolve_images.sh`](../../integrations/uni-agent/scripts/resolve_images.sh)
     supports default `MODE=proxy` (resolves digests through the remote
     repository), `MODE=hubapi` (queries Docker Hub's tags API directly and
     prefixes the remote repository path), and `MODE=mirror` (copies images by
     digest into a standard Artifact Registry repository).
   - **In-cluster loader helper images**:
     [`./run-in-cluster.sh`](./run-in-cluster.sh) and
     [`./run-r2e-swebench.sh`](./run-r2e-swebench.sh) start a `keep` sidecar
     from `busybox:1.36` (pinned by digest on Docker Hub), and `--client python`
     builds `FROM python:3.12-slim` unless `--python-image` provides a prebuilt
     image. Private GKE pools without Cloud NAT need egress for Docker Hub
     pulls (plus `--python-image` when the workstation cannot pull
     `python:3.12-slim`).

3. Build the manifest outside the git tree: gold patch, test patch, harness
   eval script, log parser, and the digest-pinned image per instance. Pass
   `--registry "${DST}"` for a single-repository mirror, or pass
   `--image-ref '{registry}/sweb.eval.x86_64.{instance_id_1776}:latest'` against
   the Docker Hub remote repository:

   ```bash
   # From a single-repository mirror:
   /tmp/swe-venv/bin/python3 benchmarking/swebench/make-dataset.py \
     --registry "${DST}" \
     --revision <dataset commit> \
     --out /tmp/swebench-verified.jsonl

   # Or directly through the Docker Hub remote repository:
   /tmp/swe-venv/bin/python3 benchmarking/swebench/make-dataset.py \
     --registry us-central1-docker.pkg.dev/PROJECT/dockerhub-remote/swebench \
     --image-ref '{registry}/sweb.eval.x86_64.{instance_id_1776}:latest' \
     --revision <dataset commit> \
     --out /tmp/swebench-verified.jsonl
   ```

   `--image-ref` sets how an instance maps to an image reference (default
   `{registry}:sweb.eval.x86_64.{instance_id}`). The script reports and skips
   instances whose image `crane` cannot resolve. It loads the dataset at a
   resolved commit (`--revision`, else the current head) and writes that commit
   (`resolved_revision`, or the file's `sha256` for a local dataset) and the
   `swebench` version to `OUT.meta.json`.

4. Optional: plan where the images go. Placement is image-blind, so the
   first boot of an image on a node pays its pull and unpack wherever the
   scheduler puts it. A placement plan assigns each image to **domains**,
   WorkerPools (or sets of pools sharing a label) that a per-environment
   worker selector addresses, under each node's image-cache budget, so that
   every node of a domain holds every image assigned to it. Describe the
   domains once per cluster shape, then plan:

   ```bash
   cat > /tmp/domains.json <<'EOF'
   {"domains": [
     {"name": "d0", "selector": {"ate-env/domain": "d0"}, "nodes": 4, "node_cache_bytes": 400000000000, "node_slots": 16},
     {"name": "d1", "selector": {"ate-env/domain": "d1"}, "nodes": 4, "node_cache_bytes": 400000000000, "node_slots": 16}
   ]}
   EOF
   go run ./cmd/benchmarking/swebench-loader plan-placement \
     -dataset /tmp/swebench-verified.jsonl \
     -exclude-file benchmarking/swebench/offline-exclusions.txt \
     -domains /tmp/domains.json -replication 1 -out /tmp/image-placement.json
   ```

   `node_cache_bytes` is the unpacked bytes one node may hold for the plan:
   the image-cache volume times atelet's low watermark (`--image-cache-low-percent`),
   minus headroom; `node_slots`, optional, is how many environments one node
   runs at once, which caps the pre-flight's concurrency. The output of
   `ate-env placement-domains --json` is a domains file too (`ate-env
   placement-domains --template default-template --json > domains.json`; it
   needs the global viewer binding). `--template` drops the pools without a
   selector and the ones the template does not match. A pool that no
   selector addresses is skipped, and `plan-placement` prints it with the
   reason (`skipped pool <namespace>/<name>: ...`): its workers share no
   label, a label they share has an empty value, or none reports a node. A
   skipped pool gets no image; a file whose pools are all skipped is an
   error. A skipped pool is still checked: a negative count, a planner field
   (`assigned_bytes`, `image_count`), an empty label key or a worker count
   that overflows `node_slots` makes the reader refuse the file. One pool name in two namespaces still makes the reader refuse the
   file, so remove one of the two from it. Its `node_slots` is one worker's configured actor
   capacity: the reader turns it into a per-node value, `min(capacity, 3) x
   workers per node` (rounded down, at least one worker per node), until
   Substrate reports what a node sustains. `-node-cache-bytes 400GB` supplies
   the budget the output does not carry; without the flag those domains get
   no budget check, and a zero budget is refused. Sizes come from the
   manifest's `image_bytes_compressed`
   (make-dataset.py records it) times `-unpack-factor` (default 2.5); an image
   without a size gets `-default-image-size` and is marked `size_assumed`. The
   planner takes the largest image first and gives each image its
   `-replication` domains with the most free budget; with `-schedule`, an
   image whose demand (the most environments one step asks for) reaches
   `-hot-demand` gets `-hot-replication` domains instead. Images no domain
   can take are listed under `unplaced` and pay cold first boots. The plan is
   `image-placement.json` (`schema_version: v1`, `internal/benchmarking/schema`);
   the pre-flight fills the node caches from it and the run steers each
   create into its image's domain.

5. Optional: warm the nodes from the plan. The PreflightWarmer ensures each
   image's derived template, which starts its golden bake (5 at a time per
   ateapi replica, about two minutes each: scale `ate-api-server` for a
   large image set), then warms every domain in parallel: as soon as an
   image's golden has settled (ready; `none` when the base template disables
   goldens; failed or given up on, which still fills the layers by cold
   boots) it runs one `ate-env-api` `WarmImage` call for the image in the
   domain: the server creates probe environments with the domain's worker
   selector, round after round, until every node of the domain has run the
   image once, holding each probe until it runs (the layers and the golden
   snapshot are on its node by then), reading its node and deleting it. A
   domain warms `-image-concurrency` images at once (default 4; `node_slots`
   in `domains.json` caps it at `node_slots / -probes-per-node`), so the
   bakes and the node warming overlap and the slots stay busy; the probes
   of concurrent images share the nodes, so an image can need a second
   round. Across calls `ate-env-api` caps the probes in flight per node of
   one worker selector (`-warm-max-probes-per-node`, default 2, also the
   most it takes for `-probes-per-node`): calls beyond the cap wait for
   slots on the server, and the coverage's `waited_ms` sums the wait. A
   call the control plane cuts off is repeated with its own run id, for
   up to `-probe-timeout` and nine attempts, unless its stream showed
   every node already; a pair's coverage is the sum of its attempts. The
   server deletes the probes of a call that broke when it can, but it holds
   a call's probes in memory only: an `ate-env-api` that restarted under
   the call leaves them. The pass cannot tell, so it lists every probe a
   broken call streamed under `undeleted_probes`, and each log line about a
   cut-off call carries the call's `run_id` (its probes are
   `<run_id>-<round>-<slot>`; the ones not yet streamed are in no list).
   The pass's `-run-id` is at most 20 characters with the
   `-<domain>-<image>-<attempt>` indexes it appends per call (the plan
   indexes of the pairs the pass warms, in base 36):

   ```bash
   export SUBSTRATE_ENV_AUTH_TOKEN="$(kubectl -n ate-env get secret ate-env-api-token -o jsonpath='{.data.token}' | base64 -d)"
   go run ./cmd/benchmarking/swebench-loader preflight \
     -plan /tmp/image-placement.json -events-dir /tmp/runs/pf-$(date +%s) \
     -endpoint 127.0.0.1:7777 -probes-per-node 2 -max-rounds 3
   ```

   It writes `preflight-events.jsonl` (a `preflight_template` and a
   `preflight_golden` per image, a `preflight_probe` per probe with its
   node; next to the run's `events/` directory, not in it, so the run report
   does not count them: `-events-dir runs/x/events` writes to `runs/x`) and
   `preflight-coverage.json` (`schema_version: v1`):
   per domain the nodes seen and per image the nodes covered, probes, rounds
   and failures. It exits non-zero when a golden is not ready, a pair is
   short of its nodes or a probe could not be deleted, and says which; the
   probes left behind (a failed delete, or a call that broke before it
   reported its deletes) are listed per image under `undeleted_probes` and hold
   a node slot each until swept with `ate-env delete`. `-golden-timeout` (default 30 min)
   gives up on the goldens still baking once none became ready for that
   long. `-golden` says what the pass does about a golden that is not ready:
   `wait` (the default) probes the image after the wait whether or not its
   golden is ready, because a boot fills the layer caches too; `skip` does
   not wait and probes at once, and records the goldens still baking as
   `pending`, which makes the coverage incomplete: such a pass cannot be the
   Rule 1 evidence of a report on warm footing
   (`report -validate -footing warm -preflight-coverage` fails on it), so use
   `skip` only when each golden is ready before the pass or when the run needs
   no warm-footing report; `fail` waits and does not probe an image whose golden is not
   ready, and reports its pairs short with `golden not ready`. The probes are deleted: the planner keeps each node's assigned
   images under atelet's GC low watermark, so the layers stay until the run.

## Run

From a machine with a port-forward (fine for the smoke test and the step
loop):

```bash
kubectl -n ate-env port-forward svc/ate-env-api 7777:7777 &
export SUBSTRATE_ENV_AUTH_TOKEN="$(kubectl -n ate-env get secret ate-env-api-token -o jsonpath='{.data.token}' | base64 -d)"
go run ./cmd/benchmarking/swebench-loader run -dataset /tmp/swebench-verified.jsonl \
  -exclude-file benchmarking/swebench/offline-exclusions.txt \
  -images 5 -tasks-per-image 1 -width 5 -run-id smoke -out /tmp/smoke.jsonl
go run ./cmd/benchmarking/swebench-loader report /tmp/smoke.jsonl
```

`-dry-run` loads and renders everything and prints the plan without touching
the API. `-keep` leaves the fleet in place for inspection.

After a pre-flight (step 5 above), `-placement /tmp/image-placement.json` makes
every create carry the worker selector of one of its image's domains,
round-robin per image, so the environments land on the nodes that already
hold the image; images the plan left unplaced get no selector. It needs the
ate-env API backend. In eval mode `-domain-width N` next to `-placement` runs
N tasks at a time in each placement domain, a closed loop per domain, in place
of `-width` across the run: replay k of an image goes to the k-th of its
domains, and a domain that falls behind keeps only its own tasks, where with
`-width` it would collect the run's in-flight tasks. Use it to hold a set
number of environments running in every domain. The run then has up to N
times the number of domains in flight, and N more when the plan left some of
its images unplaced: 19 domains at `-domain-width 24` are 456. Set
`-connections` and the API server's database connection pool as for a
`-width` of that total; see the warning under the flag table in
[How many actors](#how-many-actors-and-on-which-clusters). The run event
records `domain_width` in place of `width`, so `report -arm` pools reps
whatever their `-width`. `-preflight` next
to `-placement` runs the pre-flight
(step 5) inside the same command before the run, with that step's defaults
unless `-preflight-probes-per-node`, `-preflight-max-rounds`,
`-preflight-probe-timeout`, `-preflight-golden-timeout` or
`-preflight-golden` say otherwise (they need `-preflight`; a value out of
range ends the command before it writes a file, the converted trace of
`-convert-trace` included), writing
`preflight-events.jsonl` and `preflight-coverage.json` next to the run's
events (`-events-dir`, or the directory of `-out`; the directory above when
that one is named `events`, so `events/*.jsonl` stays the run's shards); an
incomplete pre-flight is logged and the run goes on (as is a complete one that
left probes it could not delete: they hold slots until swept), a cancel during
it ends the command, as does a golden that is not ready with
`-preflight-golden fail` (after the coverage file is written), and `-out`
cannot be one of the two file names. With `-preflight-golden skip` the coverage
is incomplete while a golden bakes, and the report below then fails Rule 1. Give the report the pre-flight's result too:
`swebench-loader report run.jsonl -validate -footing warm -preflight-coverage preflight-coverage.json`
takes a complete pre-flight as Validity Rule 1's prewarm evidence and fails
on an incomplete one, naming the shortfall; the report's `validity.evidence`
says which Rule 1 evidence the verdict rests on (the coverage, the prewarm
events, the node cache delta).

When ate-env-api requires a bearer token (`-auth-token-file` on the API), give
the loader the same token: `-auth-token-file PATH`, or the
`SUBSTRATE_ENV_AUTH_TOKEN` environment variable that the `ate-env` CLI also
reads. The file wins when both are set. Without either, the loader sends no
token.

For the 500-wide shapes, invoke [`./run-in-cluster.sh`](./run-in-cluster.sh)
from an output directory outside the repository in a subshell so the copied
events file keeps the git tree clean, and run the loader next to `ate-env-api`
across its replicas (`-connections`, default 4):

```bash
export ENV_REPO="${PWD}"
(
  mkdir -p /tmp/swe-runs && cd /tmp/swe-runs
  AUTH_TOKEN_SECRET=ate-env-api-token \
  KO_DOCKER_REPO=us-central1-docker.pkg.dev/PROJECT/bench \
    "${ENV_REPO}/benchmarking/swebench/run-in-cluster.sh" swe-eval500x6 /tmp/swebench-verified.jsonl \
    -mode eval -images 500 -tasks-per-image 6 -width 500 -connections 8
)
```

The script bakes the dataset, the exclusion list, any `-placement` plan and
any `-trajectory` file you pass (with the files its `write` steps name, so a
grader script travels with its trajectory, and nothing else from its
directory) into the loader image, runs a Job next to ate-env-api, follows the full loader log (step failures
included), copies the events file back to the current directory, prints the
report and deletes the Job. `CONTEXT=<kubectl context>` selects the cluster;
the same command with a different context and run id is how two clusters are
compared. For an API with a token, set `AUTH_TOKEN_SECRET`
to a Secret in the namespace whose key `token` holds it; the Job passes it to
the loader as `SUBSTRATE_ENV_AUTH_TOKEN`.
`LOADER_NODEPOOL=<gke node pool>` pins the loader pod to a node pool and
`LOADER_TOLERATION_KEY=<taint key>` lets it tolerate that key's taint, any
effect (each may be set alone; both are checked before the build); without it several
shards share the few untainted nodes, and a drain of one of them ends every
shard on it, events included, because the Job does not retry.

Running several shards at once: each invocation bakes its dataset into the
same `kodata` directory and removes it after `ko build`, so two invocations
whose bake-and-build windows overlap take each other's files (the loser's
loader exits with `open dataset ... no such file`). The window is not a fixed
time: it covers `go run ... trajectory-assets` per trajectory, the build and
the push, minutes on a cold Go cache. Start the next invocation only after
the previous one has printed `loader image ...`.
In eval mode the per-loader `-delete-rate` (200/s by default) caps the shard's
task rate at that many tasks per second once starts are warm; raise it for a
throughput run. The events file lives in the pod until the copy at the end, so
a shard that loses its pod loses its events.

## Image-delivery workloads

Three built-in trajectories exercise image delivery rather than SWE-bench
grading, for comparing conventional pull, image streaming and other loaders:
`workload-file-read` (read a sample of files), `workload-compile`
(byte-compile every Python file, which reads each one and writes next to it)
and `workload-read-all` (read every file, the worst case for lazy loading).
Each starts with a `probe` step that runs `true`, so `ttfic` is the cold start
alone and the workload's own time is its own `step` row. They need no grading
material, so the manifest can be image-only:

```bash
benchmarking/swebench/make-dataset.py --images-file images.txt --out images.jsonl
swebench-loader run -dataset images.jsonl -trajectory workload-read-all -images 200 -width 200 -run-id pull-baseline
```

The workloads take their knobs from `run -set KEY=VALUE`, through the
trajectories' `var` function, and each file states its defaults, which fit a
SWE-bench image. A value lands in the command as one quoted shell word. Each
`-set` is recorded as a `set.KEY` label, which the reps of an arm must agree
on, and a key that no trajectory reads is an error:

| Workload | Knobs | Default | What it reads |
|---|---|---|---|
| `workload-file-read-one` | `READ_FILE` | `/testbed/.git/HEAD` (tiny; set a real file to measure a read) | one file |
| `workload-file-read` | `READ_ROOT`, `READ_FILES` | `/`, `200` | a sample of Python files |
| `workload-compile` | `PYTHON`, `COMPILE_DIR` | `python3`, `/testbed` | every Python file under the directory, compiled with the image's own Python |
| `workload-read-all` | `READ_ROOT` | `/` | every regular file, the worst case for lazy loading |

The reading steps print the bytes read, which the reward step's output tail
keeps in the events file, and fail when nothing was read or a read reported
an error, so a wrong path or a lazy-loading fault scores 0, not 1. `-trajectory workload-file-read-one,workload-compile,workload-read-all`
mixes them: each environment draws one, with `-seed` fixing the draw so every
rep gives the same environment the same workload; the task event's `name` is
the trajectory it replayed. In steps mode the first step's work runs after the
create and every later step's after a wake, which is the boot-then-resume
shape of the profiling scripts these workloads were ported from.

`images.txt` holds one reference per line, a tag or `repo@sha256:...`; tags
are resolved with `crane`. The instance id is the image name, the tag if one
was given, and the first 12 digits of the digest, so two pinned images of one
repository stay distinct. A line that resolves to a reference already
written is skipped. `--limit` applies; `--instances` does not.

Run the same command against each delivery mechanism and diff the reports;
the loader sees wall time per phase as a trainer would, and the split of a
cold start into pull, unpack and restore comes from Substrate's own metrics
over the run window, joined on the run id.

To know whether those first pulls were cold, sample the node image caches
before and after the run and hand both files to the report:

```bash
benchmarking/swebench/sample-node-cache.sh before.txt
benchmarking/swebench/run-in-cluster.sh pull-baseline images.jsonl -trajectory workload-read-all -images 200 -width 200
benchmarking/swebench/sample-node-cache.sh after.txt
swebench-loader report pull-baseline.jsonl -cache-before before.txt -cache-after after.txt
```

The sample reads each atelet's `ate_imagecache_requests_total` counters and
its process start time through the API server's pod proxy (atelet has no
shell), so it needs kubectl with RBAC `get` on `pods/proxy` in `ate-system`.
The report then adds a line with the run's misses, which are its cold pulls,
the nodes they landed on, hits, errors, and misses per first command: 1.0
means every environment pulled its image, 0 means every start was served
from a node's cache. The count is cluster-wide for the window, so it also
holds golden bakes and wakes that landed on a node without the image. A node
whose atelet restarted between the samples, and a node the autoscaler added
in between, count from zero; a node removed in between is left out; a node
present only in the after sample with an atelet older than the before sample
had a failed before scrape and is reported and left out. The report does not
label one environment cold or warm: the ate-env API reports the node an
environment landed on (`Environment.node`, which the pre-flight reads), but
the report has no per-node image-cache state to pair it with;
`preflight-coverage.json` (`report -preflight-coverage`) says which nodes ran
each image before the run.

To split the first-command window itself, harvest atelet's restore records
for the run and give them to the report:

```bash
benchmarking/swebench/harvest-restore-timings.sh pull-baseline restores.jsonl
swebench-loader report pull-baseline.jsonl -restore-log restores.jsonl
```

Kubelet rotates a busy atelet's log within the hour, so for a run longer
than a few minutes let the run script harvest as it goes: with
`RESTORE_LOG=restores.jsonl` it runs the harvest every ten minutes
(`HARVEST_INTERVAL_MIN`) and once more at the end, merges the parts with
duplicates dropped, and passes the file to the final report.

```bash
RESTORE_LOG=restores.jsonl benchmarking/swebench/run-in-cluster.sh pull-baseline images.jsonl ...
```

atelet logs one record per actor restore with the seconds each phase took:
manifest fetch, snapshot download, image unpack, the worker's restore, and
the total. The actor's name is the environment's name, so the report pairs
each record with the `ttfic` or `wake` event of that environment whose window
(the event's end minus its duration, up to its end) contains the record's
time: a record inside a `ttfic` window is a first start, one inside a `wake`
window is a wake. The table gives p50/p95/max per phase plus an `outside`
row, the event's span minus atelet's total, which is ateapi, the scheduler,
the router and the command itself. The report does not call a restore cold
or warm: atelet's `oci_unpack` phase times the bundle preparation, which runs
on every restore whether or not the node had the layers, so the record
carries no cache signal; the cache samples above give the run's misses until
atelet records the image cache outcome per restore. Failed restores are
counted and left out of the percentiles; restores whose event failed are
counted and left out of `outside`. Records before the run's first event or after its last (a rerun
with the same run id), for actors the run never created, or inside no window
(a restore after an eviction or a failed suspend) are counted, not paired.
The records come from the kubelet's container logs, which rotate, so harvest
right after the run; when atelet restarted, the previous container's log is
read too, one restart back.

## Network and the offline exclusion list

Actors have no network until an egress policy is created for them, which the
env SDK cannot do yet, and the SWE-bench harness assumes network (matplotlib's
eval scripts pip-install, requests and django have tests that open sockets).
`offline-exclusions.txt` lists the Verified instances that do not resolve
without egress, with the grader's verdict as the reason; pass it with
`-exclude-file` and every remaining instance should resolve. Regenerate it
from a full pass when the dataset, the images or the sandbox network change,
and review the diff: a new entry is either a regression or a new
network-dependent test, never silently accepted. Egress at scale is a
benchmark of its own.

## Two things the first runs showed

- Commands run as start, poll, read (the Go client's `ShellRequest.Poll`)
  rather than through the SDK's streaming shell: the router's route timeout
  (five minutes by default) cuts a single streamed request, and SWE-bench
  test runs regularly exceed it.
- On a large pool a new actor rarely lands on a node that already has its
  image, so time to first instruction is the cold pull plus the golden bake
  (about 20 s) for nearly every task. Report cold and warm passes separately.

## Record with every run

The bundle built with `bench measure -k8s` records the cluster, node pools,
Kubernetes and Substrate versions and the loader commit; pass the arm as a
`run -label` and record by hand what neither records: boot disk type and size,
CPU family, workers per node, bucket region. The loader records its own flags
itself: mode, width or domain width, images, trajectory, dataset, tasks per
image or rollouts, steps and park, connections, delete rate, base template, exclude
file, instances, and the create, step, wake-budget and wake-backoff timeouts.
The report alone is not reproducible without them.

## R2E-Gym and SWE-bench sweeps

Run mixed R2E-Gym and SWE-bench Verified sweeps across warm-pool strategies and
SDK/API client chains with [`./run-r2e-swebench.sh`](./run-r2e-swebench.sh) (see
[`./r2e-swebench/SKILL.md`](./r2e-swebench/SKILL.md) for dataset manifest
generation and [`./r2e-swebench/REPORT_TEMPLATE.md`](./r2e-swebench/REPORT_TEMPLATE.md)
for the report template):

```bash
export KO_DOCKER_REPO=us-central1-docker.pkg.dev/PROJECT/bench
export AUTH_TOKEN_SECRET=ate-env-api-token
export PYTHON_BIN=/tmp/swe-venv/bin/python3
export MANIFEST_DIR=/tmp/r2eswe-manifests
export ARTIFACT_DIR=/tmp/r2eswe-run

# 1. Validate manifests, trajectories, shard widths, and <63-char actor names offline:
benchmarking/swebench/run-r2e-swebench.sh dry-run \
  --r2e-dataset "${MANIFEST_DIR}/r2e.jsonl" \
  --swe-dataset "${MANIFEST_DIR}/swebench.jsonl" \
  --concurrency 200 \
  --images 20 \
  --rollouts-per-image 10 \
  --client all \
  --strategy all \
  --window-size 10 \
  --artifact-dir /tmp/r2eswe-dry

# 2. Bake derived ActorTemplate golden snapshots and warm node caches once before timing:
benchmarking/swebench/run-r2e-swebench.sh prewarm \
  --r2e-dataset "${MANIFEST_DIR}/r2e.jsonl" \
  --swe-dataset "${MANIFEST_DIR}/swebench.jsonl" \
  --concurrency 200 \
  --images 20 \
  --rollouts-per-image 10 \
  --artifact-dir "${ARTIFACT_DIR}"

# 3. Run the timed benchmark, collect 4-scope telemetry, validate, and optionally upload to GCS:
benchmarking/swebench/run-r2e-swebench.sh run \
  --r2e-dataset "${MANIFEST_DIR}/r2e.jsonl" \
  --swe-dataset "${MANIFEST_DIR}/swebench.jsonl" \
  --concurrency 200 \
  --images 20 \
  --rollouts-per-image 10 \
  --client all \
  --strategy all \
  --window-size 10 \
  --workload graded \
  --mode steps \
  --steps 1 \
  --api-service ate-env-api \
  --run-id r2eswe \
  --execution in-cluster \
  --collect-telemetry \
  --measure-interval 60 \
  ${GCS_BENCHMARK_BUCKET:+--gcs-bucket "${GCS_BENCHMARK_BUCKET}"} \
  --artifact-dir "${ARTIFACT_DIR}"
```

Size `--window-size` with two bounds when comparing `sliding` and `pipelined`:
keep `window_size < shard_images` (`10 < 20` above, creating `2` windows) so
both strategies advance across multiple windows (`window_size >= shard_images`
creates one window and matches `naive`), and keep
`window_size * rollouts_per_image >= shard_width` (`10 * 10 >= 100` when
`--concurrency 200` splits across two shards) so every worker slot stays busy
(`--window-size 0` clamps `DefaultWindowSize` to `8` images).
`LOADER_NODEPOOL` and `LOADER_TOLERATION_KEY` apply only to single-dataset
[`./run-in-cluster.sh`](./run-in-cluster.sh) `Job`s.

### `run-r2e-swebench.sh` subcommands and flags

CLI flags override their matching environment variables. Pass extra
`swebench-loader` flags after `--` (`-set KEY=VALUE` and `-label KEY=VALUE` also
forward to `--client python`).

| Flag / subcommand | Environment variable | Default | Description |
| :--- | :--- | :--- | :--- |
| `dry-run \| prewarm \| run` | - | `run` | Subcommand: validate plans offline (`dry-run`), bake goldens and warm caches (`prewarm`), or execute timed shards and reports (`run`). |
| `--r2e-dataset PATH` | `R2E_DATASET` | required | Digest-pinned R2E-Gym JSONL manifest path (`make-r2e-dataset.py`). |
| `--swe-dataset PATH` | `SWE_DATASET` | required | Digest-pinned SWE-bench Verified JSONL manifest path (`make-dataset.py`). |
| `-c, --concurrency N` | `MAX_CONCURRENCY` | `200` | Total active sandboxes across both `r2e` and `swe` shards (`>= 2`). |
| `--r2e-width N` | `R2E_WIDTH` | proportional split | Active sandbox cap for the R2E-Gym shard. |
| `--swe-width N` | `SWE_WIDTH` | proportional split | Active sandbox cap for the SWE-bench shard. |
| `-i, --images N` | `IMAGES` | `20` | Distinct images selected per dataset (`N` R2E-Gym + `N` SWE-bench = `2N` total). |
| `--r2e-images N` | `R2E_IMAGES` | `--images` (`20`) | Distinct images selected from `--r2e-dataset`. |
| `--swe-images N` | `SWE_IMAGES` | `--images` (`20`) | Distinct images selected from `--swe-dataset`. |
| `-r, --rollouts-per-image N` (`--rollouts N`) | `ROLLOUTS_PER_IMAGE` | `ceil(shard_width / shard_images)` | Replica rollouts (`steps` mode) or eval tasks (`eval` mode) per image. |
| `--r2e-rollouts N` | `R2E_FANOUT` | `--rollouts-per-image` | Replica rollouts per R2E-Gym image. |
| `--swe-rollouts N` | `SWE_FANOUT` | `--rollouts-per-image` | Replica rollouts per SWE-bench image. |
| `--strategy STRATEGY` | `STRATEGY` | unset | Warm-pool orchestration strategy: `none`, `naive`, `sliding`, `pipelined`, `all`, or comma-separated list (`CSV`). |
| `--window-size N` | `WINDOW_SIZE` | `0` (auto `1..8`) | Distinct-image window size for `sliding` and `pipelined` (`0` picks `clamp(ceil(images/2), 1, 8)`). |
| `--client, --chain CHAIN` | `CLIENT` | `go` | Client and API chain: `go` (Go SDK -> `ate-env-api`), `go-direct` (`DirectSubstrateBackend` -> `ateapi` + `atenet-router`), `python` (`ate_env_rl` `SandboxFleet` -> `ate-env-api`), `all`, or `CSV` (`all` includes `go-direct` when `--control-addr` is set). |
| `--workload graded\|io` | `WORKLOAD` | `graded` | Trajectory profile: `graded` (`r2e-gold.yaml` + `swebench-gold`) or `io` (`IO_TRAJECTORY`, default `workload-file-read`). |
| `--mode steps\|eval\|open-loop` | `MODE` | `steps` | Loader execution mode (`--strategy` and `--client python` support `steps` and `eval`). |
| `--steps N` | `STEPS` | `4` (`1` with `--strategy` or `python`) | Passes in `steps` mode (`--strategy` and `--client python` are single-pass and require `1`). |
| `--park POLICY` | `PARK` | `suspend` | Park policy in `steps` mode (`suspend`, `delete`, or `none`). |
| `--step-pause DUR` | `STEP_PAUSE` | `0s` | Pause before parking at step boundaries (for example `5s`). |
| `--execution in-cluster\|local` | `EXECUTION` | `in-cluster` if `KO_DOCKER_REPO` set, else `local` | Run loader shards as Kubernetes `Job`s in `NAMESPACE` or as local processes. |
| `--artifact-dir DIR` | `ARTIFACT_DIR` | `/tmp/<RUN_ID>` | Output directory outside the git checkout for `events/`, `arms/`, `report.{json,md}`, and `comparison.{txt,json,md}`. |
| `--run-id ID` | `RUN_ID` | `r2eswe` | DNS-label prefix (`<= 14` characters) for `Job` and actor names. |
| `--api-service NAME` | `API_SERVICE` | `ate-env-api` | In-cluster `ate-env-api` Kubernetes `Service` name (`<API_SERVICE>.<NAMESPACE>.svc:7777`). |
| `--auth-token-file PATH` | `AUTH_TOKEN_FILE` | unset | Bearer token file for `ate-env-api` in `--execution local` mode (in-cluster `Job`s use `AUTH_TOKEN_SECRET`). |
| `--python-bin PATH` | `PYTHON_BIN` | `python3` | Python interpreter for `dry-run` and `--execution local --client python` (`grpcio>=1.85.1`, `protobuf>=7.35`). |
| `--python-image IMAGE` | `PYTHON_LOADER_IMAGE` | built via `KO_DOCKER_REPO` | Prebuilt container image for in-cluster `--client python` `Job`s. |
| `--control-addr ADDR` | `CONTROL_ADDR` | unset | Substrate `ateapi` gRPC `host:port` (required for `--client go-direct`). |
| `--router-addr ADDR` | `ROUTER_ADDR` | unset | Substrate `atenet-router` `host:port` override for `--client go-direct`. |
| `--atenet-client-cert PATH` | `ATENET_CLIENT_CERT` | unset | Client mTLS PEM bundle (cert + key) for `atenet-router` in `--client go-direct`. |
| `--atenet-ca-file PATH` | `ATENET_CA_FILE` | unset | CA PEM bundle verifying `atenet-router` TLS in `--client go-direct`. |
| `--atenet-server-name NAME` | `ATENET_SERVER_NAME` | unset | TLS SNI server name override for `atenet-router` in `--client go-direct`. |
| `--ateapi-token-file PATH` | `ATEAPI_TOKEN_FILE` | unset | Bearer token file for `ateapi` in `--client go-direct`. |
| `--ateapi-ca-file PATH` | `ATEAPI_CA_FILE` | unset | CA PEM bundle verifying `ateapi` TLS in `--client go-direct`. |
| `--collect-telemetry` / `--no-collect-telemetry` | `COLLECT_TELEMETRY` | `1` if `--gcs-bucket` set, else `0` | Enable node-cache before/after scraping, `atelet` restore harvesting, periodic `bench measure -k8s` snapshots (`measurements_snapshots/`), `bench report -dest`, and bundle validation. |
| `--measure-interval SEC` | `MEASURE_INTERVAL_SEC` | `60` when `COLLECT_TELEMETRY=1`, else `0` | Periodic in-flight `bench measure -k8s` snapshot interval in seconds (`0` disables periodic snapshots). |
| `--ate-endpoint ADDR` | `ATE_ENDPOINT` | unset | Optional Substrate `ateapi` gRPC endpoint for `bench reap` (`events/reap.jsonl`) and `bench measure`. |
| `--prom-url URL` | `PROM_URL` | unset | Optional Prometheus HTTP base URL for `bench measure`. |
| `--preflight-coverage PATH` | `PREFLIGHT_COVERAGE` | unset | Optional `preflight-coverage.json` copied into each arm bundle for warm-footing validation (Validity Rule 1). |
| `--footing warm\|cold` | `FOOTING` | unset | Optional footing check passed to `bench validate` / `bench upload`. |
| `--trace, --enable-tracing` | `ENABLE_TRACING` | `0` | Enable OTel/SDK tracing (`-enable-tracing` on Go `swebench-loader`; `traces/{r2e,swe}.jsonl` `v1` `TraceEvent` capture on `--client python`). Only a `python` arm with a trace file gets `_traced` appended to its derived GCS folder; a Go arm exports OTLP spans and its folder stays untraced. |
| `--otlp-endpoint ADDR` | `OTEL_EXPORTER_OTLP_ENDPOINT` | unset | Optional OTLP gRPC collector `host:port` forwarded to traced loader processes / `Job`s. |
| `--validate-bundle` | `VALIDATE_BUNDLE` | `1` when `COLLECT_TELEMETRY=1` or `--gcs-bucket` set | Run `bench validate -dest` on each completed arm bundle. |
| `--gcs-bucket BUCKET` | `GCS_BENCHMARK_BUCKET` | unset (never hardcoded) | Validate and upload each arm bundle via `bench upload` to `gs://<BUCKET>/<folder>/`. |
| `--gcs-folder FOLDER` | `GCS_BENCHMARK_FOLDER` | `<YYYY-MM-DD>_simulated-w<C>_r2e<R>-swe<S>_<nick>[_traced]` | Explicit `<YYYY-MM-DD>_<model>_<dataset>_<nickname>[_traced]` folder override (multi-arm sweeps append `-<arm>` per arm). |
| `--gcs-nickname NAME` | `GCS_BENCHMARK_NICKNAME` | `<RUN_ID>-<arm>` | Nickname segment used when deriving the GCS folder name. |
| `--gcs-dry-run` | `GCS_DRY_RUN` | `0` | Validate bundle and target `gs://` URI without executing `gcloud storage cp`. |
| `-h, --help` | - | - | Print command usage and flags. |

Additional environment variables for cluster targeting, timeouts, and node
telemetry:

| Environment variable | Default | Description |
| :--- | :--- | :--- |
| `KO_DOCKER_REPO` | unset | OCI registry prefix for `ko` Go loader and Docker Python fleet images; selects `--execution in-cluster` by default. |
| `CONTEXT`, `NAMESPACE`, `ATESPACE` | current context, `ate-env`, `$NAMESPACE` | `kubectl` context, Kubernetes namespace for `Job`s and `API_SERVICE`, and Substrate atespace. |
| `BASE_TEMPLATE` | `default-template` | Base `ActorTemplate` from which per-image templates (`<BASE_TEMPLATE>-<12hex>`) derive. |
| `ENDPOINT`, `AUTH_TOKEN_SECRET` | `localhost:7777`, unset | Local `ate-env-api` address (`--execution local`), and Kubernetes `Secret` (`key: token`) mounted as `SUBSTRATE_ENV_AUTH_TOKEN` in-cluster. |
| `CONNECTIONS`, `WAKE_BUDGET`, `CREATE_TIMEOUT`, `STEP_TIMEOUT`, `START_TIMEOUT` | `8`, `5m`, `5m`, `15m`, `600` | Sharded gRPC channel count, per-operation timeouts, and in-cluster pod start timeout in seconds. |
| `SWE_EXCLUDE_FILE`, `R2E_EXCLUDE_FILE` | `offline-exclusions.txt`, unset | Per-dataset instance exclusion files (set `SWE_EXCLUDE_FILE=""` to disable exclusions). |
| `SAMPLE_CACHE`, `HARVEST_RESTORES`, `HARVEST_INTERVAL_MIN` | `1` when `COLLECT_TELEMETRY=1` (else `0`), `10` | Scrape `atelet` `ate_imagecache_requests_total` (`cache_before.prom`, `cache_after.prom`) and harvest `atelet` restore records (`atelet_restores.jsonl`, joined into both per-shard summaries and `report.json` via `bench report -dest`). `run-in-cluster.sh` honors the same `COLLECT_TELEMETRY`, `ARTIFACT_DIR`, `MEASURE_INTERVAL_SEC`, and `GCS_BENCHMARK_*` environment variables for single-dataset runs. |
