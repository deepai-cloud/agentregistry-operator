{{/* Keep webhook and managed gateway images identical, including digest pins. */}}
{{- define "agentregistry-operator.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository .Values.image.tag -}}
{{- end -}}
{{- end -}}

{{/* Selectors stay stable across upgrades; descriptive labels belong to metadata. */}}
{{- define "agentregistry-operator.labels" -}}
{{- dict
  "app.kubernetes.io/name" "agentregistry-operator"
  "app.kubernetes.io/instance" .Release.Name
  "app.kubernetes.io/version" .Chart.AppVersion
  "app.kubernetes.io/managed-by" .Release.Service
  "helm.sh/chart" (printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-")
  | toJson -}}
{{- end -}}
