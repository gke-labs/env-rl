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

// Command ate-env-fleet-site serves the sandbox fleet site: one read-only
// website, on Cloud Run behind Identity-Aware Proxy, over every cluster's
// inventory snapshot and the actor events (docs/plans/sandbox-fleet-site).
// It reads a bucket and a BigQuery view with its own credentials and holds
// no cluster credential. See internal/fleetsite.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
	"github.com/agent-substrate/env/internal/fleetsite"
	"github.com/agent-substrate/env/internal/telemetry"
)

const shutdownTimeout = 10 * time.Second

func main() {
	listen := ":8080"
	if port := os.Getenv("PORT"); port != "" {
		listen = ":" + port
	}
	var cfg config
	flag.StringVar(&cfg.listen, "listen", listen, "address to serve on; defaults to :$PORT, as Cloud Run sets it, else :8080")
	flag.StringVar(&cfg.bucket, "bucket", os.Getenv("FLEET_BUCKET"), "Cloud Storage bucket the reporters write v1/inventory/ to (env FLEET_BUCKET); exactly one of -bucket and -dir")
	flag.StringVar(&cfg.dir, "dir", "", "read the bucket layout under this local directory, as ate-env-reporter -out-dir writes it, instead of a bucket")
	flag.StringVar(&cfg.eventsTable, "events-table", os.Getenv("FLEET_EVENTS_TABLE"), "BigQuery view of the actor events, <project>.<dataset>.actor_events_v (env FLEET_EVENTS_TABLE); required with IAP")
	flag.StringVar(&cfg.iapAudience, "iap-audience", os.Getenv("FLEET_IAP_AUDIENCE"), "audience of the IAP assertion, for Cloud Run /projects/<number>/locations/<region>/services/<service> (env FLEET_IAP_AUDIENCE)")
	flag.BoolVar(&cfg.insecureNoIAP, "insecure-no-iap", false, "serve without checking an IAP assertion, for a local run: -listen must then be a loopback address")
	flag.Parse()
	if flag.NArg() > 0 {
		log.Fatalf("unexpected argument %q", flag.Arg(0))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg); err != nil {
		log.Fatal(err)
	}
}

type config struct {
	listen, bucket, dir, eventsTable, iapAudience string
	insecureNoIAP                                 bool
}

func (c config) validate() error {
	switch {
	case (c.bucket == "") == (c.dir == ""):
		return errors.New("set one of -bucket and -dir")
	case c.insecureNoIAP && c.iapAudience != "":
		return errors.New("-insecure-no-iap and -iap-audience cannot be used together")
	case !c.insecureNoIAP && c.iapAudience == "":
		return errors.New("-iap-audience is required: the site serves only requests that IAP signed (-insecure-no-iap for a local run)")
	case !c.insecureNoIAP && c.eventsTable == "":
		return errors.New("-events-table is required with IAP")
	}
	if c.insecureNoIAP {
		host, _, err := net.SplitHostPort(c.listen)
		if err != nil {
			return fmt.Errorf("-listen %q: %w", c.listen, err)
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("-insecure-no-iap serves anyone who connects: -listen %q must be a loopback address such as 127.0.0.1:8080", c.listen)
		}
	}
	return nil
}

func run(ctx context.Context, cfg config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	var store fleetsite.Store = fleetsite.DirStore{Dir: cfg.dir}
	if cfg.bucket != "" {
		gcs, err := storage.NewClient(ctx)
		if err != nil {
			return fmt.Errorf("creating the Cloud Storage client: %w", err)
		}
		defer gcs.Close()
		store = fleetsite.BucketStore{Bucket: gcs.Bucket(cfg.bucket)}
	}

	var auth fleetsite.Authenticate
	if cfg.insecureNoIAP {
		log.Printf("WARNING: -insecure-no-iap: every request on %s is served as the local user", cfg.listen)
		auth = func(*http.Request) (string, error) { return "local (no IAP)", nil }
	} else {
		v, err := fleetsite.NewIAPVerifier(ctx, cfg.iapAudience)
		if err != nil {
			return err
		}
		auth = fleetsite.IAPAuth(v)
	}

	var events fleetsite.EventSource
	if cfg.eventsTable != "" {
		bq, err := fleetsite.NewBigQueryEvents(ctx, cfg.eventsTable)
		if err != nil {
			return err
		}
		events = bq
	}

	version, _ := telemetry.BuildVersion()
	site, err := fleetsite.New(fleetsite.Config{
		Snapshots: fleetsite.NewSnapshots(store, nil),
		Events:    events,
		Auth:      auth,
		Version:   version,
	})
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           site,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	source := "gs://" + cfg.bucket
	if cfg.bucket == "" {
		source = cfg.dir
	}
	log.Printf("ate-env-fleet-site %s: serving %s on %s (events: %q)", version, source, ln.Addr(), cfg.eventsTable)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
