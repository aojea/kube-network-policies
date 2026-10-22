---
title: "Troubleshooting with JSON Logging"
weight: 1
---

The controller supports structured JSON logging via klog's `--logging-format=json` flag. When combined with `-v=2`, it produces machine-readable log entries for packets denied by network policies, making it straightforward to filter and analyze traffic decisions with tools like [`jq`](https://jqlang.github.io/jq/).

## Enabling JSON logging

Pass the following flags to the controller binary (or set them in the DaemonSet args):

```
--logging-format=json -v=2
```

Verbosity levels control packet-level logging:
- At `-v=2` (recommended for production troubleshooting), the policy engine emits one self-contained log entry per **denied** packet with the packet 5-tuple, source and destination pods, traffic direction, and the policy evaluator that denied the packet. Accepted packets are not logged at `-v=2`.
- At `-v=4` (for deep debugging), the policy engine also logs accepted packets and per-evaluator / per-rule evaluation traces.
- Below `-v=2` (e.g., `-v=0`), packet-level verdict logs are not produced.

### Fine-grained logging with `-vmodule` (text format)

Because packet verdicts are logged in `engine.go` (`V(2)` for denials, `V(4)` for accepted packets) and per-rule traces live in each policy evaluator's file (`networkpolicy.go`, `clusternetworkpolicy.go`, `adminnetworkpolicy.go`, `baselineadminnetworkpolicy.go`) at `V(4)`, you can use klog's `-vmodule` flag in the default `text` log format for selective logging:

- **Log only denied packet verdicts** (keeping all other controller logs at `V(0)`):
  ```
  -v=0 -vmodule=engine=2
  ```
- **Log both accepted and denied packet verdicts without per-rule trace noise**:
  ```
  -v=2 -vmodule=engine=4
  ```
- **Log denied verdicts plus per-rule traces for a single evaluator** (e.g., standard `NetworkPolicy` only):
  ```
  -v=2 -vmodule=networkpolicy=4
  ```

> **Note:** Kubernetes `component-base/logs` only supports `-vmodule` with `--logging-format=text` (the default). In addition, `-vmodule` evaluates caller stack frames when checking verbosity thresholds higher than the global `-v` flag, so global `-v=2` is recommended for steady-state production clusters.

## Log entry structure

### Verdict entry

Emitted once per denied packet at `-v=2` (and also for accepted packets at `-v=4`) after the pipeline completes:

```json
{
  "ts": 1720777151838.4001,
  "caller": "networkpolicy/engine.go:67",
  "msg": "Packet denied by egress policy",
  "v": 2,
  "id": 9,
  "direction": "Egress",
  "evaluator": "StandardNetworkPolicy",
  "srcPod": "default/frontend",
  "dstPod": "default/backend",
  "packet": {
    "ID": 9,
    "Family": "IPv4",
    "SrcIP": "10.0.1.3",
    "DstIP": "10.0.2.2",
    "Proto": "TCP",
    "SrcPort": 40243,
    "DstPort": 8080
  }
}
```

Possible `msg` values:
- `"Packet denied by egress policy"` (emitted at `-v=2`)
- `"Packet denied by ingress policy"` (emitted at `-v=2`)
- `"Packet accepted by policy"` (emitted at `-v=4`)

Key fields:
- `id`: packet ID; links the verdict entry to per-rule trace entries when `-v=4` is enabled
- `direction`: `"Egress"` or `"Ingress"` (present on denial entries)
- `evaluator`: policy evaluator that produced the denial (e.g., `"StandardNetworkPolicy"`, `"ClusterNetworkPolicy"`, `"AdminNetworkPolicy"`, `"BaselineAdminNetworkPolicy"`)
- `srcPod` / `dstPod`: `"namespace/name"`, or `"external"` for traffic outside the cluster
- `packet`: the 5-tuple without the raw payload (payload is omitted in JSON mode)

## Text vs JSON format

In the default text format, packets are logged with a raw hex dump of the payload:

```
I0712 09:53:52.257809       1 engine.go:67] "Packet denied by egress policy" id=9 direction="Egress" evaluator="StandardNetworkPolicy" srcPod="default/frontend" dstPod="default/backend" packet=<
        [9] 10.0.1.3:40243 10.0.2.2:8080 TCP
        00000000  00 00 a0 02 fa f0 19 33  00 00 02 04 05 b4 04 02  |.......3........|
        00000010  08 0a c1 d9 c1 bd 00 00  00 00 01 03 03 07        |..............|
 >
```

In JSON mode, the payload is omitted and the packet is rendered as a structured object, making it easy to filter with `jq`.

## Troubleshooting examples with jq

The examples below assume log lines are streamed from `kubectl logs`:

```sh
kubectl logs -n kube-system -l app=kube-network-policies --follow
```

### Show all denied packets

```sh
kubectl logs ... | jq -c 'select(.msg | test("Packet denied"))'
```

### Show only egress denials

```sh
kubectl logs ... | jq -c 'select(.msg == "Packet denied by egress policy")'
```

### Show compact summary for denied connections

Each denial entry contains the full pod and packet metadata directly:

```sh
kubectl logs ... | jq -c '
  select(.msg | test("Packet denied")) |
  {id: .id, direction: .direction, evaluator: .evaluator, srcPod: .srcPod, dstPod: .dstPod, dstPort: .packet.DstPort, proto: .packet.Proto}
'
```

### Filter packets by destination port

```sh
kubectl logs ... | jq -c 'select(.packet.DstPort == 8080)'
```

### Filter packets from a specific pod

```sh
kubectl logs ... | jq -c 'select(.srcPod == "default/frontend")'
```

### Show all traffic to external destinations

```sh
kubectl logs ... | jq -c 'select(.dstPod == "external")'
```

### Count denials per direction

```sh
kubectl logs ... | jq -r 'select(.msg | test("Packet denied")) | .msg' | sort | uniq -c
```

### Summarize denied destination ports

```sh
kubectl logs ... | jq -r 'select(.packet != null) | .packet.DstPort' | sort -n | uniq -c | sort -rn
```
