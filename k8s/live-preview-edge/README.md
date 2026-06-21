# live-preview-edge (Helm library chart)

Reusable named templates for running Glimmung's **live-preview edge** in front of
an app's backend inside a preview test environment. The edge
(`cmd/live-preview-edge`) is an override-first reverse proxy: it proxies the
stable app backend by default and serves a developer's freshly-pushed frontend
bundle once one is pushed, so UI iterates in seconds without a CI image
build+deploy.

This chart owns the **partial and its render contract**. How an app repo
(ambience, kill-me, …) vendors or depends on it is settled in a later stage; the
`k8s/live-preview-edge-harness` chart stands in for a consumer to prove the
partial renders.

## Lane boundary

The live-preview lane is **scratch, for seeing** — never a validation input. It
is distinct from the faithful image-deploy lane (which runs the exact CI image)
and shares no vocabulary with the retired hot-swap path
(`scripts/check-deleted-test-slot-hot-swap.mjs`).

## Invariants

- **Inactive until wired.** Every template self-gates on `livePreview.enabled`.
  Include them unconditionally; with `enabled: false` they render nothing, so
  adding the partial to a chart changes no behavior until Glimmung sets the value
  on a preview lease.
- **Single serving pod.** When enabled, `live-preview-edge.replicas` returns `1`
  and **fails the render** if the consumer asked for more. A per-pod `emptyDir`
  override behind `replicas > 1` load-balances reads 50/50 and the co-watched
  frontend flickers (v1 #1419). Always drive the Deployment's `replicas:` through
  this helper.
- **A pod may only write its own preview.** The edge accepts pushes only from the
  `authorizedSubject` (the lease's verified, IdP-signed auth.romaine.life JWT
  `sub`). Set it per lease.

## Templates

| Template | Arg | Renders |
| --- | --- | --- |
| `live-preview-edge.container` | the `livePreview` spec | the edge container (empty when disabled) |
| `live-preview-edge.volume` | the `livePreview` spec | the override `emptyDir` volume (empty when disabled) |
| `live-preview-edge.replicas` | `dict "spec" <spec> "requested" <n>` | `1` when enabled (fails if requested > 1), else `<n>` |
| `live-preview-edge.servedPortName` | `dict "spec" <spec> "appPortName" <name>` | the edge port name when enabled, else `<appPortName>` |
| `live-preview-edge.enabled` | the `livePreview` spec | `"true"`/`"false"` |

The spec shape is documented in [`values.yaml`](values.yaml).

## Consuming it

In the app's slot chart, after declaring this chart as a dependency:

```yaml
# Deployment
spec:
  replicas: {{ include "live-preview-edge.replicas" (dict "spec" .Values.livePreview "requested" .Values.replicas) }}
  template:
    spec:
      containers:
        - name: app
          image: {{ .Values.appImage | quote }}
          ports:
            - name: app-internal      # the backend listens internally
              containerPort: 8000
        {{- include "live-preview-edge.container" .Values.livePreview | nindent 8 }}
      {{- if .Values.livePreview.enabled }}
      volumes:
        {{- include "live-preview-edge.volume" .Values.livePreview | nindent 8 }}
      {{- end }}
---
# Service — the edge becomes the served port when enabled
spec:
  ports:
    - name: http
      port: 80
      targetPort: {{ include "live-preview-edge.servedPortName" (dict "spec" .Values.livePreview "appPortName" "app-internal") }}
```

The edge's `upstream.url` points at the app backend (localhost in the single-pod
model). Configured `backendPrefixes` (e.g. `/api`, `/healthz`) always proxy to
the backend so its API stays reachable through the edge.

## Proving it renders

```sh
helm dependency update k8s/live-preview-edge-harness
helm template demo k8s/live-preview-edge-harness                          # enabled (preview)
helm template demo k8s/live-preview-edge-harness --set livePreview.enabled=false   # inert
helm template demo k8s/live-preview-edge-harness --set replicas=3         # fails: single-pod invariant
```
