package internalcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

func latencyCount(t *testing.T, vec *prometheus.HistogramVec, value string) uint64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := vec.WithLabelValues(value).(prometheus.Metric).Write(metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetHistogram().GetSampleCount()
}

func TestLatencyMetricsCountAPICallsNotCacheHitsAndRecordReadFailures(t *testing.T) {
	ctx := context.Background()
	cloud, _ := cloudFixture(t, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "private-resource-id"})
	})
	before := latencyCount(t, ociRequestDuration, "GetNetworkLoadBalancer")
	cloud.ResetSnapshot()
	for i := 0; i < 2; i++ {
		if _, err := cloud.get(ctx, "private-resource-id"); err != nil {
			t.Fatal(err)
		}
	}
	if got := latencyCount(t, ociRequestDuration, "GetNetworkLoadBalancer"); got != before+1 {
		t.Fatalf("cache hit counted as SDK request: %d -> %d", before, got)
	}
	cloud.ResetSnapshot()
	if _, err := cloud.get(ctx, "private-resource-id"); err != nil {
		t.Fatal(err)
	}
	if got := latencyCount(t, ociRequestDuration, "GetNetworkLoadBalancer"); got != before+2 {
		t.Fatal("fresh request timing missing")
	}
	scheme := runtime.NewScheme()
	_ = core.AddToScheme(scheme)
	_ = discovery.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	resources := []string{"services", "endpointslices", "pods", "nodes"}
	counts := map[string]uint64{}
	for _, resource := range resources {
		counts[resource] = latencyCount(t, snapshotReadDuration, resource)
	}
	if _, err := newEndpointSnapshot(ctx, c, "private-namespace"); err != nil {
		t.Fatal(err)
	}
	for _, resource := range resources {
		if latencyCount(t, snapshotReadDuration, resource) != counts[resource]+1 {
			t.Fatalf("missing successful %s read timing", resource)
		}
	}
	want := errors.New("injected API timeout")
	if _, err := newEndpointSnapshot(ctx, endpointErrorReader{Reader: c, getKind: "Service", err: want}, "private-namespace"); !errors.Is(err, want) {
		t.Fatal(err)
	}
	if latencyCount(t, snapshotReadDuration, "services") != counts["services"]+2 {
		t.Fatal("failed snapshot read timing missing")
	}
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		label := ""
		switch family.GetName() {
		case "shared_nlb_oci_request_duration_seconds":
			label = "operation"
		case "shared_nlb_snapshot_read_duration_seconds":
			label = "resource"
		default:
			continue
		}
		for _, metric := range family.Metric {
			if len(metric.Label) != 1 || metric.Label[0].GetName() != label || metric.Label[0].GetValue() == "private-resource-id" || metric.Label[0].GetValue() == "private-namespace" {
				t.Fatalf("unexpected or private latency metric labels: %v", metric.Label)
			}
		}
	}
}
