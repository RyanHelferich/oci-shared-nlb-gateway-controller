package internalgateway

import (
	"context"
	core "k8s.io/api/core/v1"
	gateway "sigs.k8s.io/gateway-api/apis/v1"
	"testing"
)

func TestClusterRouteOptInAndImmutableMode(t *testing.T) {
	f := setup(t, func(f *fixture) {
		f.route.Annotations = map[string]string{BackendModeAnnotation: "NodePortCluster"}
		f.svc.Spec.Type, f.svc.Spec.ExternalTrafficPolicy = core.ServiceTypeNodePort, core.ServiceExternalTrafficPolicyCluster
		f.svc.Spec.Ports = f.svc.Spec.Ports[:1]
		f.svc.Spec.Ports[0].NodePort = 30001
	})
	f.reconcile(t)
	b := f.binding(t)
	if b.Spec.Mode != "NodePortCluster" || b.Spec.HealthPort != 10256 || b.Spec.Suspended {
		t.Fatalf("wrong generated binding %+v", b.Spec)
	}
	// A valid different mode must suspend the immutable binding, not retarget it.
	readObject(t, f.c, f.route)
	f.route.Annotations = map[string]string{HealthPortAnnotation: "9901"}
	if err := f.c.Update(context.Background(), f.route); err != nil {
		t.Fatal(err)
	}
	readObject(t, f.c, f.svc)
	f.svc.Spec.Type = core.ServiceTypeClusterIP
	f.svc.Spec.ExternalTrafficPolicy = ""
	f.svc.Spec.Ports[0].NodePort = 0
	f.svc.Spec.Ports = append(f.svc.Spec.Ports, core.ServicePort{Name: "health", Protocol: core.ProtocolTCP, Port: 9901})
	if err := f.c.Update(context.Background(), f.svc); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	b = f.binding(t)
	if !b.Spec.Suspended || b.Spec.Mode != "NodePortCluster" {
		t.Fatalf("mode retarget not suspended %+v", b.Spec)
	}
}

func TestClusterRouteRequiresExplicitContract(t *testing.T) {
	for _, scenario := range []string{"omitted-mode", "Local", "ClusterIP", "unallocated", "foreign-health", "unsupported"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t, nil)
			f.route.Annotations = map[string]string{BackendModeAnnotation: "NodePortCluster"}
			f.svc.Spec.Type, f.svc.Spec.ExternalTrafficPolicy = core.ServiceTypeNodePort, core.ServiceExternalTrafficPolicyCluster
			f.svc.Spec.Ports = f.svc.Spec.Ports[:1]
			f.svc.Spec.Ports[0].NodePort = 30001
			switch scenario {
			case "omitted-mode":
				delete(f.route.Annotations, BackendModeAnnotation)
			case "Local":
				f.svc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyLocal
			case "ClusterIP":
				f.svc.Spec.Type = core.ServiceTypeClusterIP
			case "unallocated":
				f.svc.Spec.Ports[0].NodePort = 0
			case "foreign-health":
				f.route.Annotations[HealthPortAnnotation] = "9901"
			case "unsupported":
				f.route.Annotations[BackendModeAnnotation] = "guess"
			}
			if _, err := desiredBinding(f.route, f.g, &f.g.Spec.Listeners[0], f.svc); err == nil {
				t.Fatal("unsafe contract accepted")
			}
		})
	}
}

func TestGatewayWorkerBudgetAndFrozenPool(t *testing.T) {
	f := setup(t, nil)
	f.g.Annotations[MaxWorkersAnnotation] = "50"
	workers, slots, err := gatewayWorkers(f.g)
	if err != nil || workers != 50 || slots != 20 {
		t.Fatalf("workers=%d slots=%d err=%v", workers, slots, err)
	}
	if _, err := f.r.ensurePool(context.Background(), f.g); err == nil {
		t.Fatal("existing pool changed immutable worker budget")
	}
	f.g.Spec.Listeners = make([]gateway.Listener, 21)
	if err := validateGateway(f.g); err == nil {
		t.Fatal("manual Gateway exceeded worker budget")
	}
}
