// aizhan-probe fetches one public SEO result without opening or writing MongoDB.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"seo-monitor/internal/model"
	"seo-monitor/internal/scraper"
)

func main() {
	domain := flag.String("domain", "www.baidu.com", "one hostname to query (one request, no database writes)")
	withChinaz := flag.Bool("with-chinaz", false, "also fetch the five supplemental Chinaz fields (up to three extra requests)")
	flag.Parse()
	cfg := scraper.Config{
		BaseURL: "https://www.aizhan.com", UserAgent: "seo-monitor/1.0 (daily metrics collector; contact your administrator)",
		Timeout: 25 * time.Second, MinDelay: 10 * time.Second, MaxDelay: 20 * time.Second, Retries: 1,
	}
	var source interface {
		Fetch(context.Context, string) (model.Metric, error)
	}
	var err error
	if *withChinaz {
		extra := cfg
		extra.BaseURL, extra.DataBaseURL = "https://seo.chinaz.com", "https://othertool.chinaz.com"
		extra.MinDelay, extra.MaxDelay = 3*time.Second, 8*time.Second
		source, err = scraper.NewHybrid(cfg, 15*time.Minute, extra, 15*time.Minute)
	} else {
		source, err = scraper.NewAizhan(cfg, 15*time.Minute)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	metric, err := source.Fetch(ctx, *domain)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(metric); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
