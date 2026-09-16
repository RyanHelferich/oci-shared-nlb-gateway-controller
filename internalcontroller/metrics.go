package internalcontroller

import (
	"context"
	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	"fmt"
	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/prometheus/client_golang/prometheus"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"reflect"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"time"
)

var poolSlots = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "shared_nlb_pool_slots", Help: "Slots by state; retired slots cannot be reused."}, []string{"namespace", "pool", "state"})
var poolShards = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "shared_nlb_pool_shards", Help: "Persisted native NLB shards."}, []string{"namespace", "pool"})
var poolPending = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "shared_nlb_work_pending", Help: "One when a pool is waiting for an OCI work request."}, []string{"namespace", "pool"})
var latencyBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 40, 80}
var ociRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "shared_nlb_oci_request_duration_seconds", Help: "OCI SDK call duration including SDK retries; static operation labels only.", Buckets: latencyBuckets}, []string{"operation"})
var snapshotReadDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "shared_nlb_snapshot_read_duration_seconds", Help: "Fresh Kubernetes endpoint snapshot list duration; static resource-kind labels only.", Buckets: latencyBuckets}, []string{"resource"})

func init() {
	metrics.Registry.MustRegister(poolSlots, poolShards, poolPending, ociRequestDuration, snapshotReadDuration)
}

func timedOCI[T any](operation string, call func() (T, error)) (T, error) {
	started := time.Now()
	defer func() { ociRequestDuration.WithLabelValues(operation).Observe(time.Since(started).Seconds()) }()
	return call()
}

func (r *Reconciler) report(ctx context.Context, req ctrl.Request, result ctrl.Result, problem error) {
	if req.Namespace != r.Namespace {
		return
	}
	p := &api.NLBPool{}
	if err := r.Reader.Get(ctx, req.NamespacedName, p); err != nil {
		return
	}
	active, retired := 0, 0
	for _, a := range p.Status.Allocations {
		if a.Tombstone {
			retired++
		} else {
			active++
		}
	}
	for state, n := range map[string]int{"allocated": active, "retired": retired, "available": p.Spec.Occupancy*p.Spec.MaxShards - active - retired} {
		poolSlots.WithLabelValues(p.Namespace, p.Name, state).Set(float64(n))
	}
	poolShards.WithLabelValues(p.Namespace, p.Name).Set(float64(len(p.Status.Shards)))
	pending := 0.
	if p.Status.PendingWork != "" {
		pending = 1
	}
	poolPending.WithLabelValues(p.Namespace, p.Name).Set(pending)
	old := p.DeepCopyObject().(*api.NLBPool)
	condition := metav1.Condition{Type: "Reconciled", Status: metav1.ConditionFalse, Reason: "Progressing", Message: "Reconciliation in progress", ObservedGeneration: p.Generation}
	if problem != nil {
		condition.Reason = "ReconcileError"
		condition.Message = fmt.Sprint(problem)
		if e, ok := common.IsServiceError(problem); ok {
			condition.Message = fmt.Sprintf("OCI HTTP %d %s: %s", e.GetHTTPStatusCode(), e.GetCode(), e.GetMessage())
		}
		if len(condition.Message) > 1024 {
			condition.Message = condition.Message[:1024]
		}
	}
	if problem == nil && result.RequeueAfter >= 15*time.Second {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "Reconciled"
		condition.Message = "Pool reconciliation completed; inspect individual binding conditions and application probes"
	}
	meta.SetStatusCondition(&p.Status.Conditions, condition)
	if !reflect.DeepEqual(old.Status, p.Status) {
		if err := r.Status().Patch(ctx, p, client.MergeFrom(old)); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "updating pool condition")
		}
	}
}
