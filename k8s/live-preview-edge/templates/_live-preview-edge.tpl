{{/*
live-preview-edge — reusable named templates for running Glimmung's live-preview
edge in front of an app backend inside a preview test environment.

An app's slot chart includes these to compose the edge into ITS own Deployment /
Service / HTTPRoute. Cross-repo consumption (Stage 4a): Glimmung publishes this
chart as a ConfigMap and vendors it into the app chart's charts/ at preview-
provision time (NOT an oci:// dependency — ACR is Basic SKU). App charts include
the templates guarded on livePreview.enabled and declare no Helm dependency. See
docs/live-preview-plan.md "Stage 4a landed contracts" and the chart README.

Activation: every template self-gates on the spec's `.enabled`. Include them
UNCONDITIONALLY — when livePreview.enabled is false they render nothing, so
wiring the partial in changes no behavior until Glimmung sets the value on a
preview lease (inactive-until-wired).

Each container/volume template takes the live-preview spec dict as its context
(`.`), e.g.  {{ include "live-preview-edge.container" .Values.livePreview }}.
The replicas/servedPortName helpers take a dict of named args (see below). The
spec shape is documented in this chart's values.yaml.
*/}}

{{- define "live-preview-edge.enabled" -}}
{{- if .enabled -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "live-preview-edge.portName" -}}
{{- .servedPortName | default "http" -}}
{{- end -}}

{{- define "live-preview-edge.port" -}}
{{- .port | default 8080 -}}
{{- end -}}

{{- define "live-preview-edge.volumeName" -}}
{{- dig "override" "volumeName" "live-preview-override" . -}}
{{- end -}}

{{- define "live-preview-edge.overrideMountPath" -}}
{{- dig "override" "mountPath" "/var/run/glimmung-live-preview" . -}}
{{- end -}}

{{/*
live-preview-edge.container renders the edge container for the consumer's pod
spec `containers:` list. Empty when disabled. Config is derived from the spec
and mirrors cmd/live-preview-edge's env contract.
*/}}
{{- define "live-preview-edge.container" -}}
{{- if .enabled -}}
- name: {{ .containerName | default "live-preview-edge" }}
  image: "{{ required "livePreview.image.repository is required when livePreview.enabled" (dig "image" "repository" "" .) }}:{{ required "livePreview.image.tag is required when livePreview.enabled" (dig "image" "tag" "" .) }}"
  imagePullPolicy: {{ dig "image" "pullPolicy" "IfNotPresent" . }}
  env:
    - name: LIVE_PREVIEW_EDGE_LISTEN
      value: ":{{ include "live-preview-edge.port" . }}"
    - name: LIVE_PREVIEW_EDGE_UPSTREAM
      value: {{ required "livePreview.upstream.url is required when livePreview.enabled" (dig "upstream" "url" "" .) | quote }}
    - name: LIVE_PREVIEW_EDGE_BACKEND_PREFIXES
      value: {{ join "," (.backendPrefixes | default (list)) | quote }}
    - name: LIVE_PREVIEW_EDGE_OVERRIDE_ROOT
      value: {{ include "live-preview-edge.overrideMountPath" . | quote }}
    - name: LIVE_PREVIEW_EDGE_AUTHORIZED_SUBJECT
      value: {{ required "livePreview.authorizedSubject is required when livePreview.enabled" .authorizedSubject | quote }}
  ports:
    - name: {{ include "live-preview-edge.portName" . }}
      containerPort: {{ include "live-preview-edge.port" . }}
  readinessProbe:
    httpGet:
      path: /__live-preview/readyz
      port: {{ include "live-preview-edge.portName" . }}
    initialDelaySeconds: 3
    periodSeconds: 10
  livenessProbe:
    httpGet:
      path: /__live-preview/healthz
      port: {{ include "live-preview-edge.portName" . }}
    initialDelaySeconds: 5
    periodSeconds: 30
  volumeMounts:
    - name: {{ include "live-preview-edge.volumeName" . }}
      mountPath: {{ include "live-preview-edge.overrideMountPath" . }}
  resources:
    {{- toYaml (.resources | default (dict "requests" (dict "cpu" "25m" "memory" "32Mi") "limits" (dict "cpu" "250m" "memory" "128Mi"))) | nindent 4 }}
{{- end -}}
{{- end -}}

{{/*
live-preview-edge.volume renders the per-pod override emptyDir for the
consumer's pod spec `volumes:` list. Empty when disabled.
*/}}
{{- define "live-preview-edge.volume" -}}
{{- if .enabled -}}
- name: {{ include "live-preview-edge.volumeName" . }}
  emptyDir: {}
{{- end -}}
{{- end -}}

{{/*
live-preview-edge.replicas enforces the single-serving-pod invariant. Call it
for the consumer Deployment's `replicas:` with the requested count:

  replicas: {{ include "live-preview-edge.replicas" (dict "spec" .Values.livePreview "requested" .Values.replicas) }}

When live preview is enabled it returns 1 and FAILS the render if the consumer
asked for more than one replica. A preview env must be a single serving pod: a
per-pod emptyDir override behind replicas>1 load-balances reads 50/50 and the
co-watched frontend flickers (v1 #1419). When disabled it echoes the requested
count, so normal (non-preview) renders are unaffected.
*/}}
{{- define "live-preview-edge.replicas" -}}
{{- $spec := .spec -}}
{{- $requested := .requested | default 1 -}}
{{- if $spec.enabled -}}
{{- if gt (int $requested) 1 -}}
{{- fail "livePreview.enabled requires a single serving pod (replicas=1): a per-pod emptyDir override with replicas>1 load-balances reads 50/50 and flickers the co-watched frontend (v1 #1419)" -}}
{{- end -}}
1
{{- else -}}
{{- $requested -}}
{{- end -}}
{{- end -}}

{{/*
live-preview-edge.servedPortName returns the port name the pod's Service /
HTTPRoute should target. When live preview is enabled the edge owns the served
port (so it fronts the backend); otherwise the consumer's own app port name is
served unchanged. Call with named args:

  targetPort: {{ include "live-preview-edge.servedPortName" (dict "spec" .Values.livePreview "appPortName" "app-internal") }}
*/}}
{{- define "live-preview-edge.servedPortName" -}}
{{- $spec := .spec -}}
{{- if $spec.enabled -}}
{{- include "live-preview-edge.portName" $spec -}}
{{- else -}}
{{- required "appPortName is required for live-preview-edge.servedPortName" .appPortName -}}
{{- end -}}
{{- end -}}
