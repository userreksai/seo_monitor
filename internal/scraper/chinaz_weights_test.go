package scraper

import (
	"context"
	"net/http"
	"testing"
)

func TestDailyChinazWeightsDoNotDependOnAizhanFailure(t *testing.T) {
	a := agentForRecheck(t, "ok")
	c := fallbackChinaz(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(chinazZero)) })
	aizhan, err := a.Fetch(context.Background(), "www.baidu.com")
	if err != nil {
		t.Fatal(err)
	}
	chinaz, err := c.Weights().Fetch(context.Background(), "www.baidu.com")
	if err != nil {
		t.Fatal(err)
	}
	if aizhan.WeightSource != "aizhan" || chinaz.WeightSource != "chinaz" || chinaz.WeightValid == nil || !*chinaz.WeightValid || chinaz.CollectionRoute != "direct:chinaz" {
		t.Fatal("both daily results must be returned independently")
	}
}
