package internalcontroller

import (
	"context"
	"errors"
	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	"fmt"
	n "github.com/oracle/oci-go-sdk/v65/networkloadbalancer"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"reflect"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sort"
	"strings"
	"time"
)

type Reconciler struct {
	client.Client
	Reader      client.Reader
	Cloud       *Cloud
	Recorder    record.EventRecorder
	Namespace   string
	Compartment string
	Subnet      string
}

func (r *Reconciler) Setup(m ctrl.Manager) error {
	enqueue := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		var pools api.NLBPoolList
		if e := r.List(ctx, &pools, client.InNamespace(r.Namespace)); e != nil {
			return nil
		}
		out := []reconcile.Request{}
		for _, p := range pools.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Name}})
		}
		return out
	})
	return ctrl.NewControllerManagedBy(m).For(&api.NLBPool{}).Watches(&api.TunnelBinding{}, enqueue).Watches(&core.Service{}, enqueue).Watches(&discovery.EndpointSlice{}, enqueue).Watches(&core.Pod{}, enqueue).Watches(&core.Node{}, enqueue).WithOptions(controller.Options{MaxConcurrentReconciles: 1}).Complete(r)
}
func (r *Reconciler) bindingStatus(ctx context.Context, b *api.TunnelBinding, a *api.Allocation, p *api.NLBPool, problem error) error {
	old := b.DeepCopyObject().(*api.TunnelBinding)
	status := metav1.ConditionTrue
	reason := "Configured"
	message := "Native NLB configuration converged; application health must be probed independently"
	if problem != nil {
		status = metav1.ConditionFalse
		reason = "NotConfigured"
		message = problem.Error()
	}
	meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{Type: "Configured", Status: status, Reason: reason, Message: message, ObservedGeneration: b.Generation})
	if a != nil && a.Shard < len(p.Status.Shards) {
		s := p.Status.Shards[a.Shard]
		b.Status.ExternalIP = s.PublicIP
		b.Status.ExternalPort = a.Port
		b.Status.NLBID = s.ID
		b.Status.BackendSet = a.Name
	}
	if reflect.DeepEqual(old.Status, b.Status) {
		return nil
	}
	if e := r.Status().Patch(ctx, b, client.MergeFrom(old)); e != nil {
		return e
	}
	eventType := core.EventTypeNormal
	if problem != nil {
		eventType = core.EventTypeWarning
	}
	r.Recorder.Event(b, eventType, reason, message)
	return nil
}
func (r *Reconciler) savePool(ctx context.Context, p *api.NLBPool) error {
	p.Status.Used = len(p.Status.Allocations)
	p.Status.Available = p.Spec.Occupancy*p.Spec.MaxShards - p.Status.Used
	return r.Status().Update(ctx, p)
}
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Cloud.ResetSnapshot()
	result, err := r.reconcile(ctx, req)
	r.report(ctx, req, result, err)
	if err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}
func (r *Reconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	again := ctrl.Result{RequeueAfter: 3 * time.Second}
	if req.Namespace != r.Namespace {
		return ctrl.Result{}, nil
	}
	p := &api.NLBPool{}
	if e := r.Reader.Get(ctx, req.NamespacedName, p); e != nil {
		return ctrl.Result{}, client.IgnoreNotFound(e)
	}
	if e := ValidatePool(p.Spec); e != nil {
		return again, e
	}
	if p.Spec.CompartmentID != r.Compartment || p.Spec.SubnetID != r.Subnet {
		return again, fmt.Errorf("pool outside configured compartment/subnet")
	}
	if p.Status.FrozenSpec != nil && !reflect.DeepEqual(*p.Status.FrozenSpec, p.Spec) {
		return again, fmt.Errorf("pool spec immutable after first reconciliation; restore original spec")
	}
	if !controllerutil.ContainsFinalizer(p, api.Finalizer) && p.DeletionTimestamp.IsZero() {
		controllerutil.AddFinalizer(p, api.Finalizer)
		return again, r.Update(ctx, p)
	}
	if p.Status.FrozenSpec == nil {
		s := p.Spec
		p.Status.FrozenSpec = &s
		return again, r.savePool(ctx, p)
	}
	if p.Status.PendingWork != "" {
		done, e := r.Cloud.Work(ctx, p.Status.PendingWork)
		if !done {
			return again, e
		}
		p.Status.PendingWork = ""
		if save := r.savePool(ctx, p); save != nil {
			return again, save
		}
		if e != nil {
			return again, e
		}
		return again, nil
	}
	if p.Spec.ProvisionEmpty && p.DeletionTimestamp.IsZero() {
		s, work, err := r.Cloud.EnsureShard(ctx, p, 0)
		if err != nil {
			return again, err
		}
		if len(p.Status.Shards) == 0 {
			p.Status.Shards = append(p.Status.Shards, api.Shard{})
		}
		if p.Status.Shards[0] != s || work != "" {
			p.Status.Shards[0] = s
			p.Status.PendingWork = work
			return again, r.savePool(ctx, p)
		}
	}
	var bindings api.TunnelBindingList
	if e := r.Reader.List(ctx, &bindings, client.InNamespace(p.Namespace)); e != nil {
		return again, e
	}
	sort.Slice(bindings.Items, func(i, j int) bool { return bindings.Items[i].Name < bindings.Items[j].Name })
	live := 0
	var endpointReader client.Reader
	var workers map[string]api.WorkerIdentity
	var workerProblem error
	workersRead := false
	prepareWorkers := func() (bool, error) {
		if workersRead {
			return false, workerProblem
		}
		workersRead = true
		if endpointReader == nil {
			snapshot, err := newEndpointSnapshot(ctx, r.Reader, p.Namespace)
			if err != nil {
				workerProblem = err
				return false, err
			}
			endpointReader = snapshot
		}
		workers, workerProblem = clusterWorkers(ctx, endpointReader, p)
		if workerProblem != nil {
			return false, workerProblem
		}
		if len(workers) == 0 && len(p.Status.Workers) == 0 {
			return false, nil
		}
		if !reflect.DeepEqual(workers, p.Status.Workers) {
			p.Status.Workers = workers
			return true, r.savePool(ctx, p) // Pin Node UID/provider/IP before native writes.
		}
		return false, nil
	}
	for i := range bindings.Items {
		b := &bindings.Items[i]
		a, allocated := p.Status.Allocations[string(b.UID)]
		if b.Spec.Pool != p.Name && !allocated {
			continue
		}
		live++
		if !allocated {
			if !b.DeletionTimestamp.IsZero() {
				// Allocation is persisted before any cloud mutation. A binding
				// rejected before allocation has no cloud objects to clean up.
				if controllerutil.ContainsFinalizer(b, api.Finalizer) {
					controllerutil.RemoveFinalizer(b, api.Finalizer)
					return again, r.Update(ctx, b)
				}
				continue
			}
			if !p.DeletionTimestamp.IsZero() {
				if e := r.bindingStatus(ctx, b, nil, p, fmt.Errorf("pool deleting")); e != nil {
					return again, e
				}
				continue
			}
			if !controllerutil.ContainsFinalizer(b, api.Finalizer) {
				controllerutil.AddFinalizer(b, api.Finalizer)
				return again, r.Update(ctx, b)
			}
			if b.Spec.Suspended {
				if err := r.bindingStatus(ctx, b, nil, p, fmt.Errorf("Gateway adapter rejected this route")); err != nil {
					return again, err
				}
				continue
			}
			svc := &core.Service{}
			nodePort := 0
			e := r.Reader.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.Service}, svc)
			if e == nil {
				e = validateBindingService(b, svc, p.Spec.PreserveSource)
			}
			if e == nil && b.Spec.Mode == "NodePortCluster" {
				udp, err := clusterServicePort(b, svc)
				if err != nil {
					e = err
				} else {
					nodePort = int(udp.NodePort)
				}
			}
			if e == nil && b.Spec.Mode == "NodePortCluster" {
				changed, err := prepareWorkers()
				if changed {
					return again, err
				}
				e = err
				if e == nil && len(workers) == 0 {
					e = fmt.Errorf("no verified workers; allocation deferred")
				}
			}
			if e != nil {
				if x := r.bindingStatus(ctx, b, nil, p, e); x != nil {
					return again, x
				}
				continue
			}
			allocation, allocationErr := Allocate(p, b, string(svc.UID))
			e = allocationErr
			if e != nil {
				if x := r.bindingStatus(ctx, b, nil, p, e); x != nil {
					return again, x
				}
				continue
			}
			if b.Spec.Mode == "NodePortCluster" {
				allocation.NodePort = nodePort
				p.Status.Allocations[string(b.UID)] = allocation
			}
			return again, r.savePool(ctx, p) // persist allocation BEFORE cloud calls
		}
		if a.Tombstone {
			if !b.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(b, api.Finalizer) {
				controllerutil.RemoveFinalizer(b, api.Finalizer)
				return again, r.Update(ctx, b)
			}
			continue
		}
		if !controllerutil.ContainsFinalizer(b, api.Finalizer) && b.DeletionTimestamp.IsZero() {
			controllerutil.AddFinalizer(b, api.Finalizer)
			return again, r.Update(ctx, b)
		}
		if b.DeletionTimestamp.IsZero() && !b.Spec.Suspended && endpointReader == nil {
			snapshot, err := newEndpointSnapshot(ctx, r.Reader, p.Namespace)
			if err != nil {
				return again, err
			}
			endpointReader = snapshot
		}
		if a.Mode == "NodePortCluster" && b.DeletionTimestamp.IsZero() && !b.Spec.Suspended {
			changed, err := prepareWorkers()
			if changed {
				return again, err
			}
			var readError *EndpointReadError
			if errors.As(err, &readError) {
				return again, err
			}
		}
		if a.Mode == "NodePortCluster" && a.NodePort == 0 && b.DeletionTimestamp.IsZero() && (a.Shard >= len(p.Status.Shards) || p.Status.Shards[a.Shard].ID == "") {
			if err := r.bindingStatus(ctx, b, &a, p, fmt.Errorf("legacy NodePort allocation has no native port proof; retire and recreate Route")); err != nil {
				return again, err
			}
			continue
		}
		s, work, e := r.Cloud.EnsureShard(ctx, p, a.Shard)
		if e != nil {
			return again, e
		}
		for len(p.Status.Shards) <= a.Shard {
			p.Status.Shards = append(p.Status.Shards, api.Shard{})
		}
		if p.Status.Shards[a.Shard] != s || work != "" {
			p.Status.Shards[a.Shard] = s
			p.Status.PendingWork = work
			return again, r.savePool(ctx, p)
		}
		removing := !b.DeletionTimestamp.IsZero()
		var backend *Backend
		var backends []Backend
		var problem error
		if !removing {
			if b.Spec.Suspended {
				problem = fmt.Errorf("Gateway adapter rejected this route")
			} else if b.Spec.Pool != p.Name || b.Spec.Service != a.ServiceName || b.Spec.PortName != a.PortName || b.Spec.Mode != a.Mode || (p.Spec.AllocationMode == "Explicit" && b.Spec.RequestedPort != a.Port) || (p.Spec.AllocationMode != "Explicit" && b.Spec.RequestedPort != 0) {
				problem = fmt.Errorf("binding target fields immutable; restore original spec")
			} else {
				if a.Mode == "NodePortCluster" {
					if a.NodePort == 0 {
						svc := &core.Service{}
						if err := endpointReader.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.Service}, svc); err != nil {
							problem = endpointGetError(err)
						} else if string(svc.UID) != a.ServiceUID || !svc.DeletionTimestamp.IsZero() {
							problem = fmt.Errorf("Service UID changed; cannot migrate NodePort allocation")
						} else if udp, err := clusterServicePort(b, svc); err != nil {
							problem = err
						} else {
							verified, err := r.Cloud.verifyLegacyNodePort(ctx, p, a, int(udp.NodePort))
							if err != nil {
								return again, err
							}
							if verified {
								a.NodePort = int(udp.NodePort)
								p.Status.Allocations[string(b.UID)] = a
								return again, r.savePool(ctx, p) // Upgrade proof persisted before any native change.
							}
							problem = fmt.Errorf("legacy NodePort allocation has no matching nonempty native port proof; retire and recreate Route for migration")
						}
					}
					var capacity *BackendCapacityError
					if problem != nil {
						// Preserve the explicit migration failure instead of resolving
						// a replacement port from current Service state.
					} else if errors.As(workerProblem, &capacity) {
						// An overflow pauses admission, not identity validation. Keep
						// only previously pinned identities still present in this read;
						// never retain a disappeared worker to mask a capacity problem.
						retained := map[string]api.WorkerIdentity{}
						for name, identity := range workers {
							if p.Status.Workers[name] == identity {
								retained[name] = identity
							}
						}
						backends, problem = resolveCluster(ctx, endpointReader, b, a, retained)
						var gap *ClusterEndpointGap
						if problem == nil || errors.As(problem, &gap) {
							problem = workerProblem
						}
					} else if workerProblem != nil {
						problem = workerProblem
					} else {
						backends, problem = resolveCluster(ctx, endpointReader, b, a, workers)
					}
				} else {
					backend, problem = Resolve(ctx, endpointReader, b, a)
				}
				var readError *EndpointReadError
				if errors.As(problem, &readError) {
					return again, problem // Unobserved state must not clear a healthy backend.
				}
			}
		}
		if e := ctx.Err(); e != nil {
			return again, e
		}
		healthPort := b.Spec.HealthPort
		if healthPort < 1 || healthPort > 65535 {
			backend = nil
			backends = nil
			problem = fmt.Errorf("invalid healthPort")
			healthPort = 9
		}
		if backend != nil {
			backends = []Backend{*backend}
		}
		var unavailable *BackendUnavailable
		if !removing && errors.As(problem, &unavailable) {
			hold, err := r.canRetainBackend(ctx, endpointReader, p, a, unavailable)
			if err != nil {
				return again, err
			}
			if hold {
				if err := r.bindingStatus(ctx, b, &a, p, problem); err != nil {
					return again, err
				}
				continue // Preserve the native flow; no clear-and-readd during a verified gap.
			}
		}
		for _, candidate := range backends {
			if removing {
				break
			}
			conflict, err := r.Cloud.destinationConflict(ctx, p, a, &candidate)
			if err != nil {
				return again, err
			}
			if conflict {
				// Clear only this binding's prior route. Its alias must not steal
				// another set's destination or stall every sibling with OCI 409s.
				backend = nil
				backends = nil
				problem = fmt.Errorf("Pod destination is already used by another binding on this NLB")
				break
			}
		}
		work, e = r.Cloud.StepBackends(ctx, p, a, backends, healthPort, removing)
		if e != nil {
			var capacity *BackendCapacityError
			if errors.As(e, &capacity) {
				if err := r.bindingStatus(ctx, b, &a, p, e); err != nil {
					return again, err
				}
				continue
			}
			return again, e
		}
		if work != "" {
			p.Status.PendingWork = work
			return again, r.savePool(ctx, p)
		}
		if removing {
			a.Tombstone = true
			p.Status.Allocations[string(b.UID)] = a
			return again, r.savePool(ctx, p)
		}
		if e = r.bindingStatus(ctx, b, &a, p, problem); e != nil {
			return again, e
		}
	}
	// Orphaned allocations (e.g. forced finalizer removal) are quarantined, never reused.
	present := map[string]bool{}
	for _, b := range bindings.Items {
		present[string(b.UID)] = true
	}
	for uid, a := range p.Status.Allocations {
		if !present[uid] && !a.Tombstone {
			if a.Shard >= len(p.Status.Shards) || p.Status.Shards[a.Shard].ID == "" {
				a.Tombstone = true
				p.Status.Allocations[uid] = a
				return again, r.savePool(ctx, p)
			}
			work, e := r.Cloud.Step(ctx, p, a, nil, 9, true)
			if e != nil {
				return again, e
			}
			if work != "" {
				p.Status.PendingWork = work
				return again, r.savePool(ctx, p)
			}
			a.Tombstone = true
			p.Status.Allocations[uid] = a
			return again, r.savePool(ctx, p)
		}
	}
	// Tombstones reserve the endpoint forever and must also stay absent in OCI.
	// Repair an externally resurrected listener instead of silently leaving a
	// retired endpoint active merely because its binding no longer exists.
	for _, a := range p.Status.Allocations {
		if !a.Tombstone || a.Shard >= len(p.Status.Shards) || p.Status.Shards[a.Shard].ID == "" {
			continue
		}
		work, e := r.Cloud.Step(ctx, p, a, nil, 9, true)
		if e != nil {
			return again, e
		}
		if work != "" {
			p.Status.PendingWork = work
			return again, r.savePool(ctx, p)
		}
	}
	if !p.DeletionTimestamp.IsZero() && live == 0 {
		controllerutil.RemoveFinalizer(p, api.Finalizer)
		return ctrl.Result{}, r.Update(ctx, p)
	} // NLBs always retained
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// Retention never selects or recreates a backend. It only leaves an existing
// verified node/NodePort in place while Local forwarding and fail-closed native
// health checks gate delivery. Missing identity, port changes or unsafe cloud
// configuration falls through to normal fail-closed reconciliation.
func (r *Reconciler) canRetainBackend(ctx context.Context, reader client.Reader, p *api.NLBPool, a api.Allocation, gap *BackendUnavailable) (bool, error) {
	if a.Shard < 0 || a.Shard >= len(p.Status.Shards) {
		return false, nil
	}
	response, err := r.Cloud.get(ctx, p.Status.Shards[a.Shard].ID)
	if err != nil {
		return false, err
	}
	lb := response.NetworkLoadBalancer
	if !r.Cloud.owns(p, a.Shard, lb) || string(lb.LifecycleState) != "ACTIVE" {
		return false, nil
	}
	owned := map[string]bool{}
	for _, allocation := range p.Status.Allocations {
		if allocation.Shard == a.Shard {
			owned[allocation.Name] = true
		}
	}
	for name := range lb.Listeners {
		if !owned[name] {
			return false, nil
		}
	}
	for name := range lb.BackendSets {
		if !owned[name] {
			return false, nil
		}
	}
	bs, found := lb.BackendSets[a.Name]
	if !found || len(bs.Backends) != 1 || !sameHealth(bs.HealthChecker, gap.HealthPort) || bs.Policy != n.NetworkLoadBalancingPolicyFiveTuple || value(bs.IsPreserveSource) != p.Spec.PreserveSource || value(bs.IsFailOpen) || !value(bs.IsInstantFailoverEnabled) {
		return false, nil
	}
	backend := bs.Backends[0]
	if value(backend.Port) != gap.NodePort || value(backend.TargetId) == "" || value(backend.Weight) != 1 || value(backend.IsBackup) || value(backend.IsOffline) || value(backend.IsDrain) {
		return false, nil
	}
	listener, found := lb.Listeners[a.Name]
	if !found || value(listener.Port) != a.Port || value(listener.DefaultBackendSetName) != a.Name || string(listener.Protocol) != "UDP" || value(listener.IsPpv2Enabled) || value(listener.UdpIdleTimeout) != 120 {
		return false, nil
	}
	var nodes core.NodeList
	if err := reader.List(ctx, &nodes); err != nil {
		return false, &EndpointReadError{Err: err}
	}
	for _, node := range nodes.Items {
		if strings.TrimPrefix(node.Spec.ProviderID, "oci://") != value(backend.TargetId) || !node.DeletionTimestamp.IsZero() {
			continue
		}
		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == core.NodeReady && condition.Status == core.ConditionTrue {
				ready = true
			}
		}
		if !ready {
			continue
		}
		for _, address := range node.Status.Addresses {
			if address.Type == core.NodeInternalIP && address.Address == value(backend.IpAddress) {
				return true, nil
			}
		}
	}
	return false, nil
}
