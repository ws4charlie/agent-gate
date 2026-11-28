{{- define "agentgate.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "agentgate.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "agentgate.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
app.kubernetes.io/name: {{ include "agentgate.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ include "agentgate.tag" . | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/* selector labels: call with (dict "ctx" . "component" "gateway") */}}
{{- define "agentgate.selector" -}}
app.kubernetes.io/name: {{ include "agentgate.name" .ctx }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "agentgate.tag" -}}
{{- .Values.image.tag | default .Chart.AppVersion -}}
{{- end -}}

{{- define "agentgate.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ include "agentgate.tag" . }}
{{- end -}}
{{- end -}}

{{/* AG_RPC_URL env entry, literal or from a Secret; empty when neither is set. */}}
{{- define "agentgate.rpcEnv" -}}
{{- if .Values.chain.rpcUrlSecret.name -}}
- name: AG_RPC_URL
  valueFrom:
    secretKeyRef: {name: {{ .Values.chain.rpcUrlSecret.name | quote }}, key: {{ .Values.chain.rpcUrlSecret.key | quote }}}
{{- else if .Values.chain.rpcUrl -}}
- {name: AG_RPC_URL, value: {{ .Values.chain.rpcUrl | quote }}}
{{- end -}}
{{- end -}}

{{- define "agentgate.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- .Values.serviceAccount.name | default (include "agentgate.fullname" .) -}}
{{- else -}}
{{- .Values.serviceAccount.name | default "default" -}}
{{- end -}}
{{- end -}}

{{- define "agentgate.paidtoolHost" -}}
{{ include "agentgate.fullname" . }}-paidtool
{{- end -}}

{{- define "agentgate.paidtoolURL" -}}
http://{{ include "agentgate.paidtoolHost" . }}:{{ .Values.paidtool.port }}
{{- end -}}

{{/* Fails the render on settings that cannot work. */}}
{{- define "agentgate.validate" -}}
{{- $_ := required "database.existingSecret is required (a Secret holding the Postgres URL)" .Values.database.existingSecret -}}
{{- if not (has .Values.signer (list "wallet" "vault")) -}}
{{- fail (printf "signer is %q: want wallet or vault" .Values.signer) -}}
{{- end -}}
{{- if and .Values.image.digest (not (regexMatch "^sha256:[0-9a-f]{64}$" .Values.image.digest)) -}}
{{- fail "image.digest must be sha256:<64 hex>" -}}
{{- end -}}
{{- if and .Values.chain.rpcUrl .Values.chain.rpcUrlSecret.name -}}
{{- fail "set chain.rpcUrl or chain.rpcUrlSecret.name, not both" -}}
{{- end -}}
{{- $chains := list "anvil" "base-sepolia" -}}
{{- $needRPC := or (and .Values.payments.enabled (has .Values.payments.network $chains)) (and .Values.paidtool.enabled (has .Values.paidtool.network $chains)) -}}
{{- if and $needRPC (not .Values.chain.rpcUrl) (not .Values.chain.rpcUrlSecret.name) -}}
{{- fail "chain.rpcUrl or chain.rpcUrlSecret.name is required when payments.network or paidtool.network is anvil or base-sepolia" -}}
{{- end -}}
{{- if and .Values.paidtool.enabled .Values.payments.enabled .Values.payments.paidtoolTools (ne .Values.payments.network .Values.paidtool.network) -}}
{{- fail (printf "payments.network (%s) and paidtool.network (%s) differ, but payments.paidtoolTools sends payments to this paid tool" .Values.payments.network .Values.paidtool.network) -}}
{{- end -}}
{{- if not (regexMatch "^[A-Za-z0-9._-]+$" .Values.wallet.key) -}}
{{- fail "wallet.key must be a plain file name" -}}
{{- end -}}
{{- if not (regexMatch "^[A-Za-z0-9._-]+$" .Values.gateCred.key) -}}
{{- fail "gateCred.key must be a plain file name" -}}
{{- end -}}
{{- range $name, $_ := .Values.gateway.cedarPolicies -}}
{{- if not (hasSuffix ".cedar" $name) -}}
{{- fail (printf "gateway.cedarPolicies: %q must end in .cedar (the gateway loads only *.cedar)" $name) -}}
{{- end -}}
{{- end -}}
{{- if .Values.paidtool.token -}}
{{- if ne .Values.paidtool.network "anvil" -}}
{{- fail "paidtool.token is for paidtool.network=anvil only" -}}
{{- end -}}
{{- if not (regexMatch "^0x[0-9a-fA-F]{40}$" .Values.paidtool.token) -}}
{{- fail "paidtool.token must be a 0x address" -}}
{{- end -}}
{{- end -}}
{{- if and .Values.paidtool.fund (not .Values.paidtool.fundAddress) -}}
{{- fail "paidtool.fund needs paidtool.fundAddress" -}}
{{- end -}}
{{- if .Values.networkPolicy.enabled -}}
{{- $eg := .Values.networkPolicy.egress -}}
{{- $groups := list "postgres" -}}
{{- if eq .Values.signer "vault" -}}{{- $groups = append $groups "vault" -}}{{- $groups = append $groups "tokenProvider" -}}{{- end -}}
{{- range $g := $groups -}}
{{- $rules := index $eg $g -}}
{{- if not $rules -}}
{{- fail (printf "networkPolicy.egress.%s is required with networkPolicy.enabled (else the gateway cannot reach it)" $g) -}}
{{- end -}}
{{- range $rules -}}
{{- if not .ports -}}
{{- fail (printf "networkPolicy.egress.%s: every rule needs ports" $g) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if .Values.payments.enabled -}}
{{- if not (has .Values.payments.network (list "sim" "anvil" "base-sepolia")) -}}
{{- fail "payments.network is required when payments.enabled: sim, anvil or base-sepolia" -}}
{{- end -}}
{{- if and .Values.payments.paidtoolTools (not .Values.paidtool.enabled) -}}
{{- fail "payments.paidtoolTools needs paidtool.enabled" -}}
{{- end -}}
{{- if and (eq .Values.payments.network "sim") (not .Values.payments.chainQueryURL) (not .Values.paidtool.enabled) -}}
{{- fail "payments.network=sim needs payments.chainQueryURL or paidtool.enabled" -}}
{{- end -}}
{{- if eq .Values.signer "wallet" -}}
{{- $_ := required "wallet.existingSecret is required with payments.enabled and signer=wallet" .Values.wallet.existingSecret -}}
{{- end -}}
{{- end -}}
{{- if eq .Values.signer "vault" -}}
{{- if not .Values.payments.enabled -}}
{{- fail "signer=vault needs payments.enabled (the signer only signs payments)" -}}
{{- end -}}
{{- $v := .Values.vault -}}
{{- range $k := list "url" "walletId" "walletAddress" "tokenUrl" "clientId" "audience" "existingSecret" -}}
{{- if not (index $v $k) -}}
{{- fail (printf "vault.%s is required with signer=vault" $k) -}}
{{- end -}}
{{- end -}}
{{- $_ := required "gateCred.existingSecret is required with signer=vault" .Values.gateCred.existingSecret -}}
{{- $_ := required "gateCred.kid is required with signer=vault" .Values.gateCred.kid -}}
{{- end -}}
{{- if and .Values.gateway.cedarEnforce (not .Values.gateway.cedarPolicies) -}}
{{- fail "gateway.cedarEnforce needs gateway.cedarPolicies" -}}
{{- end -}}
{{- if .Values.paidtool.enabled -}}
{{- $_ := required "paidtool.payTo is required when paidtool.enabled" .Values.paidtool.payTo -}}
{{- if and (eq .Values.paidtool.network "base-sepolia") (not .Values.paidtool.facilitatorKey.existingSecret) -}}
{{- fail "paidtool.network=base-sepolia needs paidtool.facilitatorKey.existingSecret" -}}
{{- end -}}
{{- end -}}
{{- if or (hasKey .Values.gateway.config "listen") (hasKey .Values.gateway.config "database_url") -}}
{{- fail "gateway.config must not set listen or database_url (the chart sets them)" -}}
{{- end -}}
{{- end -}}

{{/* payments file content */}}
{{- define "agentgate.paymentsJSON" -}}
{{- $tools := dict -}}
{{- range $name, $url := .Values.payments.tools -}}
{{- $_ := set $tools $name $url -}}
{{- end -}}
{{- range $name := .Values.payments.paidtoolTools -}}
{{- $_ := set $tools $name (printf "%s/%s" (include "agentgate.paidtoolURL" $) $name) -}}
{{- end -}}
{{- $f := dict "network" .Values.payments.network "tools" $tools -}}
{{- if .Values.payments.maxTxUSD -}}
{{- $_ := set $f "max_tx_usd" (toString .Values.payments.maxTxUSD) -}}
{{- end -}}
{{- if eq .Values.payments.network "sim" -}}
{{- $_ := set $f "chain_query_url" (.Values.payments.chainQueryURL | default (include "agentgate.paidtoolURL" .)) -}}
{{- end -}}
{{- toPrettyJson $f -}}
{{- end -}}

{{- define "agentgate.dnsEgress" -}}
- to:
    - namespaceSelector:
        matchLabels: {kubernetes.io/metadata.name: kube-system}
      podSelector:
        matchLabels: {k8s-app: kube-dns}
  ports:
    - {port: 53, protocol: UDP}
    - {port: 53, protocol: TCP}
{{- end -}}
