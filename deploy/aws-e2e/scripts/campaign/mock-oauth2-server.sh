#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# T10's identity providers (ADR 1003): navikt/mock-oauth2-server 6.0.4 in the cluster, over HTTPS with a throwaway CA,
# with Google-shaped (/google, /google-short) and GitHub-shaped (/github) issuers, plus the TrustPolicies that make the
# STS trust them. Key material lives under ~/.config/cucina/aws-e2e/mock-idp (0700) and never in the repository.
#
#   mock-oauth2-server.sh up     generate CA + server certificate, deploy the mock, apply the TrustPolicies
#   mock-oauth2-server.sh down   delete the mock namespace and the TrustPolicies
#
# Environment: KUBECONFIG (default ~/.config/cucina/aws-e2e/kubeconfig), CUCINA_NAMESPACE (default cucina, where the
# TrustPolicies go), MOCK_NAMESPACE (default cucina-e2e). The dev Mac reaches the mock with
#   kubectl -n cucina-e2e port-forward svc/mock-oauth2-server 18443:30443
# (port-forwards.sh), as idp.localAddr=127.0.0.1:18443 in the environment descriptor.
set -eu
umask 077
IMAGE="ghcr.io/navikt/mock-oauth2-server:6.0.4"
NS=${MOCK_NAMESPACE:-cucina-e2e}
CUCINA_NS=${CUCINA_NAMESPACE:-cucina}
DIR=${CUCINA_SECRETS_DIR:-$HOME/.config/cucina}/aws-e2e/mock-idp
export KUBECONFIG="${KUBECONFIG:-${CUCINA_SECRETS_DIR:-$HOME/.config/cucina}/aws-e2e/kubeconfig}"
HOST="mock-oauth2-server.$NS.svc.cluster.local"
BASE="https://$HOST:30443"
CLIENT_ID="cucina-e2e"

die() { echo "mock-oauth2-server.sh: $*" >&2; exit 1; }

certs() {
	mkdir -p "$DIR" && chmod 0700 "$DIR"
	[ -f "$DIR/ca.pem" ] && [ -f "$DIR/keystore.p12" ] && return 0
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 14 -subj "/CN=cucina e2e mock IdP CA" \
		-keyout "$DIR/ca.key" -out "$DIR/ca.pem" 2>/dev/null
	openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=$HOST" \
		-keyout "$DIR/server.key" -out "$DIR/server.csr" 2>/dev/null
	printf 'subjectAltName=DNS:%s,DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth\n' "$HOST" >"$DIR/ext.cnf"
	openssl x509 -req -in "$DIR/server.csr" -CA "$DIR/ca.pem" -CAkey "$DIR/ca.key" -CAcreateserial -days 14 \
		-extfile "$DIR/ext.cnf" -out "$DIR/server.pem" 2>/dev/null
	openssl pkcs12 -export -inkey "$DIR/server.key" -in "$DIR/server.pem" -certfile "$DIR/ca.pem" -name mock \
		-keypbe AES-256-CBC -certpbe AES-256-CBC -macalg sha256 -passout pass:changeit -out "$DIR/keystore.p12"
	rm -f "$DIR/server.csr" "$DIR/ext.cnf"
}

# Claims per case; client_credentials requests select a case with the scope ("openid <case>"), the authorization-code
# flow of `cucinactl login` (T10a) gets the valid Google user.
config() {
	google_ok='"sub":"mock-google-user-1","email":"dev@example.com","email_verified":true,"hd":"example.com","aud":["'$CLIENT_ID'"]'
	gh_ok='"sub":"repo:sloper-ai/cucina:ref:refs/heads/main","aud":["cucina"],"repository_owner_id":"310369022","repository_id":"1401027334","event_name":"push","ref":"refs/heads/main","ref_protected":"true","job_workflow_ref":"sloper-ai/cucina/.github/workflows/ci.yml@refs/heads/main","workflow":"ci","runner_environment":"github-hosted"'
	cat <<JSON
{
  "interactiveLogin": false,
  "httpServer": {"type": "NettyWrapper", "ssl": {"keyPassword": "changeit", "keystoreFile": "/certs/keystore.p12", "keystoreType": "PKCS12", "keystorePassword": "changeit"}},
  "tokenCallbacks": [
    {"issuerId": "google", "tokenExpiry": 900, "requestMappings": [
      {"requestParam": "scope", "match": ".*wrong-hd.*", "claims": {$google_ok, "hd": "evil.example.org"}},
      {"requestParam": "scope", "match": ".*unverified-email.*", "claims": {$google_ok, "email_verified": false}},
      {"requestParam": "scope", "match": ".*wrong-aud.*", "claims": {$google_ok, "aud": ["someone-else"]}},
      {"requestParam": "scope", "match": ".*valid.*", "claims": {$google_ok}},
      {"requestParam": "grant_type", "match": "authorization_code", "claims": {$google_ok}}
    ]},
    {"issuerId": "google-short", "tokenExpiry": 30, "requestMappings": [
      {"requestParam": "scope", "match": ".*", "claims": {$google_ok}}
    ]},
    {"issuerId": "github", "tokenExpiry": 300, "requestMappings": [
      {"requestParam": "scope", "match": ".*foreign-repo.*", "claims": {$gh_ok, "repository_id": "999999999"}},
      {"requestParam": "scope", "match": ".*pull-request-target.*", "claims": {$gh_ok, "event_name": "pull_request_target"}},
      {"requestParam": "scope", "match": ".*push-main.*", "claims": {$gh_ok}}
    ]}
  ]
}
JSON
}

policies() {
	ca=$(sed 's/^/        /' "$DIR/ca.pem")
	for issuer in google google-short; do
		cat <<YAML
---
apiVersion: cucina.sloper.ai/v1alpha1
kind: TrustPolicy
metadata: {name: e2e-mock-$issuer, namespace: $CUCINA_NS, labels: {cucina.sloper.ai/e2e: mock-idp}}
spec:
  type: oidc
  issuer:
    url: $BASE/$issuer
    audiences: ["$CLIENT_ID"]
    certificateAuthority: |
$ca
  claimValidationRules:
    - {expression: "claims.email_verified == true || claims.email_verified == 'true'", message: e-mail not verified}
    - {expression: "has(claims.hd) && claims.hd in ['example.com']", message: not in an allowed Google Workspace domain}
  claimMappings:
    subject: {expression: "'google:' + claims.sub"}
    displayName: {expression: "claims.email"}
  grants:
    - {name: developers, instanceNames: [main], verbs: [cas-read, cas-write, ac-read, execute]}
YAML
		[ "$issuer" = google ] && cat <<YAML
  login: {name: google, clientID: $CLIENT_ID, clientSecret: e2e, scopes: [openid, email, profile], hostedDomainHint: example.com}
YAML
	done
	cat <<YAML
---
apiVersion: cucina.sloper.ai/v1alpha1
kind: TrustPolicy
metadata: {name: e2e-mock-github, namespace: $CUCINA_NS, labels: {cucina.sloper.ai/e2e: mock-idp}}
spec:
  type: oidc
  issuer:
    url: $BASE/github
    audiences: ["cucina"]
    certificateAuthority: |
$ca
  subjectTokenType: urn:ietf:params:oauth:token-type:jwt
  claimValidationRules:
    - {expression: "claims.repository_owner_id == '310369022' && claims.repository_id == '1401027334'", message: not sloper-ai/cucina}
    - {expression: "claims.event_name != 'pull_request_target'", message: pull_request_target is never trusted}
    - {expression: "claims.event_name in ['push', 'merge_group', 'schedule', 'workflow_dispatch', 'pull_request']", message: event not allowed}
  claimMappings:
    subject: {expression: "'github:' + claims.repository_id + ':' + claims.workflow"}
  grants:
    - {name: cache-read, instanceNames: [main], verbs: [cas-read, ac-read]}
YAML
}

up() {
	command -v kubectl >/dev/null || die "kubectl not found"
	certs
	kubectl get ns "$NS" >/dev/null 2>&1 || kubectl create ns "$NS" >/dev/null
	kubectl -n "$NS" create secret generic mock-oauth2-server-tls --from-file=keystore.p12="$DIR/keystore.p12" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	config >"$DIR/config.json"
	kubectl -n "$NS" create configmap mock-oauth2-server-config --from-file=config.json="$DIR/config.json" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: mock-oauth2-server, labels: {app: mock-oauth2-server}}
spec:
  replicas: 1
  selector: {matchLabels: {app: mock-oauth2-server}}
  template:
    metadata: {labels: {app: mock-oauth2-server}}
    spec:
      containers:
        - name: mock-oauth2-server
          image: $IMAGE
          env:
            - {name: SERVER_PORT, value: "30443"}
            - {name: JSON_CONFIG_PATH, value: /config/config.json}
          ports: [{containerPort: 30443}]
          volumeMounts:
            - {name: config, mountPath: /config}
            - {name: tls, mountPath: /certs}
          resources: {requests: {cpu: 50m, memory: 256Mi}, limits: {memory: 512Mi}}
      volumes:
        - {name: config, configMap: {name: mock-oauth2-server-config}}
        - {name: tls, secret: {secretName: mock-oauth2-server-tls}}
---
apiVersion: v1
kind: Service
metadata: {name: mock-oauth2-server}
spec:
  selector: {app: mock-oauth2-server}
  ports: [{name: https, port: 30443, targetPort: 30443}]
YAML
	kubectl -n "$NS" rollout status deploy/mock-oauth2-server --timeout=180s >/dev/null
	policies | kubectl apply -f - >/dev/null
	echo "mock IdP up: issuers $BASE/{google,google-short,github}; CA $DIR/ca.pem"
}

down() {
	kubectl -n "$CUCINA_NS" delete trustpolicy -l cucina.sloper.ai/e2e=mock-idp --ignore-not-found >/dev/null
	kubectl delete ns "$NS" --ignore-not-found --wait=true >/dev/null
	echo "mock IdP removed (certificates kept in $DIR until the run ends)"
}

case ${1:-} in
up) up ;;
down) down ;;
*) die "usage: mock-oauth2-server.sh up|down" ;;
esac
