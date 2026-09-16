package scraper

import (
	"context"
	"time"

	"seo-monitor/internal/model"
)

// Chinaz weights run every day, independently of the Aizhan result. They share
// Chinaz's request limiter/circuit with the separate supplemental worker.
type ChinazWeights struct{ source *ChinazSupplement }

func (s *ChinazSupplement) Weights() *ChinazWeights          { return &ChinazWeights{source: s} }
func (c *ChinazWeights) WaitReady(ctx context.Context) error { return c.source.WaitReady(ctx) }
func (c *ChinazWeights) Fetch(ctx context.Context, domain string) (model.Metric, error) {
	if err := c.WaitReady(ctx); err != nil {
		return model.Metric{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	metric, err := c.source.c.fetchWeights(ctx, domain)
	if err = c.source.recordResult(err); err != nil {
		return model.Metric{}, err
	}
	metric.MarkWeights("chinaz")
	metric.CollectionRoute = "direct:chinaz"
	return metric, nil
}
