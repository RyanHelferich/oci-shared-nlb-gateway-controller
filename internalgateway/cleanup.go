package internalgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	core "k8s.io/api/core/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gateway "sigs.k8s.io/gateway-api/apis/v1"
)

// Cleanup is driven even without a Gateway event, including route deletion after
// a forced parent removal. Normal binding finalizers remain the only cloud path.
func (r *Reconciler) cleanupRoutes(ctx context.Context, routes []gateway.UDPRoute, bindings []api.TunnelBinding, pools []api.NLBPool) (bool, error) {
	byUID := map[types.UID]*gateway.UDPRoute{}
	for i := range routes {
		byUID[routes[i].UID] = &routes[i]
	}
	for i := range bindings {
		b := &bindings[i]
		if b.Labels[InstallationLabel] != r.Installation || b.Labels[RouteUIDLabel] == "" {
			continue
		}
		if !r.ownsBinding(b) {
			return false, fmt.Errorf("binding with adapter ownership labels failed exact ownership validation")
		}
		route := byUID[types.UID(b.Labels[RouteUIDLabel])]
		attached := false
		if route != nil && route.DeletionTimestamp.IsZero() {
			for _, parent := range route.Spec.ParentRefs {
				for _, owner := range b.OwnerReferences {
					if parentMatches(parent, &gateway.Gateway{ObjectMeta: metav1.ObjectMeta{Name: owner.Name}}, route.Namespace) {
						attached = true
					}
				}
			}
		}
		if !attached && b.DeletionTimestamp.IsZero() {
			return true, r.deleteExact(ctx, b)
		}
	}
	for i := range routes {
		route := &routes[i]
		if !controllerutil.ContainsFinalizer(route, RouteFinalizer) {
			continue
		}
		// Detached live routes also release our finalizer after child cleanup; the
		// allocator can subsequently attach them, subject to existing port leases.
		needsCleanup := !route.DeletionTimestamp.IsZero() || len(route.Spec.ParentRefs) == 0
		if !needsCleanup {
			continue
		}
		pending := false
		for _, b := range bindings {
			if b.Labels[RouteUIDLabel] == string(route.UID) && r.ownsBinding(&b) {
				pending = true
			}
		}
		for _, p := range pools {
			gUID := types.UID(p.Labels[GatewayUIDLabel])
			if !r.owns(&p, gUID) || p.Name != PoolName(gUID) {
				continue
			}
			for _, a := range p.Status.Allocations {
				if a.BindingName == BindingName(gUID, route.UID) && (!a.Tombstone || p.Status.PendingWork != "") {
					pending = true
				}
			}
		}
		if pending {
			continue
		}
		controllerutil.RemoveFinalizer(route, RouteFinalizer)
		return true, r.Update(ctx, route)
	}
	return false, nil
}

func (r *Reconciler) retireGateway(ctx context.Context, g *gateway.Gateway, bindings []api.TunnelBinding) error {
	if !controllerutil.ContainsFinalizer(g, GatewayFinalizer) {
		return nil
	}
	for i := range bindings {
		b := &bindings[i]
		if b.Spec.Pool != PoolName(g.UID) && b.Labels[GatewayUIDLabel] != string(g.UID) {
			continue
		}
		if !r.ownsBinding(b) || b.Labels[GatewayUIDLabel] != string(g.UID) {
			return fmt.Errorf("foreign binding references retiring Gateway pool")
		}
		if b.DeletionTimestamp.IsZero() {
			return r.deleteExact(ctx, b)
		}
		return nil // Core must finish native deletion and persist its tombstone first.
	}
	p := &api.NLBPool{}
	err := r.Reader.Get(ctx, types.NamespacedName{Namespace: g.Namespace, Name: PoolName(g.UID)}, p)
	if errors.IsNotFound(err) {
		if g.Annotations[poolUIDAnnotation] != "" {
			archive := &core.ConfigMap{}
			if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: g.Namespace, Name: "retired-" + string(g.UID)}, archive); err != nil {
				return fmt.Errorf("pool absent without retained archive: %w", err)
			}
			archived := &api.NLBPool{}
			if archive.Labels[GatewayUIDLabel] != string(g.UID) || archive.Labels[InstallationLabel] != r.Installation || len(archive.OwnerReferences) != 0 || archive.Immutable == nil || !*archive.Immutable || json.Unmarshal([]byte(archive.Data["pool.json"]), archived) != nil || string(archived.UID) != g.Annotations[poolUIDAnnotation] || !r.owns(archived, g.UID) || archived.Status.PendingWork != "" {
				return fmt.Errorf("retired pool archive ownership mismatch")
			}
			for _, a := range archived.Status.Allocations {
				if !a.Tombstone {
					return fmt.Errorf("retired archive contains active lease")
				}
			}
		}
		controllerutil.RemoveFinalizer(g, GatewayFinalizer)
		return r.Update(ctx, g)
	}
	if err != nil {
		return err
	}
	if !r.owns(p, g.UID) || (g.Annotations[poolUIDAnnotation] != "" && g.Annotations[poolUIDAnnotation] != string(p.UID)) {
		return fmt.Errorf("refusing retirement of foreign pool")
	}
	if p.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(p, PoolArchiveFinalizer) {
		controllerutil.AddFinalizer(p, PoolArchiveFinalizer)
		return r.Update(ctx, p)
	}
	// When GC reaches the pool before the Gateway, core must first verify that
	// native listeners/backends are gone and finish its pending work. The archive
	// guard keeps the object alive after core removes its own finalizer.
	if !p.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(p, api.Finalizer) {
		return nil
	}
	if p.Status.PendingWork != "" {
		return nil
	}
	for _, a := range p.Status.Allocations {
		if !a.Tombstone {
			return nil
		}
	}
	// The archive outlives the Gateway; deliberately omit ownerReferences.
	archivalCopy := p.DeepCopyObject().(*api.NLBPool)
	archivalCopy.APIVersion = api.GroupVersion.String()
	archivalCopy.Kind = "NLBPool"
	raw, err := json.Marshal(archivalCopy)
	if err != nil {
		return err
	}
	archive := &core.ConfigMap{}
	key := types.NamespacedName{Namespace: g.Namespace, Name: "retired-" + string(g.UID)}
	err = r.Reader.Get(ctx, key, archive)
	if errors.IsNotFound(err) {
		immutable := true
		archive = &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: r.labels(g.UID, "")}, Immutable: &immutable, Data: map[string]string{"pool.json": string(raw)}}
		if err := r.Create(ctx, archive); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		prior := &api.NLBPool{}
		if archive.Labels[GatewayUIDLabel] != string(g.UID) || archive.Labels[InstallationLabel] != r.Installation || len(archive.OwnerReferences) != 0 || archive.Immutable == nil || !*archive.Immutable || json.Unmarshal([]byte(archive.Data["pool.json"]), prior) != nil || prior.UID != p.UID || !reflect.DeepEqual(prior.Spec, p.Spec) || !reflect.DeepEqual(prior.Status.Allocations, p.Status.Allocations) || !reflect.DeepEqual(prior.Status.Shards, p.Status.Shards) || prior.Status.PendingWork != "" {
			return fmt.Errorf("retired archive collision or ledger changed; refusing overwrite")
		}
	}
	if p.DeletionTimestamp.IsZero() {
		return r.deleteExact(ctx, p)
	}
	if controllerutil.ContainsFinalizer(p, PoolArchiveFinalizer) {
		controllerutil.RemoveFinalizer(p, PoolArchiveFinalizer)
		return r.Update(ctx, p)
	}
	return nil // Normal core pool finalizer retains its empty NLB by policy.
}
