# Kubernetes operator

The operator manages organizations, teams, Cedar policies and stored configuration objects as Kubernetes resources, so a platform team can keep tenants in Git and apply them with Argo CD or Flux. It is `gogctl operator`, shipped in the gateway image, and works through the [admin API](api.md) as [`gogctl config import`](gogctl.md) does for a file. Decisions are recorded in [ADR 0034](adr/0034-control-and-data-planes.md), which refines ADR 0010.

## Installing

The [Helm chart](helm.md) installs three CRDs from its `crds/` directory and, with `operator.enabled`, runs the operator:

```yaml
operator:
  enabled: true
  adminTokenSecret: {name: genaiopengateway, key: GATEWAY_ADMIN_TOKEN}   # an admin token with the owner role
  watchNamespace: ""   # empty watches every namespace, with a ClusterRole; a namespace gets a Role there
  interval: 15s        # time between full passes; changes to resources are applied at once
```

The operator reaches the gateway at its Service. It may read and watch the three resource kinds, read Secrets, and write their finalizers and status; it needs nothing else in the cluster. Helm installs CRDs once and never upgrades them, so after an upgrade that changes them run `kubectl apply -f crds/` from the chart.

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

## Cedar policy sets

A `GatewayCedarPolicySet` holds Cedar policies that several organizations share, such as a company-wide residency rule, so they are written once:

```yaml
apiVersion: genaiopengateway.io/v1
kind: GatewayCedarPolicySet
metadata: {name: residency, namespace: gateway}
spec:
  organizations: [acme, globex]   # or allOrganizations: true
  cedar: |
    forbid (principal, action == Action::"invoke_model", resource)
    when { resource.residency != "eu" };
```

A set applies only to organizations that a `GatewayOrganization` manages, named as in the gateway; `allOrganizations` selects every one of them. The Cedar policies of an organization are then the sets that select it, in the order of their namespaces and names, each under a comment naming the set, followed by the organization's own `cedar`. Cedar evaluates policies together, so the order is only for reading: a `forbid` in a set cannot be undone by an organization's `permit`.

Changing a set changes every organization it selects, and deleting it removes its policies from them. `Ready` is `False` on a set that names an organization no resource manages, that has no `cedar`, or whose policies the gateway refused for an organization; in the last case that organization's resource is `False` too, and its policies stay as they were.

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

The operator watches the three kinds, and makes a pass when a resource is added, changed or deleted: it lists the resources, reads the gateway's state, and changes what differs. A change is in the gateway within a second or so. Every `interval` it makes the same pass without a change, which undoes changes made in the gateway and picks up changed Secrets, since Secrets are not watched. On a large installation `interval` can be raised to a few minutes without slowing down changes to resources. Its own status and finalizer writes do not start passes. When a watch fails, the operator logs it and starts another from the next pass; until then the interval still applies.

A cluster whose CRDs were installed before policy sets existed runs as before, and the operator says once that the kind is missing. A finalizer, `genaiopengateway.io/cleanup`, holds a deleted resource until what it made is gone.

Run one operator per gateway. Several operators against one gateway would undo each other's changes for resources they see differently.
