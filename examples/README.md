# Examples

| File | Use |
| --- | --- |
| `nodeport-local.yaml` | Preferred pilot canary for Cilium overlay and VCN-native clusters: one ready pod's worker and a workload TCP health NodePort (`NodePortLocal`) |
| `vcn-native-podip.yaml` | Direct pod backend canary when pod addresses are proven routable from the NLB subnet (`PodIP`) |
| `overlay-nodeport.yaml` | All-worker compatibility canary (`NodePortCluster`); worker health is not workload health, see the recorded EndpointSlice failure in the Cilium test report |
| `multi-port-service.yaml` | Several UDPRoutes selecting distinct ports on one Service |
| `manual-fixed-ports.yaml` | Platform-managed fixed Gateway listener ports |
| `dual-tunnel-ha.yaml` | Two tunnels behind two routes for active/standby HA; see `docs/dual-tunnel-ha.md` |

Route annotations (`nlb.independent.dev/pool`, `backend-mode`, `health-port`) and the Gateway `max-workers` annotation are documented in [docs/annotations.md](../docs/annotations.md).

Copy an example and replace every `YOUR_` or `REPLACE_` value. Run a server-side dry run before applying it. These examples do not create OCI IAM, VCN routes, security rules, workload keys, or application Deployments.
