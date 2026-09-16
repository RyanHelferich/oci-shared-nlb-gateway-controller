# Examples

| File | Use |
| --- | --- |
| `overlay-nodeport.yaml` | Recommended canary for Cilium or another overlay network |
| `vcn-native-podip.yaml` | Direct pod backend canary when pod addresses are routable from the NLB subnet |
| `multi-port-service.yaml` | Several UDPRoutes selecting distinct ports on one Service |
| `manual-fixed-ports.yaml` | Platform-managed fixed Gateway listener ports |

Copy an example and replace every `YOUR_` or `REPLACE_` value. Run a server-side dry run before applying it. These examples do not create OCI IAM, VCN routes, security rules, workload keys, or application Deployments.
