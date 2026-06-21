{{- define "glimmung-issue.renderMode" -}}
{{- $mode := .Values.renderMode | default "normal" -}}
{{- if not (has $mode (list "normal" "warm" "hot")) -}}
{{- fail (printf "renderMode must be one of: normal, warm, hot; got %q" $mode) -}}
{{- end -}}
{{- $mode -}}
{{- end -}}

{{- define "glimmung-issue.isTestEnv" -}}
{{- $mode := include "glimmung-issue.renderMode" . -}}
{{- if or (eq $mode "warm") (eq $mode "hot") -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "glimmung-issue.renderWarm" -}}
{{- $mode := include "glimmung-issue.renderMode" . -}}
{{- if or (eq $mode "normal") (eq $mode "warm") -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "glimmung-issue.renderHot" -}}
{{- $mode := include "glimmung-issue.renderMode" . -}}
{{- if or (eq $mode "normal") (eq $mode "hot") -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "glimmung-issue.resourceName" -}}
{{- if eq (include "glimmung-issue.isTestEnv" .) "true" -}}{{ required "testEnv.slotName is required when renderMode is warm or hot" .Values.testEnv.slotName }}{{- else -}}{{ .Release.Name }}{{- end -}}
{{- end -}}

{{/*
glimmung-issue.appPortName — the backend container's served port name. When a
live-preview edge fronts the backend (livePreview.enabled), the edge owns the
"http" served port, so the backend's own port is renamed to an internal name to
avoid a duplicate port name in the pod; the Service then targets the edge via
live-preview-edge.servedPortName. Without the edge it stays "http", so normal /
validation renders are byte-identical.
*/}}
{{- define "glimmung-issue.appPortName" -}}
{{- if .Values.livePreview.enabled -}}app-internal{{- else -}}http{{- end -}}
{{- end -}}
