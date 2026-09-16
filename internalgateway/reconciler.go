package internalgateway

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	core "k8s.io/api/core/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gateway "sigs.k8s.io/gateway-api/apis/v1"
)

const poolUIDAnnotation = "nlb.independent.dev/internal-pool-uid"
const PoolArchiveFinalizer = "nlb.independent.dev/gateway-pool-archive"

type Reconciler struct {
	client.Client
	Reader                                                                         client.Reader // Use the manager APIReader, especially for exact GatewayClass GETs.
	Namespace, GatewayClassName, ControllerName, Installation, Compartment, Subnet string
}

func (r *Reconciler) Setup(manager ctrl.Manager) error {
	if r.Namespace == "" || r.GatewayClassName == "" || r.ControllerName == "" || r.Installation == "" || r.Compartment == "" || r.Subnet == "" {
		return fmt.Errorf("Gateway adapter configuration is incomplete")
	}
	enqueue := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		if o.GetNamespace() != r.Namespace {
			return nil
		}
		var list gateway.GatewayList
		if err := r.List(ctx, &list, client.InNamespace(r.Namespace)); err != nil {
			return nil
		}
		out := []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: "gateway-cleanup-sweep"}}}
		for _, g := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&g)})
		}
		return out
	})
	return ctrl.NewControllerManagedBy(manager).Named("native-udp-gateway").For(&gateway.Gateway{}).Watches(&gateway.UDPRoute{}, enqueue).Watches(&api.NLBPool{}, enqueue).Watches(&api.TunnelBinding{}, enqueue).Watches(&core.Service{}, enqueue).WithOptions(controller.Options{MaxConcurrentReconciles: 1}).Complete(r)
}

func (r *Reconciler) labels(gatewayUID, routeUID types.UID) map[string]string {
	labels := map[string]string{GatewayUIDLabel: string(gatewayUID), InstallationLabel: r.Installation}
	if routeUID != "" {
		labels[RouteUIDLabel] = string(routeUID)
	}
	return labels
}
func gatewayOwner(g *gateway.Gateway) metav1.OwnerReference {
	yes := true
	return metav1.OwnerReference{APIVersion: gateway.GroupVersion.String(), Kind: "Gateway", Name: g.Name, UID: g.UID, Controller: &yes, BlockOwnerDeletion: &yes}
}
func (r *Reconciler) owns(object client.Object, gatewayUID types.UID) bool {
	if object.GetNamespace() != r.Namespace || gatewayUID == "" || object.GetLabels()[GatewayUIDLabel] != string(gatewayUID) || object.GetLabels()[InstallationLabel] != r.Installation {
		return false
	}
	for _, owner := range object.GetOwnerReferences() {
		if owner.UID == gatewayUID && owner.APIVersion == gateway.GroupVersion.String() && owner.Kind == "Gateway" && owner.Controller != nil && *owner.Controller {
			return true
		}
	}
	return false
}
func (r *Reconciler) ownsBinding(b *api.TunnelBinding) bool {
	g, route := types.UID(b.Labels[GatewayUIDLabel]), types.UID(b.Labels[RouteUIDLabel])
	return route != "" && r.owns(b, g) && b.Name == BindingName(g, route) && b.Spec.Pool == PoolName(g)
}
func (r *Reconciler) deleteExact(ctx context.Context, obj client.Object) error {
	uid, version := obj.GetUID(), obj.GetResourceVersion()
	if uid == "" {
		return fmt.Errorf("refusing deletion without UID")
	}
	return client.IgnoreNotFound(r.Delete(ctx, obj, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}}))
}

func (r *Reconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	again := ctrl.Result{RequeueAfter: 15 * time.Second}
	if request.Namespace != r.Namespace {
		return ctrl.Result{}, nil
	}
	if err := ctx.Err(); err != nil {
		return again, err
	}
	var routes gateway.UDPRouteList
	var bindings api.TunnelBindingList
	var pools api.NLBPoolList
	if err := r.Reader.List(ctx, &routes, client.InNamespace(r.Namespace)); err != nil {
		return again, err
	}
	if err := r.Reader.List(ctx, &bindings, client.InNamespace(r.Namespace)); err != nil {
		return again, err
	}
	if err := r.Reader.List(ctx, &pools, client.InNamespace(r.Namespace)); err != nil {
		return again, err
	}
	if changed, err := r.cleanupRoutes(ctx, routes.Items, bindings.Items, pools.Items); changed || err != nil {
		return again, err
	}
	g := &gateway.Gateway{}
	if err := r.Reader.Get(ctx, request.NamespacedName, g); err != nil {
		return again, client.IgnoreNotFound(err)
	}
	if string(g.Spec.GatewayClassName) != r.GatewayClassName && !controllerutil.ContainsFinalizer(g, GatewayFinalizer) {
		return ctrl.Result{}, nil
	}
	if !g.DeletionTimestamp.IsZero() {
		if err := r.rejectRoutes(ctx, g, routes.Items, "Gateway is being deleted"); err != nil {
			return again, err
		}
		return again, r.retireGateway(ctx, g, bindings.Items)
	}
	class := &gateway.GatewayClass{}
	classErr := r.Reader.Get(ctx, types.NamespacedName{Name: r.GatewayClassName}, class)
	if classErr != nil && !errors.IsNotFound(classErr) {
		return again, classErr
	}
	problem := validateGateway(g)
	if string(g.Spec.GatewayClassName) != r.GatewayClassName || classErr != nil || string(class.Spec.ControllerName) != r.ControllerName {
		problem = fmt.Errorf("configured GatewayClass/controller is missing or does not match")
	} else {
		previous := class.DeepCopy()
		ok := class.Spec.ParametersRef == nil && class.DeletionTimestamp.IsZero()
		reason, message := "Accepted", "Restricted same-namespace UDP profile; full Gateway API conformance is not claimed"
		if !ok {
			reason, message = "InvalidParameters", "GatewayClass parameters and deletion are unsupported"
			problem = fmt.Errorf("%s", message)
		}
		meta.SetStatusCondition(&class.Status.Conditions, condition("Accepted", ok, reason, message, class.Generation))
		class.Status.SupportedFeatures = nil
		if !reflect.DeepEqual(previous.Status, class.Status) {
			if err := r.Status().Patch(ctx, class, client.MergeFrom(previous)); err != nil {
				return again, err
			}
		}
	}
	if problem != nil {
		for i := range bindings.Items {
			b := &bindings.Items[i]
			if r.ownsBinding(b) && b.Labels[GatewayUIDLabel] == string(g.UID) {
				if err := r.disableBinding(ctx, b); err != nil {
					return again, err
				}
			}
		}
		if err := r.rejectRoutes(ctx, g, routes.Items, problem.Error()); err != nil {
			return again, err
		}
		return again, r.gatewayStatus(ctx, g, nil, nil, problem)
	}
	if !controllerutil.ContainsFinalizer(g, GatewayFinalizer) {
		controllerutil.AddFinalizer(g, GatewayFinalizer)
		return again, r.Update(ctx, g)
	}
	pool, err := r.ensurePool(ctx, g)
	if err != nil {
		if statusErr := r.gatewayStatus(ctx, g, nil, nil, err); statusErr != nil {
			return again, statusErr
		}
		return again, err
	}
	// A fresh bulk snapshot bounds Kubernetes reads as route count grows; an
	// operational List failure never suspends existing forwarding.
	var services core.ServiceList
	if err := r.Reader.List(ctx, &services, client.InNamespace(r.Namespace)); err != nil {
		return again, err
	}
	serviceByName := map[string]*core.Service{}
	for i := range services.Items {
		serviceByName[services.Items[i].Name] = &services.Items[i]
	}
	states := map[gateway.SectionName]listenerState{}
	sort.Slice(routes.Items, func(i, j int) bool {
		a, b := routes.Items[i], routes.Items[j]
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
	claimed := map[gateway.SectionName]bool{}
	for i := range routes.Items {
		route := &routes.Items[i]
		if !route.DeletionTimestamp.IsZero() {
			continue
		}
		targeted := false
		for _, parent := range route.Spec.ParentRefs {
			if parentMatches(parent, g, route.Namespace) {
				targeted = true
			}
		}
		if !targeted {
			continue
		} // Unattached fleet routes belong to the allocator until it sets ParentRefs.
		listener, validation := listenerFor(route, g)
		accepted, resolved, programmed := false, false, false
		reason, message := "UnsupportedValue", ""
		var spec api.BindingSpec
		if validation == nil {
			if claimed[listener.Name] {
				validation = fmt.Errorf("an older route already owns this listener")
				reason = "Conflicted"
			} else {
				claimed[listener.Name] = true
				accepted = true
			}
		}
		if validation == nil {
			service, _, err := backendService(route)
			validation = err
			if err == nil {
				svc := serviceByName[service]
				if svc == nil {
					validation = fmt.Errorf("backend Service not found")
					reason = "BackendNotFound"
				} else {
					spec, validation = desiredBinding(route, g, listener, svc)
				}
			}
			resolved = validation == nil
		}
		if validation == nil {
			// A previous route's lease survives Route deletion and prevents a stale
			// client from being silently sent to a new owner at the same IP:port.
			bindingName := BindingName(g.UID, route.UID)
			for _, allocation := range pool.Status.Allocations {
				if allocation.Port == spec.RequestedPort && allocation.BindingName != bindingName {
					validation = fmt.Errorf("listener port has a permanent lease to another Route UID; use a new port or Gateway")
					accepted = false
					reason = "PortAlreadyLeased"
				}
			}
		}
		if validation == nil {
			if !controllerutil.ContainsFinalizer(route, RouteFinalizer) {
				controllerutil.AddFinalizer(route, RouteFinalizer)
				if err := r.Update(ctx, route); err != nil {
					return again, err
				}
				continue
			}
			binding, err := r.ensureBinding(ctx, g, route, spec)
			if err != nil {
				if _, rejected := err.(*immutableTargetError); rejected {
					if err := r.routeStatus(ctx, g, route, false, resolved, false, "UnsupportedValue", err.Error()); err != nil {
						return again, err
					}
					continue
				}
				return again, err
			}
			ready := meta.FindStatusCondition(binding.Status.Conditions, "Configured")
			programmed = ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == binding.Generation && pool.Status.PendingWork == "" && binding.Status.ExternalPort == spec.RequestedPort && len(pool.Status.Shards) == 1 && binding.Status.ExternalIP == pool.Status.Shards[0].PublicIP && binding.Status.ExternalIP != "" && binding.Status.NLBID == pool.Status.Shards[0].ID
			reason, message = "Accepted", "Route accepted; waiting for native configuration"
			if programmed {
				message = "Native configuration converged; probe application health independently"
			}
			if ready != nil && !programmed {
				message = ready.Message
			}
		} else {
			message = validation.Error()
			for j := range bindings.Items {
				b := &bindings.Items[j]
				if b.Name == BindingName(g.UID, route.UID) {
					if !r.ownsBinding(b) {
						return again, fmt.Errorf("generated binding name occupied by foreign object")
					}
					if err := r.disableBinding(ctx, b); err != nil {
						return again, err
					}
				}
			}
		}
		if err := r.routeStatus(ctx, g, route, accepted, resolved, programmed, reason, message); err != nil {
			return again, err
		}
		if listener != nil && accepted {
			states[listener.Name] = listenerState{attached: 1, programmed: programmed, message: message}
		}
	}
	return again, r.gatewayStatus(ctx, g, pool, states, nil)
}

func (r *Reconciler) ensurePool(ctx context.Context, g *gateway.Gateway) (*api.NLBPool, error) {
	p := &api.NLBPool{}
	key := types.NamespacedName{Namespace: g.Namespace, Name: PoolName(g.UID)}
	maxWorkers, capacity, workerErr := gatewayWorkers(g)
	if workerErr != nil {
		return nil, workerErr
	}
	spec := api.PoolSpec{CompartmentID: r.Compartment, SubnetID: r.Subnet, Occupancy: capacity, MaxShards: 1, PortStart: 1, AllocationMode: "Explicit", ProvisionEmpty: true, MaxWorkers: maxWorkers}
	err := r.Reader.Get(ctx, key, p)
	if errors.IsNotFound(err) {
		if g.Annotations[poolUIDAnnotation] != "" {
			return nil, fmt.Errorf("owned pool disappeared; refusing to replace durable port ledger")
		}
		p = &api.NLBPool{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: r.labels(g.UID, ""), OwnerReferences: []metav1.OwnerReference{gatewayOwner(g)}, Finalizers: []string{api.Finalizer, PoolArchiveFinalizer}}, Spec: spec}
		if err = r.Create(ctx, p); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if !r.owns(p, g.UID) || !reflect.DeepEqual(p.Spec, spec) || !p.DeletionTimestamp.IsZero() || (g.Annotations[poolUIDAnnotation] != "" && g.Annotations[poolUIDAnnotation] != string(p.UID)) {
		return nil, fmt.Errorf("generated pool ownership, UID or immutable spec mismatch")
	}
	// A parent finalizer does not protect its children from cascading garbage
	// collection. Upgrade live owned pools before using their durable ledger.
	if !controllerutil.ContainsFinalizer(p, PoolArchiveFinalizer) {
		controllerutil.AddFinalizer(p, PoolArchiveFinalizer)
		if err := r.Update(ctx, p); err != nil {
			return nil, err
		}
	}
	if g.Annotations[poolUIDAnnotation] == "" {
		old := g.DeepCopy()
		if g.Annotations == nil {
			g.Annotations = map[string]string{}
		}
		g.Annotations[poolUIDAnnotation] = string(p.UID)
		if err := r.Patch(ctx, g, client.MergeFrom(old)); err != nil {
			return nil, err
		}
	}
	return p, nil
}

type immutableTargetError struct{}

func (*immutableTargetError) Error() string {
	return "Route backend Service/port and listener port are immutable once bound; restore the original target or migrate to a new endpoint"
}
func sameBindingTarget(a, b api.BindingSpec) bool {
	return a.Pool == b.Pool && a.Service == b.Service && a.PortName == b.PortName && a.Mode == b.Mode && a.RequestedPort == b.RequestedPort
}

func (r *Reconciler) ensureBinding(ctx context.Context, g *gateway.Gateway, route *gateway.UDPRoute, spec api.BindingSpec) (*api.TunnelBinding, error) {
	b := &api.TunnelBinding{}
	key := types.NamespacedName{Namespace: g.Namespace, Name: BindingName(g.UID, route.UID)}
	err := r.Reader.Get(ctx, key, b)
	if errors.IsNotFound(err) {
		b = &api.TunnelBinding{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: r.labels(g.UID, route.UID), OwnerReferences: []metav1.OwnerReference{gatewayOwner(g)}}, Spec: spec}
		return b, r.Create(ctx, b)
	}
	if err != nil {
		return nil, err
	}
	if !r.ownsBinding(b) || b.Labels[RouteUIDLabel] != string(route.UID) || !b.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("generated binding ownership mismatch or cleanup pending")
	}
	if !sameBindingTarget(b.Spec, spec) {
		if err := r.disableBinding(ctx, b); err != nil {
			return nil, err
		}
		return nil, &immutableTargetError{}
	}
	if !reflect.DeepEqual(b.Spec, spec) {
		old := b.DeepCopyObject().(*api.TunnelBinding)
		b.Spec = spec
		if err := r.Patch(ctx, b, client.MergeFrom(old)); err != nil {
			return nil, err
		}
		b.Status = api.BindingStatus{}
	}
	return b, nil
}
func (r *Reconciler) disableBinding(ctx context.Context, b *api.TunnelBinding) error {
	if !r.ownsBinding(b) {
		return fmt.Errorf("cannot disable unowned binding")
	}
	if b.Spec.Suspended || !b.DeletionTimestamp.IsZero() {
		return nil
	}
	old := b.DeepCopyObject().(*api.TunnelBinding)
	b.Spec.Suspended = true
	return r.Patch(ctx, b, client.MergeFrom(old)) // Core clears the backend; preserve the durable lease for correction.
}

func (r *Reconciler) routeStatus(ctx context.Context, g *gateway.Gateway, route *gateway.UDPRoute, accepted, resolved, programmed bool, reason, message string) error {
	old := route.DeepCopy()
	parents := []gateway.RouteParentStatus{}
	for _, p := range route.Status.Parents {
		if string(p.ControllerName) != r.ControllerName || !parentMatches(p.ParentRef, g, route.Namespace) {
			parents = append(parents, p)
		}
	}
	for _, parent := range route.Spec.ParentRefs {
		if !parentMatches(parent, g, route.Namespace) {
			continue
		}
		status := gateway.RouteParentStatus{ParentRef: parent, ControllerName: gateway.GatewayController(r.ControllerName)}
		for _, prior := range old.Status.Parents {
			if prior.ControllerName == status.ControllerName && reflect.DeepEqual(prior.ParentRef, parent) {
				status.Conditions = append([]metav1.Condition(nil), prior.Conditions...)
			}
		}
		acceptedReason := reason
		if accepted {
			acceptedReason = "Accepted"
		}
		resolvedReason := reason
		if resolved {
			resolvedReason = "ResolvedRefs"
		}
		programmedReason := "Pending"
		if programmed {
			programmedReason = "Programmed"
		}
		meta.SetStatusCondition(&status.Conditions, condition("Accepted", accepted, acceptedReason, message, route.Generation))
		meta.SetStatusCondition(&status.Conditions, condition("ResolvedRefs", resolved, resolvedReason, message, route.Generation))
		meta.SetStatusCondition(&status.Conditions, condition("Programmed", programmed, programmedReason, message, route.Generation))
		parents = append(parents, status)
	}
	route.Status.Parents = parents
	if reflect.DeepEqual(old.Status, route.Status) {
		return nil
	}
	return r.Status().Patch(ctx, route, client.MergeFrom(old))
}

type listenerState struct {
	attached   int32
	programmed bool
	message    string
}

func (r *Reconciler) rejectRoutes(ctx context.Context, g *gateway.Gateway, routes []gateway.UDPRoute, message string) error {
	for i := range routes {
		route := &routes[i]
		targeted := false
		for _, p := range route.Spec.ParentRefs {
			if parentMatches(p, g, route.Namespace) {
				targeted = true
			}
		}
		if targeted {
			if err := r.routeStatus(ctx, g, route, false, false, false, "UnsupportedValue", message); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Reconciler) gatewayStatus(ctx context.Context, g *gateway.Gateway, p *api.NLBPool, states map[gateway.SectionName]listenerState, problem error) error {
	old := g.DeepCopy()
	g.Status.Addresses = nil
	if p != nil && len(p.Status.Shards) == 1 && p.Status.Shards[0].PublicIP != "" {
		addressType := gateway.IPAddressType
		g.Status.Addresses = []gateway.GatewayStatusAddress{{Type: &addressType, Value: p.Status.Shards[0].PublicIP}}
	}
	all := problem == nil && p != nil && p.Status.PendingWork == "" && len(g.Status.Addresses) == 1
	g.Status.Listeners = nil
	for _, l := range g.Spec.Listeners {
		state := states[l.Name]
		status := gateway.ListenerStatus{Name: l.Name, SupportedKinds: []gateway.RouteGroupKind{{Kind: "UDPRoute"}}, AttachedRoutes: state.attached}
		for _, prior := range old.Status.Listeners {
			if prior.Name == l.Name {
				status.Conditions = append([]metav1.Condition(nil), prior.Conditions...)
			}
		}
		reason, message := "Programmed", state.message
		if state.attached == 0 {
			reason, message = "WaitingForRoute", "Listener has no accepted route"
		}
		if state.attached > 0 && !state.programmed {
			reason = "Pending"
		}
		if problem != nil {
			reason, message = "UnsupportedValue", problem.Error()
		}
		ready := problem == nil && state.programmed
		conflicted := false
		for _, other := range g.Spec.Listeners {
			if other.Name != l.Name && other.Port == l.Port && other.Protocol == l.Protocol {
				conflicted = true
			}
		}
		conflictReason := "NoConflicts"
		if conflicted {
			conflictReason = "ProtocolConflict"
		}
		meta.SetStatusCondition(&status.Conditions, condition("Conflicted", conflicted, conflictReason, message, g.Generation))
		meta.SetStatusCondition(&status.Conditions, condition("Accepted", problem == nil, map[bool]string{true: "Accepted", false: "UnsupportedValue"}[problem == nil], message, g.Generation))
		meta.SetStatusCondition(&status.Conditions, condition("Programmed", ready, reason, message, g.Generation))
		g.Status.Listeners = append(g.Status.Listeners, status)
		all = all && ready
	}
	reason, message := "Accepted", "Restricted UDP profile accepted"
	if problem != nil {
		reason, message = "UnsupportedValue", problem.Error()
	}
	meta.SetStatusCondition(&g.Status.Conditions, condition("Accepted", problem == nil, reason, message, g.Generation))
	programmedReason := "Pending"
	if all {
		programmedReason = "Programmed"
		message = "Native configuration converged; application readiness is independently measured"
	} else if problem == nil {
		message = "Waiting for the NLB and every declared listener to have a configured route"
	}
	meta.SetStatusCondition(&g.Status.Conditions, condition("Programmed", all, programmedReason, message, g.Generation))
	if reflect.DeepEqual(old.Status, g.Status) {
		return nil
	}
	return r.Status().Patch(ctx, g, client.MergeFrom(old))
}
