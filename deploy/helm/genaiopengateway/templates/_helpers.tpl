{{- define "gog.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "gog.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "gog.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gog.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "gog.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "gog.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "gog.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "gog.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "gog.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{/* The most replicas the release can run. */}}
{{- define "gog.maxReplicas" -}}
{{- if .Values.autoscaling.enabled -}}
{{- max .Values.autoscaling.minReplicas .Values.autoscaling.maxReplicas -}}
{{- else -}}
{{- .Values.replicaCount -}}
{{- end -}}
{{- end -}}

{{/*
gog.validate fails the render on a config that cannot work: none at all,
or several replicas without a shared store and limiter (ADRs 0004, 0005).
*/}}
{{- define "gog.validate" -}}
{{- if not (trim .Values.config) -}}
{{- fail "config is empty: set it to the gateway's config.yaml (docs/config.md)" -}}
{{- end -}}
{{- $cfg := fromYaml .Values.config -}}
{{- if hasKey $cfg "Error" -}}
{{- fail (printf "config is not valid YAML: %s" $cfg.Error) -}}
{{- end -}}
{{- if gt (int (include "gog.maxReplicas" .)) 1 -}}
{{- $store := $cfg.store | default dict -}}
{{- $limiter := $cfg.limiter | default dict -}}
{{- if and $store (ne ($store.driver | default "sqlite") "postgres") -}}
{{- fail "several replicas need store.driver: postgres; SQLite cannot be shared between pods" -}}
{{- end -}}
{{- if ne ($limiter.driver | default "memory") "valkey" -}}
{{- fail "several replicas need limiter.driver: valkey; in-memory limits would be counted per pod" -}}
{{- end -}}
{{- end -}}
{{- end -}}
