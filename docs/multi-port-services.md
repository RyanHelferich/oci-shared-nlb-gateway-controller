# One Service with many UDP ports

One Kubernetes Service may expose several named UDP ports, and several `UDPRoute` objects may reference different ports on that Service. The routes can share one Gateway and OCI NLB.

```text
Service udp-tunnels
  port tenant-a: 51820 -> targetPort 51820
  port tenant-b: 51821 -> targetPort 51821

UDPRoute tenant-a -> Service port 51820 -> NLB listener 20000
UDPRoute tenant-b -> Service port 51821 -> NLB listener 20001
```

Use [the example](../examples/multi-port-service.yaml) when the selected workload pod set owns every declared port.

The current example uses `NodePortLocal`. Kubernetes allocates one UDP NodePort per named tunnel port plus one TCP health NodePort. Every route registers the same selected pod worker but keeps its own NLB listener and backend set.

## Important selector rule

A Kubernetes Service has one selector for all of its ports. It does not select pod set A for port A and pod set B for port B. Therefore:

- one multi-port Service is suitable for one pod or replica set that listens on all ports;
- separate workloads with different selectors need separate Services;
- separate Services may still share the same Gateway and OCI NLB;
- a port-aware application gateway could demultiplex to separate workloads, but that gateway would be an additional data-path component and must be designed for availability and capacity.

Every route still consumes one OCI listener and backend set. Grouping ports in one Service reduces Kubernetes object count; it does not increase the NLB's 50-listener limit.
