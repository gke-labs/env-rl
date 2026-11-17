// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// collectorName names the fleet telemetry collector's ServiceAccount,
// ConfigMap, Deployment and Service, and its default namespace. It differs
// from the acceptance fixture's (ate-env-telemetry/otel-collector), so an
// apply never merges into that fixture.
const collectorName = "ate-env-collector"

// defaultCollectorImage is the OpenTelemetry Collector contrib distribution
// the configuration below is written for: 0.165.0 names the components
// google_client_auth, otlp_http and google_cloud_storage, and its
// google_cloud_storage exporter takes traces and a partition.
const defaultCollectorImage = "otel/opentelemetry-collector-contrib:0.165.0@sha256:e40d61f10c9a617cca1ffd1be4d7e2b3a4b0f0001798ff07e58cabe82e618cea"

// Collector ports: OTLP over gRPC and HTTP, and the health_check extension.
const (
	collectorGRPCPort   = 4317
	collectorHTTPPort   = 4318
	collectorHealthPort = 13133
)

// collectorTracesPrefix and collectorTracesPartition place the raw span
// files in the fleet bucket (docs/plans/sandbox-fleet-site/interfaces.md,
// "Bucket layout").
const (
	collectorTracesPrefix    = "v1/traces"
	collectorTracesPartition = "year=%Y/month=%m/day=%d"
	// collectorFileBatchTimeout is the longest a span waits for its file.
	collectorFileBatchTimeout = "30s"
)

// gcpProjectID is a Google Cloud project id, optionally domain-scoped
// (example.com:project). It rules out "$", so no value reaches the
// collector's ${env:...} expansion.
var gcpProjectID = regexp.MustCompile(`^([a-z0-9][a-z0-9.-]*[a-z0-9]:)?[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

// bucketComponent is one dot-separated part of a Cloud Storage bucket name.
var bucketComponent = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]{0,61}[a-z0-9])?$`)

// checkBucketName applies the Cloud Storage naming rules: 3 to 63
// characters, or up to 222 with dots and 63 per dot-separated part; lower
// case letters, digits, "-", "_" and "."; a letter or digit at each end; not
// an IP address; no "goog" prefix and no "google" or "g00gle". The rules rule out "$" as
// well.
func checkBucketName(name string) error {
	bad := func(why string) error {
		return fmt.Errorf("--bucket %q is not a Cloud Storage bucket name: %s", name, why)
	}
	switch {
	case len(name) < 3:
		return bad("it is shorter than 3 characters")
	case !strings.Contains(name, ".") && len(name) > 63:
		return bad("it is longer than 63 characters")
	case len(name) > 222:
		return bad("it is longer than 222 characters")
	case net.ParseIP(name) != nil:
		return bad("it is an IP address")
	case strings.HasPrefix(name, "goog") || strings.Contains(name, "google") || strings.Contains(name, "g00gle"):
		return bad(`it starts with "goog" or contains "google" or a misspelling of it`)
	}
	for _, part := range strings.Split(name, ".") {
		if !bucketComponent.MatchString(part) {
			return bad(`use lower case letters, digits, "-" and "_", with a letter or digit at each end of each dot-separated part`)
		}
	}
	return nil
}

// gcpServiceAccountEmail is a Google service account's email: a
// user-managed account, <name>@<project>.iam.gserviceaccount.com, where a
// domain-scoped project (example.com:my-project) reads
// <name>@my-project.example.com.iam.gserviceaccount.com, or a project's
// default Compute Engine account, <number>-compute@developer.gserviceaccount.com.
var gcpServiceAccountEmail = regexp.MustCompile(`^(?:[a-z][a-z0-9-]{4,28}[a-z0-9]@[a-z][a-z0-9-]{4,28}[a-z0-9](?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*\.iam\.gserviceaccount\.com|[0-9]+-compute@developer\.gserviceaccount\.com)$`)

// configHashAnnotation on the pod template carries the SHA-256 of the
// collector's configuration, so a new configuration rolls the Deployment:
// the collector reads its file at start only.
const configHashAnnotation = "ate-env.dev/config-sha256"

type collectorConfig struct {
	namespace         string
	image             string
	bucket            string
	project           string
	gcpServiceAccount string
}

func (c *collectorConfig) validate() error {
	for _, v := range []*string{&c.namespace, &c.image, &c.bucket, &c.project, &c.gcpServiceAccount} {
		*v = strings.TrimSpace(*v)
	}
	switch {
	case c.namespace == "":
		return errors.New("--namespace is empty")
	case len(validation.IsDNS1123Label(c.namespace)) > 0:
		return fmt.Errorf("--namespace %q is not a Kubernetes namespace name: %s", c.namespace, strings.Join(validation.IsDNS1123Label(c.namespace), "; "))
	case c.image == "":
		return errors.New("--image is empty")
	case c.bucket == "":
		return errors.New("--bucket is required: the bucket the span files go to")
	case strings.HasPrefix(c.bucket, "gs://"):
		return fmt.Errorf("--bucket %q: give the bucket's name, not a gs:// URL", c.bucket)
	case checkBucketName(c.bucket) != nil:
		return checkBucketName(c.bucket)
	case c.gcpServiceAccount != "" && !gcpServiceAccountEmail.MatchString(c.gcpServiceAccount):
		return fmt.Errorf("--gcp-service-account %q is not <name>@<project>.iam.gserviceaccount.com or <number>-compute@developer.gserviceaccount.com", c.gcpServiceAccount)
	case c.project == "":
		return errors.New("--project is required: the project whose Cloud Trace receives the spans")
	case !gcpProjectID.MatchString(c.project):
		return fmt.Errorf("--project %q is not a Google Cloud project id", c.project)
	}
	return nil
}

func newManifestCollectorCommand() *cobra.Command {
	cfg := collectorConfig{}
	cmd := &cobra.Command{
		Use:   "collector",
		Short: "Generate the manifests of the fleet telemetry collector",
		Long: `Collector prints the Kubernetes manifests of one OpenTelemetry collector per
cluster: its namespace, ServiceAccount, configuration, Deployment and Service.
It receives OTLP traces on 4317 (gRPC) and 4318 (HTTP) and exports each batch
twice: to Cloud Trace in --project through telemetry.googleapis.com, and as an
OTLP/JSON file, gzip, under v1/traces/year=YYYY/month=MM/day=DD/ in --bucket.

  ate-env manifest collector --bucket <bucket> --project <project> \
    --gcp-service-account ate-env-collector@<central-project>.iam.gserviceaccount.com \
    | kubectl apply -f -

Point ate-env-api and the trainers at it:

  ate-env manifest --enable-tracing --otlp-insecure \
    --otlp-endpoint http://ate-env-collector.ate-env-collector:4317 ...

The collector runs on GKE: its bucket exporter reads the zone and the project
from the metadata server at start, and it authenticates as the Google service
account that --gcp-service-account binds through Workload Identity. That
account needs roles/cloudtrace.agent in --project, and on the bucket
roles/storage.objectCreator and storage.buckets.get (for example
roles/storage.legacyBucketReader), because the exporter checks that the
bucket exists before it starts. A new configuration changes the pod
template's ` + configHashAnnotation + ` annotation, so kubectl apply restarts
the collector. Enable telemetry.googleapis.com in --project
and in the project of the Google service account; otherwise each Cloud Trace
export fails with SERVICE_DISABLED and only the bucket gets the spans.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cfg.validate(); err != nil {
				return err
			}
			if !strings.Contains(cfg.image, "@sha256:") {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: --image %s is not pinned by digest; the configuration is written for %s\n", cfg.image, defaultCollectorImage)
			}
			if cfg.gcpServiceAccount == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "note: no --gcp-service-account: the collector runs as the node's service account, which needs the bucket and Cloud Trace roles itself")
			}
			objs, err := buildCollectorManifests(cfg)
			if err != nil {
				return err
			}
			return writeManifests(cmd.OutOrStdout(), objs)
		},
	}
	cmd.Flags().StringVar(&cfg.namespace, "namespace", collectorName, "Kubernetes namespace the collector runs in")
	cmd.Flags().StringVar(&cfg.image, "image", defaultCollectorImage, "OpenTelemetry Collector contrib image, 0.165.0 or a later one that keeps the component names")
	cmd.Flags().StringVar(&cfg.bucket, "bucket", "", "name of the Cloud Storage bucket the span files go to (no gs:// prefix, no path; required)")
	cmd.Flags().StringVar(&cfg.project, "project", "", "Google Cloud project whose Cloud Trace receives the spans, set as gcp.project_id (required)")
	cmd.Flags().StringVar(&cfg.gcpServiceAccount, "gcp-service-account", "", "Google service account the ServiceAccount acts as through Workload Identity, set as its "+workloadIdentityAnnotation+" annotation: <name>@<project>.iam.gserviceaccount.com (with a domain-scoped project, <name>@<project>.<domain>.iam.gserviceaccount.com) or <number>-compute@developer.gserviceaccount.com")
	return cmd
}

// collectorPipeline returns the collector's configuration: two traces
// pipelines from the OTLP receiver, one to Cloud Trace and one to the
// bucket, which batches longer.
func collectorPipeline(cfg collectorConfig) map[string]any {
	return map[string]any{
		"extensions": map[string]any{
			"health_check":       map[string]any{"endpoint": fmt.Sprintf("0.0.0.0:%d", collectorHealthPort)},
			"google_client_auth": map[string]any{"project": cfg.project},
		},
		"receivers": map[string]any{
			"otlp": map[string]any{"protocols": map[string]any{
				"grpc": map[string]any{"endpoint": fmt.Sprintf("0.0.0.0:%d", collectorGRPCPort)},
				"http": map[string]any{"endpoint": fmt.Sprintf("0.0.0.0:%d", collectorHTTPPort)},
			}},
		},
		"processors": map[string]any{
			// Refuse data before the container's memory limit kills it.
			"memory_limiter": map[string]any{
				"check_interval":         "1s",
				"limit_percentage":       80,
				"spike_limit_percentage": 20,
			},
			// The Telemetry API writes a span to the project this
			// attribute names. upsert replaces a value the sender set,
			// so a pod cannot write into another cluster's project with
			// this collector's identity.
			"resource/gcp_project_id": map[string]any{"attributes": []any{
				map[string]any{"key": "gcp.project_id", "value": cfg.project, "action": "upsert"},
			}},
			"batch": map[string]any{},
			// One object per export: a longer wait makes fewer, larger
			// span files.
			"batch/files": map[string]any{"timeout": collectorFileBatchTimeout, "send_batch_size": 8192},
		},
		"exporters": map[string]any{
			"otlp_http/cloudtrace": map[string]any{
				"endpoint": "https://telemetry.googleapis.com",
				"encoding": "proto",
				"auth":     map[string]any{"authenticator": "google_client_auth"},
			},
			"google_cloud_storage": map[string]any{"bucket": map[string]any{
				"name": cfg.bucket,
				// The bucket exists: never try to create it.
				"reuse_if_exists": true,
				"file_prefix":     "spans",
				"compression":     "gzip",
				"partition": map[string]any{
					"prefix": collectorTracesPrefix,
					"format": collectorTracesPartition,
				},
			}},
		},
		"service": map[string]any{
			"extensions": []any{"health_check", "google_client_auth"},
			"pipelines": map[string]any{
				"traces/cloudtrace": map[string]any{
					"receivers":  []any{"otlp"},
					"processors": []any{"memory_limiter", "resource/gcp_project_id", "batch"},
					"exporters":  []any{"otlp_http/cloudtrace"},
				},
				"traces/files": map[string]any{
					"receivers":  []any{"otlp"},
					"processors": []any{"memory_limiter", "resource/gcp_project_id", "batch/files"},
					"exporters":  []any{"google_cloud_storage"},
				},
			},
		},
	}
}

func buildCollectorManifests(cfg collectorConfig) ([]any, error) {
	labels := map[string]string{"app": collectorName}

	pipeline, err := yaml.Marshal(collectorPipeline(cfg))
	if err != nil {
		return nil, fmt.Errorf("encoding the collector configuration: %w", err)
	}
	configMap := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: collectorName, Namespace: cfg.namespace, Labels: labels},
		Data:       map[string]string{"config.yaml": string(pipeline)},
	}

	// The collector never calls the Kubernetes API; on GKE, Workload
	// Identity serves its Google credentials from the metadata server.
	automount := false
	sa := &corev1.ServiceAccount{
		TypeMeta:                     metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta:                   metav1.ObjectMeta{Name: collectorName, Namespace: cfg.namespace, Labels: labels},
		AutomountServiceAccountToken: &automount,
	}
	if cfg.gcpServiceAccount != "" {
		sa.Annotations = map[string]string{workloadIdentityAnnotation: cfg.gcpServiceAccount}
	}

	replicas := int32(1)
	nonRoot, readOnly, escalation := true, true, false
	probe := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(collectorHealthPort)},
	}}
	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: collectorName, Namespace: cfg.namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{configHashAnnotation: fmt.Sprintf("%x", sha256.Sum256(pipeline))},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:           collectorName,
					AutomountServiceAccountToken: &automount,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   &nonRoot,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:  "collector",
						Image: cfg.image,
						Args:  []string{"--config=/conf/config.yaml"},
						Ports: []corev1.ContainerPort{
							{Name: "otlp-grpc", ContainerPort: collectorGRPCPort},
							{Name: "otlp-http", ContainerPort: collectorHTTPPort},
							{Name: "health", ContainerPort: collectorHealthPort},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
							Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
						},
						SecurityContext: &corev1.SecurityContext{
							ReadOnlyRootFilesystem:   &readOnly,
							AllowPrivilegeEscalation: &escalation,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts:   []corev1.VolumeMount{{Name: "conf", MountPath: "/conf", ReadOnly: true}},
						LivenessProbe:  probe,
						ReadinessProbe: probe,
					}},
					Volumes: []corev1.Volume{{
						Name: "conf",
						VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: collectorName},
						}},
					}},
				},
			},
		},
	}

	service := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: collectorName, Namespace: cfg.namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{Name: "otlp-grpc", Port: collectorGRPCPort, TargetPort: intstr.FromString("otlp-grpc")},
				{Name: "otlp-http", Port: collectorHTTPPort, TargetPort: intstr.FromString("otlp-http")},
			},
		},
	}

	return []any{
		&corev1.Namespace{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
			ObjectMeta: metav1.ObjectMeta{Name: cfg.namespace},
		},
		sa, configMap, deployment, service,
	}, nil
}
