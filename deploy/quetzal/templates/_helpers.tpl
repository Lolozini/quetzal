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
{{/*
The database settings a release cannot run with, refused when it is rendered
rather than found out by a pod that crashloops: PostgreSQL fields on SQLite,
two ways of naming one server at once, or PostgreSQL with nothing to reach.
*/}}
{{- define "quetzal.validateDB" -}}
{{- $db := .Values.db }}
{{- if $db.host }}
  {{- if eq $db.driver "sqlite" }}
    {{- fail "db.host is for db.driver=postgres; SQLite takes the file in db.dsn" }}
  {{- end }}
  {{- if $db.existingSecret }}
    {{- fail "db.host gives the connection field by field and db.existingSecret a whole DSN: set one of them" }}
  {{- end }}
  {{- if and $db.password $db.existingPasswordSecret }}
    {{- fail "db.password and db.existingPasswordSecret: set one of them" }}
  {{- end }}
{{- else if ne $db.driver "sqlite" }}
  {{- if and (not $db.existingSecret) (hasPrefix "/" $db.dsn) }}
    {{- fail "db.driver=postgres needs db.host (or a whole DSN in db.dsn or db.existingSecret); db.dsn still names the SQLite file" }}
  {{- end }}
  {{- if $db.sslMode }}
    {{- fail "db.sslMode goes with db.host; a DSN carries its own sslmode" }}
  {{- end }}
{{- end }}
{{- end -}}

{{/* The CA of the database server, where it is mounted (see db.sslRootCertSecret). */}}
{{- define "quetzal.dbVolumeMount" -}}
{{- if .Values.db.sslRootCertSecret }}
- name: db-ca
  mountPath: /etc/quetzal/db-ca
  readOnly: true
{{- end }}
{{- end -}}

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
