package internalgatewaypool

import (
	"context"
	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	core "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestClusterFleetModeBudgetAndLease(t *testing.T) {
	f := newFixture(t, 0)
	svc, route := serviceAndRoute("one")
	route.Annotations = map[string]string{PoolAnnotation: "pool", ModeAnnotation: "NodePortCluster"}
	svc.Spec.Type, svc.Spec.ExternalTrafficPolicy = core.ServiceTypeNodePort, core.ServiceExternalTrafficPolicyCluster
	svc.Spec.Ports = svc.Spec.Ports[:1]
	svc.Spec.Ports[0].NodePort = 30001
	f.r.Reader = fake.NewClientBuilder().WithScheme(f.c.Scheme()).WithObjects(svc).Build()
	_, port, health, err := f.r.routeService(context.Background(), route)
	if err != nil || health != 10256 {
		t.Fatalf("health=%d err=%v", health, err)
	}
	f.p.Spec.MaxWorkers = 50
	f.p.Spec.Occupancy = 20
	allocation, err := reserve(f.p, route, svc, port, health)
	if err != nil || allocation.Mode != "NodePortCluster" {
		t.Fatalf("lease=%+v err=%v", allocation, err)
	}
	g := desiredGateway(f.p, allocation.GatewayName, "test")
	if g.Annotations[WorkersAnnotation] != "50" {
		t.Fatal("worker budget not propagated")
	}
	changed := g.DeepCopy()
	changed.Annotations[WorkersAnnotation] = "51"
	if compatibleGateway(changed, g) {
		t.Fatal("changed worker budget accepted")
	}
	f.p.Spec.Occupancy = 21
	if Validate(f.p.Spec) == nil {
		t.Fatal("1050 worker backends accepted")
	}
	if leaseMode(api.GatewayAllocation{}) != "PodIP" {
		t.Fatal("legacy lease mode changed")
	}
}
