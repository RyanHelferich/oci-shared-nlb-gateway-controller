package main

import (
	"context"
	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	engine "github.com/RyanHelferich/oci-shared-nlb-gateway-controller/internalcontroller"
	gateway "github.com/RyanHelferich/oci-shared-nlb-gateway-controller/internalgateway"
	gatewaypool "github.com/RyanHelferich/oci-shared-nlb-gateway-controller/internalgatewaypool"
	"flag"
	"fmt"
	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/core"
	n "github.com/oracle/oci-go-sdk/v65/networkloadbalancer"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var version = "0.1.0-dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	var namespace, region, compartment, subnet, installation, authentication, checkNLB string
	var gatewayClass string
	var enableGatewayAPI bool
	flag.BoolVar(&enableGatewayAPI, "enable-gateway-api", false, "Enable the native UDP Gateway API and automatic GatewayPool allocation")
	flag.StringVar(&gatewayClass, "gateway-class", "oci-native-udp", "Exact GatewayClass managed by this installation")
	flag.StringVar(&checkNLB, "check-access-nlb", "", "Read-only diagnostic against an existing NLB; exits without starting reconciliation")
	flag.StringVar(&namespace, "namespace", "shared-nlb", "Single managed namespace")
	flag.StringVar(&region, "region", "us-ashburn-1", "OCI region")
	flag.StringVar(&compartment, "compartment", "", "Only authorized OCI compartment")
	flag.StringVar(&subnet, "subnet", "", "Only authorized NLB subnet")
	flag.StringVar(&installation, "installation-id", "", "Stable unique installation ID; never change on restart")
	flag.StringVar(&authentication, "auth", "workload", "workload, instance, or config (local evaluation)")
	flag.Parse()
	if compartment == "" || subnet == "" || installation == "" {
		return fmt.Errorf("compartment, subnet and installation-id are required")
	}
	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))
	var provider common.ConfigurationProvider
	var err error
	switch authentication {
	case "workload":
		provider, err = auth.OkeWorkloadIdentityConfigurationProvider()
	case "instance":
		provider, err = auth.InstancePrincipalConfigurationProvider()
	case "config":
		provider = common.DefaultConfigProvider()
	default:
		return fmt.Errorf("unsupported authentication mode")
	}
	if err != nil {
		return err
	}
	cloud, err := n.NewNetworkLoadBalancerClientWithConfigurationProvider(provider)
	if err != nil {
		return err
	}
	cloud.SetRegion(region)
	if checkNLB != "" {
		ctx := context.Background()
		netClient, e := core.NewVirtualNetworkClientWithConfigurationProvider(provider)
		if e != nil {
			return e
		}
		netClient.SetRegion(region)
		sub, e := netClient.GetSubnet(ctx, core.GetSubnetRequest{SubnetId: &subnet})
		fmt.Printf("GetSubnet: %v\n", e)
		if e == nil {
			_, e = netClient.GetVcn(ctx, core.GetVcnRequest{VcnId: sub.VcnId})
			fmt.Printf("GetVcn: %v\n", e)
		}
		_, e = cloud.GetNetworkLoadBalancer(ctx, n.GetNetworkLoadBalancerRequest{NetworkLoadBalancerId: &checkNLB})
		fmt.Printf("GetNetworkLoadBalancer: %v\n", e)
		return e
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	if enableGatewayAPI {
		if gatewayClass == "" {
			return fmt.Errorf("gateway-class is required when Gateway API is enabled")
		}
		if err = gatewayv1.Install(scheme); err != nil {
			return err
		}
		if err = api.AddGatewayPoolToScheme(scheme); err != nil {
			return err
		}
	}
	manager, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: scheme, Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}}, LeaderElection: true, LeaderElectionNamespace: namespace, LeaderElectionID: "shared-nlb-controller", LeaderElectionReleaseOnCancel: false, HealthProbeBindAddress: ":8081", Metrics: metricsserver.Options{BindAddress: ":8080"}})
	if err != nil {
		return err
	}
	r := &engine.Reconciler{Client: manager.GetClient(), Reader: manager.GetAPIReader(), Cloud: &engine.Cloud{Client: cloud, Installation: installation}, Recorder: manager.GetEventRecorderFor("shared-nlb-controller"), Namespace: namespace, Compartment: compartment, Subnet: subnet}
	if err = r.Setup(manager); err != nil {
		return err
	}
	if enableGatewayAPI {
		g := &gateway.Reconciler{Client: manager.GetClient(), Reader: manager.GetAPIReader(), Namespace: namespace, GatewayClassName: gatewayClass, ControllerName: "nlb.independent.dev/native-udp", Installation: installation, Compartment: compartment, Subnet: subnet}
		if err = g.Setup(manager); err != nil {
			return err
		}
		p := &gatewaypool.Reconciler{Client: manager.GetClient(), Reader: manager.GetAPIReader(), Namespace: namespace, Installation: installation, ClassName: gatewayClass}
		if err = p.Setup(manager); err != nil {
			return err
		}
	}
	if err = manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err = manager.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	ctrl.Log.Info("starting independently developed shared NLB controller", "version", version, "namespace", namespace, "region", region)
	return manager.Start(ctrl.SetupSignalHandler())
}
