# Kubernetes operator

The operator manages organizations, teams, Cedar policies and stored configuration objects as Kubernetes resources, so a platform team can keep tenants in Git and apply them with Argo CD or Flux. It is `gogctl operator`, shipped in the gateway image, and works through the [admin API](api.md) as [`gogctl config import`](gogctl.md) does for a file. Decisions are recorded in [ADR 0034](adr/0034-control-and-data-planes.md), which refines ADR 0010.

## Installing

The [Helm chart](helm.md) installs two CRDs from its `crds/` directory and, with `operator.enabled`, runs the operator:

```yaml
operator:
  enabled: true
  adminTokenSecret: {name: genaiopengateway, key: GATEWAY_ADMIN_TOKEN}   # an admin token with the owner role
  watchNamespace: ""   # empty watches every namespace, with a ClusterRole; a namespace gets a Role there
  interval: 15s        # time between reconcile passes
```

The operator reaches the gateway at its Service. It may read the two resource kinds and Secrets, and write their finalizers and status; it needs nothing else in the cluster. Helm installs CRDs once and never upgrades them, so after an upgrade that changes them run `kubectl apply -f crds/` from the chart.

To run the operator elsewhere, such as a laptop against a test cluster, set `GOG_URL` and `GOG_ADMIN_TOKEN` and point it at `kubectl proxy`:

```sh
kubectl proxy &
gogctl operator -kube-url http://127.0.0.1:8001 -namespace gateway
```

`-once` runs one pass and exits with an error if the pass failed.

## Organizations

A `GatewayOrganization` is one organization with its policy, its teams and its Cedar policies, in the shape of an export's `orgs` section ([gogctl](gogctl.md#configuration-as-code)):

```yaml
apiVersion: genaiopengateway.io/v1
kind: GatewayOrganization
metadata: {name: acme, namespace: gateway}
spec:
  # name: Acme Corp      # default: the resource's name
  allowed_models: [gpt-5-mini, claude-sonnet]
  allowed_agents: [planner]
  rpm: 600
  budget: {limit_usd: 500, period: monthly}
  teams:
    search: {rpm: 100, allowed_mcp_servers: [github]}
    ml: {}
  cedar: |
    forbid (principal, action == Action::"invoke_model", resource)
    when { resource.residency != "eu" };
```

The resource owns its organization. Settings it leaves out are cleared, teams it leaves out are deleted, and Cedar policies are removed when `cedar` is absent. Changes made in the UI are reverted on the next pass. Keys are not part of the resource, since their secrets are shown only once; create them in the UI, with `gogctl` or with [Terraform](terraform.md).

Deleting the resource deletes the organization and its teams, along with their keys.

## Configuration objects

A `GatewayConfigObject` is one stored provider, model, guardrail, MCP server, virtual MCP server, alert webhook, stream or A2A agent, with `spec` as in the [config file](config.md):

```yaml
apiVersion: genaiopengateway.io/v1
kind: GatewayConfigObject
metadata: {name: openai, namespace: gateway}
spec:
  kind: providers
  # name: openai          # default: the resource's name
  spec:
    type: openai
    base_url: https://api.openai.com/v1
  secretRefs:
    - path: api_key       # a dot path into spec
      secretKeyRef: {name: openai, key: api_key}
```

Each `secretRefs` entry reads a key of a Secret in the resource's namespace and sets it at `path`, so credentials stay out of Git. A changed Secret is applied on the next pass.

Objects are applied in dependency order, so a model can name a provider from the same pass. An object that is also in the config file is overridden by the resource, as by any stored object ([ADR 0028](adr/0028-ui-wins-over-the-file.md)). Deleting the resource deletes the stored object, or for an object from the file resets it to the file's version.

## Status

Each resource gets a `Ready` condition, which `kubectl get` shows:

```console
$ kubectl get gatewayorganizations,gatewayconfigobjects -n gateway
NAME                                           ORGANIZATION   READY   AGE
gatewayorganization.genaiopengateway.io/acme                  True    2m
NAME                                             KIND        READY   AGE
gatewayconfigobject.genaiopengateway.io/openai   providers   True    2m
```

When the gateway refuses a change, `Ready` is `False` with the gateway's error as its message, and the operator tries again each pass. Two resources naming the same organization or object are refused: the first one listed keeps it. `status.observedGeneration` is the generation last applied.

The operator logs every change it makes (`created org acme`, `put providers/openai`) and every error.

## How it works

The operator polls rather than watches: every `interval` it lists both kinds, reads the gateway's state, and changes what differs. Polling also catches changes made in the gateway, and costs one list per kind. A finalizer, `genaiopengateway.io/cleanup`, holds a deleted resource until what it made is gone.

Run one operator per gateway. Several operators against one gateway would undo each other's changes for resources they see differently.
