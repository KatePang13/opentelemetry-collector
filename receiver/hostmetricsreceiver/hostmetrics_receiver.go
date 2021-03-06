// Copyright 2020, OpenTelemetry Authors
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

package hostmetricsreceiver

import (
	"context"
	"fmt"
	"time"

	"go.opencensus.io/trace"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector/component"
	"github.com/open-telemetry/opentelemetry-collector/component/componenterror"
	"github.com/open-telemetry/opentelemetry-collector/consumer"
	"github.com/open-telemetry/opentelemetry-collector/consumer/pdatautil"
	"github.com/open-telemetry/opentelemetry-collector/internal/data"
	"github.com/open-telemetry/opentelemetry-collector/receiver/hostmetricsreceiver/internal"
)

// receiver is the type that scrapes various host metrics.
type receiver struct {
	config   *Config
	scrapers []internal.Scraper
	consumer consumer.MetricsConsumer
	cancel   context.CancelFunc
}

// newHostMetricsReceiver creates a host metrics scraper.
func newHostMetricsReceiver(
	ctx context.Context,
	logger *zap.Logger,
	config *Config,
	factories map[string]internal.Factory,
	consumer consumer.MetricsConsumer,
) (*receiver, error) {

	scrapers := make([]internal.Scraper, 0)
	for key, cfg := range config.Scrapers {
		factory := factories[key]
		if factory == nil {
			return nil, fmt.Errorf("host metrics scraper factory not found for key: %s", key)
		}

		scraper, err := factory.CreateMetricsScraper(ctx, logger, cfg)
		if err != nil {
			return nil, fmt.Errorf("cannot create scraper: %s", err.Error())
		}
		scrapers = append(scrapers, scraper)
	}

	hmr := &receiver{
		config:   config,
		scrapers: scrapers,
		consumer: consumer,
	}

	return hmr, nil
}

// Start initializes the underlying scrapers and begins scraping
// host metrics based on the OS platform.
func (hmr *receiver) Start(ctx context.Context, host component.Host) error {
	ctx, hmr.cancel = context.WithCancel(ctx)

	go func() {
		hmr.initializeScrapers(ctx, host)
		hmr.startScrapers(ctx)
	}()

	return nil
}

// Shutdown terminates all tickers and stops the underlying scrapers.
func (hmr *receiver) Shutdown(ctx context.Context) error {
	hmr.cancel()
	return hmr.closeScrapers(ctx)
}

func (hmr *receiver) initializeScrapers(ctx context.Context, host component.Host) {
	for _, scraper := range hmr.scrapers {
		err := scraper.Initialize(ctx)
		if err != nil {
			host.ReportFatalError(err)
			return
		}
	}
}

func (hmr *receiver) startScrapers(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(hmr.config.CollectionInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				hmr.ScrapeMetrics(context.Background(), hmr.scrapers)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (hmr *receiver) ScrapeMetrics(ctx context.Context, scrapers []internal.Scraper) {
	ctx, span := trace.StartSpan(ctx, "hostmetricsreceiver.ScrapeMetrics")
	defer span.End()

	metricData := data.NewMetricData()
	metrics := internal.InitializeMetricSlice(metricData)

	var errors []error
	for _, scraper := range scrapers {
		scraperMetrics, err := scraper.ScrapeMetrics(ctx)
		if err != nil {
			errors = append(errors, err)
		}

		scraperMetrics.MoveAndAppendTo(metrics)
	}

	if len(errors) > 0 {
		span.SetStatus(trace.Status{Code: trace.StatusCodeDataLoss, Message: fmt.Sprintf("Error(s) when scraping metrics: %v", componenterror.CombineErrors(errors))})
	}

	if metrics.Len() > 0 {
		err := hmr.consumer.ConsumeMetrics(ctx, pdatautil.MetricsFromInternalMetrics(metricData))
		if err != nil {
			span.SetStatus(trace.Status{Code: trace.StatusCodeDataLoss, Message: fmt.Sprintf("Unable to process metrics: %v", err)})
			return
		}
	}
}

func (hmr *receiver) closeScrapers(ctx context.Context) error {
	var errs []error
	for _, scraper := range hmr.scrapers {
		err := scraper.Close(ctx)
		if err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return componenterror.CombineErrors(errs)
	}

	return nil
}
