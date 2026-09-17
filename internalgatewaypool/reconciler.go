// Package internalgatewaypool assigns durable leases to opt-in UDP routes.
// It never calls OCI. Generated standard Gateways each represent one NLB.
package internalgatewaypool

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"time"

	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gw "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	PoolAnnotation    = "nlb.independent.dev/pool"
	HealthAnnotation  = "nlb.independent.dev/health-port"
	PoolUIDLabel      = "nlb.independent.dev/gateway-pool-uid"
	InstallationLabel = "nlb.independent.dev/installation"
	Finalizer         = "nlb.independent.dev/gateway-pool-cleanup"
	RouteFinalizer    = "nlb.independent.dev/gateway-route-cleanup"
	ModeAnnotation    = "nlb.independent.dev/backend-mode"
	WorkersAnnotation = "nlb.independent.dev/max-workers"
)

func routeMode(route *gw.UDPRoute) string {
	mode := route.Annotations[ModeAnnotation]
	if mode == "" {
		return "PodIP"
	}
	return mode
}
func leaseMode(a api.GatewayAllocation) string {
	if a.Mode == "" {
		return "PodIP"
	}
	return a.Mode
}

type Reconciler struct {
	client.Client
	Reader       client.Reader
	Namespace    string
	Installation string
	ClassName    string
}

type validationError struct{ message string }

func (e *validationError) Error() string       { return e.message }
func invalid(format string, args ...any) error { return &validationError{fmt.Sprintf(format, args...)} }

func (r *Reconciler) Setup(m ctrl.Manager) error {
	enqueue := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var pools api.GatewayPoolList
		if err := r.List(ctx, &pools, client.InNamespace(r.Namespace)); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(pools.Items))
		for _, p := range pools.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&p)})
		}
		return out
	})
	return ctrl.NewControllerManagedBy(m).For(&api.GatewayPool{}).
		Watches(&gw.UDPRoute{}, enqueue).Watches(&gw.Gateway{}, enqueue).Watches(&core.Service{}, enqueue).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).Complete(r)
}

func Validate(s api.GatewayPoolSpec) error {
	workers := s.MaxWorkers
	if workers == 0 {
		workers = 20
	}
	if workers < 1 || workers > 512 || s.Occupancy*workers > 1024 {
		return fmt.Errorf("occupancy * maxWorkers must fit 1024 backend budget; default 20 includes surge")
	}
	if s.GatewayClassName == "" || s.Occupancy < 1 || s.Occupancy > 50 || s.MaxGateways < 1 || s.MaxGateways > 100 || s.PortStart < 1024 || s.PortStart+s.Occupancy-1 > 65535 {
		return fmt.Errorf("invalid GatewayPool class/capacity/port bounds")
	}
	return nil
}

func gatewayName(uid types.UID, shard int) string {
	h := sha256.Sum256([]byte(uid))
	return fmt.Sprintf("gwp-%x-%d", h[:12], shard)
}

func reserve(p *api.GatewayPool, route *gw.UDPRoute, svc *core.Service, port, health int32) (api.GatewayAllocation, error) {
	if mode := routeMode(route); mode != "PodIP" && mode != "NodePortCluster" && mode != "NodePortLocal" {
		return api.GatewayAllocation{}, fmt.Errorf("unsupported backend-mode")
	}
	if err := Validate(p.Spec); err != nil {
		return api.GatewayAllocation{}, err
	}
	if p.UID == "" || route.UID == "" || svc.UID == "" {
		return api.GatewayAllocation{}, fmt.Errorf("pool, route and Service UIDs are required")
	}
	if a, ok := p.Status.Allocations[string(route.UID)]; ok {
		return a, nil
	}
	occupied := map[[2]int]bool{}
	for _, a := range p.Status.Allocations {
		if !a.Tombstone && a.ServiceUID == string(svc.UID) && a.ServicePort == port {
			return api.GatewayAllocation{}, fmt.Errorf("Service port already allocated in this pool")
		}
		occupied[[2]int{a.Shard, int(a.Port)}] = true
	}
	for shard := 0; shard < p.Spec.MaxGateways; shard++ {
		for offset := 0; offset < p.Spec.Occupancy; offset++ {
			portNumber := p.Spec.PortStart + offset
			if occupied[[2]int{shard, portNumber}] {
				continue
			}
			h := sha256.Sum256([]byte(route.UID))
			a := api.GatewayAllocation{RouteName: route.Name, ServiceName: svc.Name, ServiceUID: string(svc.UID), ServicePort: port, HealthPort: health, Mode: routeMode(route),
				GatewayName: gatewayName(p.UID, shard), Shard: shard, ListenerName: fmt.Sprintf("r-%x", h[:12]), Port: int32(portNumber)}
			if p.Status.Allocations == nil {
				p.Status.Allocations = map[string]api.GatewayAllocation{}
			}
			p.Status.Allocations[string(route.UID)] = a
			return a, nil
		}
	}
	return api.GatewayAllocation{}, fmt.Errorf("GatewayPool exhausted, including permanently reserved tombstones")
}

func (r *Reconciler) routeService(ctx context.Context, route *gw.UDPRoute) (*core.Service, int32, int32, error) {
	if route.Spec.UseDefaultGateways != "" && route.Spec.UseDefaultGateways != gw.GatewayDefaultScopeNone {
		return nil, 0, 0, invalid("useDefaultGateways is unsupported; pool assignment requires one explicit parent")
	}
	if len(route.Spec.Rules) != 1 || len(route.Spec.Rules[0].BackendRefs) != 1 {
		return nil, 0, 0, invalid("exactly one rule and one Service backend required")
	}
	b := route.Spec.Rules[0].BackendRefs[0]
	if (b.Group != nil && *b.Group != "") || (b.Kind != nil && *b.Kind != "Service") || (b.Namespace != nil && string(*b.Namespace) != route.Namespace) || b.Port == nil || b.Name == "" || (b.Weight != nil && *b.Weight != 1) {
		return nil, 0, 0, invalid("one same-namespace Service port with weight one is required")
	}
	health, err := strconv.ParseInt(route.Annotations[HealthAnnotation], 10, 32)
	mode := routeMode(route)
	if mode != "PodIP" && mode != "NodePortCluster" && mode != "NodePortLocal" {
		return nil, 0, 0, invalid("unsupported backend-mode")
	}
	if mode == "NodePortCluster" {
		if annotation := route.Annotations[HealthAnnotation]; annotation != "" && annotation != "10256" {
			return nil, 0, 0, invalid("NodePortCluster uses worker health 10256; omit workload health annotation")
		}
		health, err = 10256, nil
	}
	if err != nil || health < 1 || health > 65535 {
		return nil, 0, 0, invalid("valid %s annotation required", HealthAnnotation)
	}
	svc := &core.Service{}
	if err = r.Reader.Get(ctx, types.NamespacedName{Namespace: route.Namespace, Name: string(b.Name)}, svc); err != nil {
		return nil, 0, 0, err
	}
	serviceTypeOK := svc.Spec.Type == core.ServiceTypeClusterIP
	if mode == "NodePortCluster" {
		serviceTypeOK = svc.Spec.Type == core.ServiceTypeNodePort && svc.Spec.ExternalTrafficPolicy == core.ServiceExternalTrafficPolicyCluster
	} else if mode == "NodePortLocal" {
		serviceTypeOK = svc.Spec.Type == core.ServiceTypeNodePort && svc.Spec.ExternalTrafficPolicy == core.ServiceExternalTrafficPolicyLocal
	}
	if !svc.DeletionTimestamp.IsZero() || !serviceTypeOK || svc.Spec.PublishNotReadyAddresses || len(svc.Spec.Selector) == 0 {
		return nil, 0, 0, invalid("live selector Service with the mode's traffic policy and without publishNotReadyAddresses required")
	}
	healthExposed := false
	healthMatches := 0
	for _, p := range svc.Spec.Ports {
		if p.Protocol != core.ProtocolTCP {
			continue
		}
		if mode == "NodePortLocal" {
			if p.Port == int32(health) && p.NodePort >= 1 && p.NodePort <= 65535 {
				healthExposed = true
				healthMatches++
			}
		} else {
			// A named target needs the adapter's Pod/EndpointSlice validation. Numeric
			// targets can be rejected here without consuming a permanent lease.
			target := p.TargetPort.IntVal
			if target == 0 {
				target = p.Port
			}
			if p.TargetPort.Type == 1 || target == int32(health) {
				healthExposed = true
			}
		}
	}
	if !healthExposed && mode != "NodePortCluster" {
		return nil, 0, 0, invalid("health port must be exposed by a TCP Service target")
	}
	if mode == "NodePortLocal" && healthMatches != 1 {
		return nil, 0, 0, invalid("NodePortLocal requires exactly one allocated TCP health Service port")
	}
	for _, p := range svc.Spec.Ports {
		if p.Port == int32(*b.Port) && p.Protocol == core.ProtocolUDP && p.Name != "" {
			if (mode == "NodePortCluster" || mode == "NodePortLocal") && (p.NodePort < 1 || p.NodePort > 65535) {
				return nil, 0, 0, invalid("UDP NodePort must be allocated")
			}
			return svc, p.Port, int32(health), nil
		}
	}
	return nil, 0, 0, invalid("backend port must identify a named UDP Service port")
}

func matchesParent(route *gw.UDPRoute, a api.GatewayAllocation) bool {
	if len(route.Spec.ParentRefs) != 1 {
		return false
	}
	p := route.Spec.ParentRefs[0]
	return string(p.Name) == a.GatewayName && p.SectionName != nil && string(*p.SectionName) == a.ListenerName &&
		(p.Group == nil || *p.Group == gw.GroupName) && (p.Kind == nil || *p.Kind == "Gateway") &&
		(p.Namespace == nil || string(*p.Namespace) == route.Namespace) && (p.Port == nil || int32(*p.Port) == a.Port)
}

func desiredGateway(p *api.GatewayPool, name, installation string) *gw.Gateway {
	controller, block := true, true
	kind := gw.Kind("UDPRoute")
	group := gw.Group(gw.GroupName)
	same := gw.NamespacesFromSame
	result := &gw.Gateway{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.Namespace,
		Labels:          map[string]string{PoolUIDLabel: string(p.UID), InstallationLabel: installation},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "GatewayPool", Name: p.Name, UID: p.UID, Controller: &controller, BlockOwnerDeletion: &block}}},
		Spec: gw.GatewaySpec{GatewayClassName: gw.ObjectName(p.Spec.GatewayClassName)}}
	if p.Spec.MaxWorkers != 0 {
		result.Annotations = map[string]string{WorkersAnnotation: strconv.Itoa(p.Spec.MaxWorkers)}
	}
	// The durable lease, rather than a stale Listener, prevents port reuse.
	// Keep one inert placeholder only when every lease on this shard is retired:
	// Gateway requires at least one Listener and the empty shard keeps its VIP.
	var retired []gw.Listener
	for _, a := range p.Status.Allocations {
		if a.GatewayName == name {
			listener := gw.Listener{Name: gw.SectionName(a.ListenerName), Port: gw.PortNumber(a.Port), Protocol: gw.UDPProtocolType,
				AllowedRoutes: &gw.AllowedRoutes{Namespaces: &gw.RouteNamespaces{From: &same}, Kinds: []gw.RouteGroupKind{{Group: &group, Kind: kind}}}}
			if a.Tombstone {
				retired = append(retired, listener)
			} else {
				result.Spec.Listeners = append(result.Spec.Listeners, listener)
			}
		}
	}
	if len(result.Spec.Listeners) == 0 && len(retired) > 0 {
		sort.Slice(retired, func(i, j int) bool { return retired[i].Port < retired[j].Port })
		result.Spec.Listeners = retired[:1]
	}
	sort.Slice(result.Spec.Listeners, func(i, j int) bool { return result.Spec.Listeners[i].Port < result.Spec.Listeners[j].Port })
	return result
}

func ownedGateway(p *api.GatewayPool, g *gw.Gateway, installation string) error {
	if g.Namespace != p.Namespace || g.Labels[PoolUIDLabel] != string(p.UID) || g.Labels[InstallationLabel] != installation {
		return fmt.Errorf("refusing foreign Gateway %s", g.Name)
	}
	o := metav1.GetControllerOf(g)
	if o == nil || o.UID != p.UID || o.Name != p.Name || o.Kind != "GatewayPool" || o.APIVersion != api.GroupVersion.String() {
		return fmt.Errorf("Gateway owner UID differs: %s", g.Name)
	}
	if expected := p.Status.Gateways[g.Name]; expected != "" && expected != string(g.UID) {
		return fmt.Errorf("Gateway UID changed: %s", g.Name)
	}
	return nil
}

// Adding ledger-backed listeners and removing exact retired listeners are
// repairable. Never overwrite retargeting or an unowned listener.
func compatibleGateway(actual, expected *gw.Gateway, retired ...gw.Listener) bool {
	if actual.Annotations[WorkersAnnotation] != expected.Annotations[WorkersAnnotation] {
		return false
	}
	a, e := normalizedGateway(actual), normalizedGateway(expected)
	a.Listeners, e.Listeners = nil, nil
	if !reflect.DeepEqual(a, e) {
		return false
	}
	want := map[gw.SectionName]gw.Listener{}
	for _, l := range normalizedGateway(expected).Listeners {
		want[l.Name] = l
	}
	for _, l := range retired {
		want[l.Name] = l
	}
	for _, l := range normalizedGateway(actual).Listeners {
		if w, ok := want[l.Name]; !ok || !reflect.DeepEqual(l, w) {
			return false
		}
	}
	return true
}

func retiredGatewayListeners(p *api.GatewayPool, name, installation string) []gw.Listener {
	// Generate the exact allowed old form without relaxing any listener fields.
	copy := p.DeepCopyObject().(*api.GatewayPool)
	retiredNames := map[string]bool{}
	for uid, a := range copy.Status.Allocations {
		if a.GatewayName == name && a.Tombstone {
			retiredNames[a.ListenerName] = true
			a.Tombstone = false
			copy.Status.Allocations[uid] = a
		}
	}
	var result []gw.Listener
	for _, l := range desiredGateway(copy, name, installation).Spec.Listeners {
		if retiredNames[string(l.Name)] {
			result = append(result, l)
		}
	}
	return result
}

// Normalize only documented no-op/default forms. A broadened namespace scope,
// added policy, requested address or protocol still fails closed as spec drift.
func normalizedGateway(g *gw.Gateway) gw.GatewaySpec {
	s := g.DeepCopy().Spec
	if s.DefaultScope == gw.GatewayDefaultScopeNone {
		s.DefaultScope = ""
	}
	if a := s.AllowedListeners; a != nil {
		if a.Namespaces == nil || (a.Namespaces.Selector == nil && (a.Namespaces.From == nil || *a.Namespaces.From == gw.NamespacesFromNone)) {
			s.AllowedListeners = nil
		}
	}
	if len(s.Addresses) == 0 {
		s.Addresses = nil
	}
	for i := range s.Listeners {
		a := s.Listeners[i].AllowedRoutes
		if a == nil {
			continue
		}
		if a.Namespaces == nil {
			same := gw.NamespacesFromSame
			a.Namespaces = &gw.RouteNamespaces{From: &same}
		}
		if a.Namespaces.From == nil {
			same := gw.NamespacesFromSame
			a.Namespaces.From = &same
		}
		for j := range a.Kinds {
			if a.Kinds[j].Group == nil {
				group := gw.Group(gw.GroupName)
				a.Kinds[j].Group = &group
			}
		}
	}
	return s
}

func (r *Reconciler) save(ctx context.Context, p *api.GatewayPool, rejected map[string]string) error {
	p.Status.RejectedRoutes = rejected
	p.Status.Used = len(p.Status.Allocations)
	p.Status.Available = p.Spec.Occupancy*p.Spec.MaxGateways - p.Status.Used
	status, reason, message := metav1.ConditionTrue, "Allocated", "Durable leases reconciled; Gateway adapter reports data-plane programming separately"
	if len(rejected) > 0 {
		status, reason, message = metav1.ConditionFalse, "RoutesRejected", fmt.Sprintf("%d route(s) rejected; see rejectedRoutes keyed by UID", len(rejected))
	}
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: "Allocated", Status: status, Reason: reason, Message: message, ObservedGeneration: p.Generation})
	return r.Status().Update(ctx, p)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	again := ctrl.Result{RequeueAfter: 3 * time.Second}
	if req.Namespace != r.Namespace {
		return ctrl.Result{}, nil
	}
	p := &api.GatewayPool{}
	if err := r.Reader.Get(ctx, req.NamespacedName, p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if err := Validate(p.Spec); err != nil {
		return again, err
	}
	if r.Installation == "" || r.ClassName == "" || p.Spec.GatewayClassName != r.ClassName {
		return again, fmt.Errorf("GatewayPool class is outside configured allocator scope")
	}
	if p.Status.FrozenSpec != nil && !reflect.DeepEqual(*p.Status.FrozenSpec, p.Spec) {
		return again, fmt.Errorf("GatewayPool spec immutable after first reconcile")
	}
	if !p.DeletionTimestamp.IsZero() {
		return r.cleanup(ctx, p)
	}
	if !controllerutil.ContainsFinalizer(p, Finalizer) {
		controllerutil.AddFinalizer(p, Finalizer)
		return again, r.Update(ctx, p)
	}
	if p.Status.FrozenSpec == nil {
		spec := p.Spec
		p.Status.FrozenSpec = &spec
		return again, r.save(ctx, p, nil)
	}
	var routes gw.UDPRouteList
	if err := r.Reader.List(ctx, &routes, client.InNamespace(r.Namespace)); err != nil {
		return again, err
	}
	var otherPools api.GatewayPoolList
	if err := r.Reader.List(ctx, &otherPools, client.InNamespace(r.Namespace)); err != nil {
		return again, err
	}
	sort.Slice(routes.Items, func(i, j int) bool { return routes.Items[i].Name < routes.Items[j].Name })
	byUID := map[string]*gw.UDPRoute{}
	for i := range routes.Items {
		byUID[string(routes.Items[i].UID)] = &routes.Items[i]
	}
	for uid, a := range p.Status.Allocations {
		if byUID[uid] == nil && !a.Tombstone {
			a.Tombstone = true
			p.Status.Allocations[uid] = a
			return again, r.save(ctx, p, p.Status.RejectedRoutes)
		}
	}
	rejected := map[string]string{}
	for i := range routes.Items {
		route := &routes.Items[i]
		a, allocated := p.Status.Allocations[string(route.UID)]
		if route.Annotations[PoolAnnotation] != p.Name && !allocated {
			continue
		}
		if !route.DeletionTimestamp.IsZero() {
			continue
		}
		problem := ""
		if route.Annotations[PoolAnnotation] != p.Name {
			problem = "allocated route pool annotation changed"
		}
		if allocated && (a.Tombstone || a.RouteName != route.Name) {
			problem = "retired or mismatched route allocation"
		}
		if allocated && a.Attached && len(route.Spec.ParentRefs) == 0 {
			problem = "allocated parentRefs removed; refusing silent reattachment"
		}
		if !allocated {
			for _, other := range otherPools.Items {
				if other.UID == p.UID {
					continue
				}
				if _, exists := other.Status.Allocations[string(route.UID)]; exists {
					problem = "route UID already leased by another GatewayPool"
					break
				}
			}
		}
		if len(route.Spec.ParentRefs) > 0 && (!allocated || !matchesParent(route, a)) {
			problem = "parentRefs conflict with allocator lease; refusing retarget"
		}
		if problem != "" {
			rejected[string(route.UID)] = problem
			continue
		}
		svc, port, health, err := r.routeService(ctx, route)
		if err != nil {
			var validation *validationError
			if !apierrors.IsNotFound(err) && !errors.As(err, &validation) {
				return again, err
			}
			rejected[string(route.UID)] = err.Error()
			continue
		}
		if allocated {
			if a.ServiceName != svc.Name || a.ServiceUID != string(svc.UID) || a.ServicePort != port || a.HealthPort != health || leaseMode(a) != routeMode(route) {
				rejected[string(route.UID)] = "allocated Service UID/port or health port changed; refusing retarget"
			}
			if rejected[string(route.UID)] == "" && matchesParent(route, a) && !a.Attached {
				a.Attached = true
				p.Status.Allocations[string(route.UID)] = a
				return again, r.save(ctx, p, rejected)
			}
			continue
		}
		if _, err = reserve(p, route, svc, port, health); err != nil {
			rejected[string(route.UID)] = err.Error()
			continue
		}
		return again, r.save(ctx, p, rejected) // durable BEFORE any Gateway or ParentRef mutation
	}
	// One mutation per reconciliation keeps retries and ambiguous replies simple.
	names := map[string]bool{}
	for _, a := range p.Status.Allocations {
		names[a.GatewayName] = true
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		want := desiredGateway(p, name, r.Installation)
		actual := &gw.Gateway{}
		err := r.Reader.Get(ctx, client.ObjectKeyFromObject(want), actual)
		if apierrors.IsNotFound(err) {
			if p.Status.Gateways[name] != "" {
				return again, fmt.Errorf("recorded Gateway disappeared; refusing silent VIP replacement: %s", name)
			}
			return again, r.Create(ctx, want)
		}
		if err != nil {
			return again, err
		}
		if err = ownedGateway(p, actual, r.Installation); err != nil {
			return again, err
		}
		if !actual.DeletionTimestamp.IsZero() {
			return again, fmt.Errorf("generated Gateway is terminating: %s", name)
		}
		if !compatibleGateway(actual, want, retiredGatewayListeners(p, name, r.Installation)...) {
			return again, fmt.Errorf("generated Gateway spec was retargeted: %s", name)
		}
		if p.Status.Gateways[name] == "" {
			if actual.UID == "" {
				return again, fmt.Errorf("generated Gateway has no server UID")
			}
			if p.Status.Gateways == nil {
				p.Status.Gateways = map[string]string{}
			}
			p.Status.Gateways[name] = string(actual.UID)
			return again, r.save(ctx, p, rejected)
		}
		if !reflect.DeepEqual(normalizedGateway(actual).Listeners, normalizedGateway(want).Listeners) {
			actual.Spec.Listeners = want.Spec.Listeners
			return again, r.Update(ctx, actual)
		}
	}
	for _, route := range byUID {
		a, ok := p.Status.Allocations[string(route.UID)]
		if !ok || a.Tombstone || !route.DeletionTimestamp.IsZero() || rejected[string(route.UID)] != "" {
			continue
		}
		if len(route.Spec.ParentRefs) == 0 {
			section := gw.SectionName(a.ListenerName)
			route.Spec.ParentRefs = []gw.ParentReference{{Name: gw.ObjectName(a.GatewayName), SectionName: &section}}
			controllerutil.AddFinalizer(route, RouteFinalizer)
			return again, r.Update(ctx, route) // resourceVersion protects concurrent spec edits
		}
	}
	before := p.DeepCopyObject().(*api.GatewayPool)
	// Avoid writing an unchanged status every polling cycle.
	p.Status.RejectedRoutes = rejected
	if reflect.DeepEqual(before.Status.RejectedRoutes, rejected) || (len(before.Status.RejectedRoutes) == 0 && len(rejected) == 0) {
		return again, nil
	}
	return again, r.save(ctx, p, rejected)
}

func (r *Reconciler) cleanup(ctx context.Context, p *api.GatewayPool) (ctrl.Result, error) {
	again := ctrl.Result{RequeueAfter: 3 * time.Second}
	if !controllerutil.ContainsFinalizer(p, Finalizer) {
		return ctrl.Result{}, nil
	}
	// The immutable archive deliberately has no ownerReference and survives pool
	// deletion. Native NLB retention/cleanup remains the adapter/engine's policy.
	h := sha256.Sum256([]byte(p.UID))
	data, err := json.Marshal(struct {
		UID    types.UID             `json:"uid"`
		Spec   api.GatewayPoolSpec   `json:"spec"`
		Status api.GatewayPoolStatus `json:"status"`
	}{p.UID, p.Spec, p.Status})
	if err != nil {
		return again, err
	}
	immutable := true
	archive := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("gateway-pool-ledger-%x", h[:12]), Namespace: p.Namespace, Labels: map[string]string{PoolUIDLabel: string(p.UID), InstallationLabel: r.Installation}}, Immutable: &immutable, Data: map[string]string{"ledger.json": string(data)}}
	existing := &core.ConfigMap{}
	err = r.Reader.Get(ctx, client.ObjectKeyFromObject(archive), existing)
	if apierrors.IsNotFound(err) {
		return again, r.Create(ctx, archive)
	}
	if err != nil {
		return again, err
	}
	if existing.Labels[PoolUIDLabel] != string(p.UID) || existing.Labels[InstallationLabel] != r.Installation || len(existing.OwnerReferences) != 0 || !reflect.DeepEqual(existing.Data, archive.Data) || existing.Immutable == nil || !*existing.Immutable {
		return again, fmt.Errorf("ledger archive conflict; refusing cleanup")
	}
	names := map[string]bool{}
	for name := range p.Status.Gateways {
		names[name] = true
	}
	for _, a := range p.Status.Allocations {
		names[a.GatewayName] = true
	}
	for name := range names {
		g := &gw.Gateway{}
		err = r.Reader.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: name}, g)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return again, err
		}
		if err = ownedGateway(p, g, r.Installation); err != nil {
			return again, err
		}
		if !g.DeletionTimestamp.IsZero() {
			return again, nil
		}
		uid, version := g.UID, g.ResourceVersion
		return again, r.Delete(ctx, g, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}})
	}
	controllerutil.RemoveFinalizer(p, Finalizer)
	return ctrl.Result{}, r.Update(ctx, p)
}
