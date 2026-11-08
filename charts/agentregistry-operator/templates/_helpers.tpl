{{/* Keep webhook and managed gateway images identical, including digest pins. */}}
{{- define "agentregistry-operator.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository .Values.image.tag -}}
{{- end -}}
{{- end -}}

{{/* Label values are limited to 63 characters and must end in an alphanumeric. */}}
{{- define "agentregistry-operator.labelValue" -}}
{{- regexReplaceAll "[^A-Za-z0-9_.-]" (toString .) "_" | trunc 63 | trimAll "-_." -}}
{{- end -}}

{{/* Keep the exact version available when the display label is shortened. */}}
{{- define "agentregistry-operator.annotations" -}}
{{- dict "registry.deepai.cloud/operator-version" .Chart.AppVersion | toJson -}}
{{- end -}}

{{/* Selectors stay stable across upgrades; descriptive labels belong to metadata. */}}
{{- define "agentregistry-operator.labels" -}}
{{- dict
  "app.kubernetes.io/name" "agentregistry-operator"
  "app.kubernetes.io/instance" .Release.Name
  "app.kubernetes.io/version" (include "agentregistry-operator.labelValue" .Chart.AppVersion)
  "app.kubernetes.io/managed-by" .Release.Service
  "helm.sh/chart" (include "agentregistry-operator.labelValue" (printf "%s-%s" .Chart.Name .Chart.Version))
  | toJson -}}
{{- end -}}
