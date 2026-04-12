# agentfab on kind

End-to-end test fabric: `etcd`, a control plane, two node hosts, and an
interactive conductor pod, all running inside a local kind cluster from a
single image built out of `deploy/Dockerfile`.

## What you get

```
kind cluster: agentfab-test
└── namespace: agentfab
    ├── Deployment/etcd              (1 pod, control-plane metadata)
    ├── Deployment/control-plane     (1 pod, agentfab control-plane serve)
    │     └── Service/control-plane  (ClusterIP, gRPC :50051)
    ├── Deployment/agentfab-node     (2 pods, agentfab node serve)
    └── Pod/conductor                (sleep infinity; kubectl exec to use)
```

The same `agentfab:dev` image runs every workload — the role is selected
purely by the pod's `args`.

`up.sh` renders the kind cluster config before creation so every kind node
mounts the same host directory at `/var/lib/agentfab-host`. The runtime uses
that host-backed path for the shared and agent storage tiers, so artifacts,
knowledge, and logs are visible across pods and survive pod restarts while the
cluster is up.

## Prerequisites

- `docker`
- `kind` (v0.20+)
- `kubectl`
- `openssl` (any modern build; LibreSSL on macOS works)
- An API key for at least one LLM provider supported by your fabric

## One-time setup

```sh
cp deploy/kind/secrets/llm.env.example deploy/kind/secrets/llm.env
$EDITOR deploy/kind/secrets/llm.env   # fill in at least one provider key
```

`secrets/llm.env` is gitignored.

## Bring it up

```sh
./deploy/kind/up.sh
```

This script:

1. Creates the kind cluster (`agentfab-test`) if it doesn't exist.
2. Builds `agentfab:dev` from `deploy/Dockerfile` and loads it into kind.
3. Creates a host-backed storage root under `deploy/kind/state/<cluster>/`.
4. Generates a long-lived shared CA with `openssl` and stores it in the
   `agentfab-cluster-ca` Secret. Every pod's `seed-ca` initContainer
   copies this into `/var/lib/agentfab/shared/identity/local-dev/` so the
   local-dev identity provider loads it instead of generating a fresh CA
   per pod.
5. Creates the `agentfab-llm` Secret from `secrets/llm.env`.
6. Applies the fabric ConfigMap, `etcd`, and the control-plane Deployment+Service.
7. Waits for the control plane to be Ready, then exec's into it to mint a
   reusable node enrollment token via `agentfab node token create`. Stores
   that token in the `agentfab-node-token` Secret.
8. Applies the node Deployment (which mounts the token Secret) and the
   conductor Pod.

After a successful run you should see two node pods registered with the
control plane.

## Talk to the fabric

Open an interactive conductor session inside the cluster:

```sh
kubectl exec -it -n agentfab pod/conductor -- \
    agentfab run \
        --config /etc/agentfab/agents.yaml \
        --data-dir /var/lib/agentfab \
        --external-nodes \
        --skip-verify
```

`--external-nodes` tells the conductor not to spawn its own local node hosts;
instead it dispatches tasks to the agent instances registered by the
in-cluster `agentfab-node` Deployment. The control-plane address is read
from `control_plane.api.address` in the mounted ConfigMap, which points at
`control-plane.agentfab.svc.cluster.local:50051`.

The conductor pod exports its Pod IP as `AGENTFAB_ADVERTISE_HOST`, so an
interactive in-cluster `agentfab run` session advertises a reachable address
to the rest of the fabric instead of falling back to `127.0.0.1`.

`--skip-verify` skips signed-bundle verification — the kind ConfigMap ships
a minimal `agents.yaml` without a signed bundle, so verification would fail
otherwise. For a production deployment you'd ship a signed bundle alongside
the ConfigMap and drop this flag.

Each `kubectl exec` is a fresh interactive session against the same fabric.
Detach with `Ctrl-D` (or however the TUI handles exit); the pod stays up,
ready for the next session.

## Validate the fabric

Run the built-in validator from your laptop:

```sh
./deploy/kind/validate.sh check
./deploy/kind/validate.sh smoke
./deploy/kind/validate.sh failover
```

What each mode does:

- `check`: confirms `etcd`, the control plane, the node Deployment, and the
  conductor pod are ready.
- `smoke`: drives one non-TTY `agentfab run` session inside the conductor pod,
  submits a real request, and verifies the request produced persisted results.
- `failover`: submits a real request, deletes the first assigned node pod after
  task placement, and verifies the request still completes.

Validation output is written under `deploy/kind/state/<cluster>/validation/`.

## Inspect things

```sh
kubectl get pods -n agentfab
kubectl logs  -n agentfab deploy/etcd
kubectl logs  -n agentfab deploy/control-plane
kubectl logs  -n agentfab deploy/agentfab-node --all-pods
kubectl exec  -n agentfab deploy/control-plane -- \
    ls -lR /var/lib/agentfab
```

Storage layout in this test fabric:

- control-plane metadata lives in `etcd`
- shared artifacts, logs, and knowledge live under
  `/var/lib/agentfab-host/fabric`
- control-plane local files such as reusable node-token state live under
  `/var/lib/agentfab-host/runtime/control-plane`

## Manual resilience drills

The validator covers happy-path execution and node failover. Keep the other two
restart drills manual for now:

1. Control-plane restart
   ```sh
   kubectl rollout restart -n agentfab deployment/control-plane
   kubectl rollout status -n agentfab deployment/control-plane
   ./deploy/kind/validate.sh check
   ```
2. Conductor restart
   ```sh
   kubectl delete pod -n agentfab conductor
   kubectl apply -f deploy/kind/manifests/40-conductor.yaml
   kubectl wait -n agentfab --for=condition=Ready pod/conductor --timeout=120s
   ./deploy/kind/validate.sh smoke
   ```

## Tear down

```sh
./deploy/kind/down.sh
```

Deletes the kind cluster and removes `deploy/kind/state/<cluster>`. The image
stays in your local Docker daemon.

## Customising

- **Different namespace, cluster, or image tag:** override via env vars.
  ```sh
  CLUSTER_NAME=agentfab-2 NAMESPACE=foo IMAGE_NAME=agentfab:1.2.3 ./deploy/kind/up.sh
  ```
  Note that the in-cluster control-plane DNS name baked into
  `manifests/10-config.yaml` (`control-plane.agentfab.svc.cluster.local`)
  is hard-coded to the `agentfab` namespace. If you use a different
  namespace you also need to edit that ConfigMap so the SAN on the
  control-plane gRPC cert matches what clients dial.
- **More node replicas:** edit `replicas:` in `manifests/30-node.yaml` and
  re-apply.
- **Different agent set:** edit the inline `agents.yaml` in
  `manifests/10-config.yaml` (or replace the ConfigMap with one generated
  from your own fabric). Anything you reference under `agents_dir:` must
  exist inside the image — `/opt/agentfab/agents` ships the bundled
  defaults; mount your own ConfigMap with extra files if you want a
  different agent dir.

## Known caveats

- `--skip-verify` is currently required because the ConfigMap-supplied
  fabric definition has no signed bundle. Once signed bundles are part of
  the deployment story we'll mount one alongside the ConfigMap and drop
  the flag.
- The `agentfab-node-token` Secret is minted at bootstrap time. Re-running
  `up.sh` will mint a fresh token and roll the node Deployment over to it.
- Pod IPs change on restart; nodes re-register their `(POD_IP, port)`
  endpoints with the control plane on every start, so this is handled
  automatically.
