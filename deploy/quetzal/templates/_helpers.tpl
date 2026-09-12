{{- define "quetzal.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "quetzal.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "quetzal.labels" -}}
app.kubernetes.io/name: {{ include "quetzal.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "quetzal.selectorLabels" -}}
app.kubernetes.io/name: {{ include "quetzal.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "quetzal.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "quetzal.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "quetzal.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
Refuse a combination that destroys data instead of letting it be discovered in
production: two pods opening the same SQLite file. The apiserver is stateless and
the controller elects a leader, so replicaCount > 1 is otherwise fine — it just
needs a database that accepts more than one writer.
*/}}
{{- define "quetzal.validateReplicas" -}}
{{- if gt (int .Values.replicaCount) 1 }}
  {{- if eq .Values.db.driver "sqlite" }}
    {{- fail "replicaCount > 1 needs db.driver=postgres: two pods cannot share one SQLite file, and running them would corrupt it" }}
  {{- end }}
  {{- if .Values.persistence.enabled }}
    {{- fail "replicaCount > 1 needs persistence.enabled=false: the data volume is ReadWriteOnce and holds nothing but the SQLite database" }}
  {{- end }}
{{- end }}
{{- end -}}
