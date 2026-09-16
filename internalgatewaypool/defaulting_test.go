package internalgatewaypool

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	gw "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"
)

// Exercise the real apiextensions pruning/defaulting functions against the
// pinned upstream standard CRD, then JSON decode exactly as a server GET does.
// This is local schema coverage, not a substitute for the live canary roundtrip.
func TestPinnedGatewayCRDDefaultingRoundTrip(t *testing.T) {
	cache, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(strings.TrimSpace(string(cache)), "sigs.k8s.io", "gateway-api@v1.6.2", "config", "crd", "standard", "gateway.networking.k8s.io_gateways.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	crd := &extv1.CustomResourceDefinition{}
	if err = yaml.Unmarshal(raw, crd); err != nil {
		t.Fatal(err)
	}
	var schema *structuralschema.Structural
	for _, version := range crd.Spec.Versions {
		if version.Name == "v1" {
			internal := &ext.JSONSchemaProps{}
			if err = extv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(version.Schema.OpenAPIV3Schema, internal, nil); err != nil {
				t.Fatal(err)
			}
			schema, err = structuralschema.NewStructural(internal)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if schema == nil {
		t.Fatal("pinned standard Gateway v1 schema missing")
	}
	f := newFixture(t, 1)
	f.steps(3)
	p := f.pool()
	a := p.Status.Allocations["route-t0"]
	expected := desiredGateway(p, a.GatewayName, "test-install")
	for _, explicitDefaults := range []bool{false, true} {
		input := expected.DeepCopy()
		if explicitDefaults {
			input.Spec.AllowedListeners = &gw.AllowedListeners{}
			input.Spec.DefaultScope = gw.GatewayDefaultScopeNone
			input.Spec.Listeners[0].AllowedRoutes.Namespaces.From = nil
			input.Spec.Listeners[0].AllowedRoutes.Kinds[0].Group = nil
		}
		data, _ := json.Marshal(input)
		var unstructured map[string]any
		_ = json.Unmarshal(data, &unstructured)
		pruning.Prune(unstructured, schema, true)
		defaulting.Default(unstructured, schema)
		data, _ = json.Marshal(unstructured)
		actual := &gw.Gateway{}
		if err = json.Unmarshal(data, actual); err != nil {
			t.Fatal(err)
		}
		if !compatibleGateway(actual, expected) {
			t.Fatalf("API defaults treated as retarget: actual=%#v expected=%#v", actual.Spec, expected.Spec)
		}
		all := gw.NamespacesFromAll
		actual.Spec.AllowedListeners = &gw.AllowedListeners{Namespaces: &gw.ListenerNamespaces{From: &all}}
		if compatibleGateway(actual, expected) {
			t.Fatal("broad ListenerSet attachment normalized away")
		}
	}
}

func TestStableReconcileDoesNotWritePoolStatus(t *testing.T) {
	f := newFixture(t, 1)
	f.steps(20)
	version := f.pool().ResourceVersion
	f.steps(5)
	if f.pool().ResourceVersion != version {
		t.Fatal("stable polling rewrites unchanged status")
	}
}

func TestDefaultGatewaySelectionConsumesNoLease(t *testing.T) {
	f := newFixture(t, 1)
	route := f.route("t0")
	route.Spec.UseDefaultGateways = gw.GatewayDefaultScopeAll
	if err := f.c.Update(t.Context(), route); err != nil {
		t.Fatal(err)
	}
	f.steps(6)
	if len(f.pool().Status.Allocations) != 0 || f.pool().Status.RejectedRoutes["route-t0"] == "" {
		t.Fatal("implicit default Gateway selection accepted")
	}
}
