{{- define "oci-shared-nlb-controller.name" -}}
oci-shared-nlb-controller
{{- end }}

{{- define "oci-shared-nlb-controller.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "oci-shared-nlb-controller.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "oci-shared-nlb-controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "oci-shared-nlb-controller.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end }}

{{- define "oci-shared-nlb-controller.labels" -}}
app.kubernetes.io/name: {{ include "oci-shared-nlb-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end }}

{{- define "oci-shared-nlb-controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "oci-shared-nlb-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
