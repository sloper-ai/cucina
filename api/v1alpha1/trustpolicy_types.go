// SPDX-License-Identifier: FSL-1.1-ALv2

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Verbs a grant can give (R-AUTH-2). They map onto the Buildbarn authorizers
// rendered by the chart (R-AUTH-4): cas-read→CAS get/findMissing, cas-write→CAS put,
// ac-read→AC get, ac-write→AC put, execute→Execute/WaitExecution, admin→management API
// mutations, BuildQueueState and drain/kill.
const (
	VerbCASRead  = "cas-read"
	VerbCASWrite = "cas-write"
	VerbACRead   = "ac-read"
	VerbACWrite  = "ac-write"
	VerbExecute  = "execute"
	VerbAdmin    = "admin"
)

// TrustPolicy tells the Cucina STS which external identities may obtain Cucina
// JWTs and what those JWTs allow. It is modeled on Kubernetes'
// AuthenticationConfiguration (apiserver.config.k8s.io/v1): an issuer, CEL claim
// validation rules and claim mappings, extended with grants (verbs × instance names).
//
// Evaluation (internal/auth): the external token is verified with go-oidc (exact iss,
// aud, exp, signature); every claimValidationRule must evaluate to true; the mapped
// subject becomes the principal; the union of the grants whose condition is true gives
// the JWT's `cucina` claim. A token matching no policy is rejected. CEL is compiled at
// load time, must return bool, has a cost limit and a per-evaluation timeout, and fails
// closed.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=tp,categories=cucina
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Issuer",type=string,JSONPath=`.spec.issuer.url`
// +kubebuilder:printcolumn:name="Valid",type=string,JSONPath=`.status.conditions[?(@.type=="Valid")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type TrustPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TrustPolicySpec   `json:"spec"`
	Status TrustPolicyStatus `json:"status,omitempty"`
}

// TrustPolicyList is a list of TrustPolicy.
//
// +kubebuilder:object:root=true
type TrustPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TrustPolicy `json:"items"`
}

// TrustPolicySpec is one trust rule.
//
// +kubebuilder:validation:XValidation:rule="self.type != 'oidc' || has(self.issuer)",message="issuer is required for type oidc"
// +kubebuilder:validation:XValidation:rule="self.type != 'serviceAccount' || has(self.serviceAccount)",message="serviceAccount is required for type serviceAccount"
// +kubebuilder:validation:XValidation:rule="self.type != 'oidc' || has(self.claimMappings)",message="claimMappings.subject is required for type oidc"
type TrustPolicySpec struct {
	// Type is "oidc" (default) for external identity providers or "serviceAccount" for opt-in
	// long-lived keys (R-AUTH-10), which are exchanged at the STS like any other subject token.
	// +kubebuilder:validation:Enum=oidc;serviceAccount
	// +kubebuilder:default=oidc
	Type string `json:"type"`

	// Disabled keeps the object but ignores it.
	// +optional
	Disabled bool `json:"disabled,omitempty"`

	// Issuer of the external token (type oidc).
	// +optional
	Issuer *IssuerSpec `json:"issuer,omitempty"`

	// SubjectTokenType is the RFC 8693 subject_token_type this policy accepts:
	// "urn:ietf:params:oauth:token-type:id_token" (default) or "urn:ietf:params:oauth:token-type:jwt"
	// (GitHub Actions).
	// +optional
	SubjectTokenType string `json:"subjectTokenType,omitempty"`

	// ClaimValidationRules are CEL expressions over `claims`; all must be true.
	// +optional
	ClaimValidationRules []ClaimValidationRule `json:"claimValidationRules,omitempty"`

	// ClaimMappings derive the principal from the claims.
	// +optional
	ClaimMappings *ClaimMappings `json:"claimMappings,omitempty"`

	// Grants are evaluated independently; the result is their union. A grant with a CEL
	// condition applies only when the condition is true (fail closed).
	// +kubebuilder:validation:MinItems=1
	Grants []Grant `json:"grants"`

	// ServiceAccount configures type serviceAccount.
	// +optional
	ServiceAccount *ServiceAccountSpec `json:"serviceAccount,omitempty"`

	// Login publishes interactive-login parameters in /.well-known/cucina-configuration so
	// `cucinactl login <url>` needs no other config (R-AUTH-1, R-AUTH-5).
	// +optional
	Login *LoginSpec `json:"login,omitempty"`

	// RequireUniqueJTI enables the jti replay cache for this issuer (GitHub, R-AUTH-7).
	// +optional
	RequireUniqueJTI bool `json:"requireUniqueJTI,omitempty"`

	// GroupLookup optionally resolves group membership at every exchange (cached 5–10 min),
	// e.g. Google Cloud Identity searchDirectGroups (R-AUTH-2/-5).
	// +optional
	GroupLookup *GroupLookupSpec `json:"groupLookup,omitempty"`
}

// IssuerSpec describes an external OIDC issuer (modeled on structured authentication config).
type IssuerSpec struct {
	// URL is the exact `iss` value (e.g. https://accounts.google.com, https://token.actions.githubusercontent.com).
	// +kubebuilder:validation:Pattern=`^https://`
	URL string `json:"url"`
	// DiscoveryURL overrides {url}/.well-known/openid-configuration.
	// +optional
	DiscoveryURL string `json:"discoveryURL,omitempty"`
	// AdditionalIssuers lists extra accepted `iss` values (Google also emits "accounts.google.com").
	// +optional
	AdditionalIssuers []string `json:"additionalIssuers,omitempty"`
	// Audiences accepted in `aud`; for GitHub the audience used only by Cucina ("cucina").
	// +kubebuilder:validation:MinItems=1
	Audiences []string `json:"audiences"`
	// CertificateAuthority is a PEM bundle trusted for discovery/JWKS fetches (optional).
	// +optional
	CertificateAuthority string `json:"certificateAuthority,omitempty"`
}

// ClaimValidationRule is a CEL expression over `claims` that must evaluate to true.
type ClaimValidationRule struct {
	// Expression is CEL, e.g. `claims.hd in ['example.com'] && string(claims.email_verified) == 'true'`.
	Expression string `json:"expression"`
	// Message is returned (and audited) when the rule is false.
	// +optional
	Message string `json:"message,omitempty"`
}

// ClaimMappings derive the principal. All fields are CEL expressions over `claims`.
type ClaimMappings struct {
	// Subject is the stable principal, e.g. `'google:' + claims.sub` or
	// `'github:' + claims.repository_id + ':' + claims.workflow`. Never key on email.
	Subject CELExpression `json:"subject"`
	// DisplayName is for audit/UI only.
	// +optional
	DisplayName *CELExpression `json:"displayName,omitempty"`
	// Groups yields a list of group strings.
	// +optional
	Groups *CELExpression `json:"groups,omitempty"`
}

// CELExpression wraps one CEL expression.
type CELExpression struct {
	Expression string `json:"expression"`
}

// Grant gives verbs on instance names.
type Grant struct {
	// Name labels the grant in audit logs.
	// +optional
	Name string `json:"name,omitempty"`
	// Condition is an optional CEL expression over `claims`, `subject` and `groups`.
	// +optional
	Condition string `json:"condition,omitempty"`
	// InstanceNames the verbs apply to; "*" = every configured instance name.
	// +kubebuilder:validation:MinItems=1
	InstanceNames []string `json:"instanceNames"`
	// Verbs: cas-read, cas-write, ac-read, ac-write, execute, admin.
	// +kubebuilder:validation:MinItems=1
	Verbs []string `json:"verbs"`
	// MaxTTL caps the JWT lifetime for this grant (default and maximum 15m for humans and CI).
	// +optional
	MaxTTL *metav1.Duration `json:"maxTTL,omitempty"`
}

// ServiceAccountSpec configures a service-account principal (keys are created with
// `cucinactl keys create`, stored hashed, revocable instantly).
type ServiceAccountSpec struct {
	// Name is the principal name (subject becomes `sa:<name>`).
	Name string `json:"name"`
	// Break-glass marks the bootstrap admin account generated by `helm install` (R-AUTH-12).
	// +optional
	BreakGlass bool `json:"breakGlass,omitempty"`
}

// LoginSpec is published for interactive clients.
type LoginSpec struct {
	// Name of the identity provider shown by the CLI ("google", "github", …).
	Name string `json:"name"`
	// ClientID of the OAuth2 "Desktop app" client; ClientSecret is non-confidential by design
	// (Google desktop clients) and is shipped in the discovery document.
	ClientID     string `json:"clientID"`
	ClientSecret string `json:"clientSecret,omitempty"`
	// Scopes requested; default [openid, email, profile] (+ offline_access for non-Google).
	// +optional
	Scopes []string `json:"scopes,omitempty"`
	// HostedDomainHint is passed as `hd` (a hint only; trust comes from claimValidationRules).
	// +optional
	HostedDomainHint string `json:"hostedDomainHint,omitempty"`
	// RedirectPorts restricts loopback ports for providers that need registered ports (empty = any).
	// +optional
	RedirectPorts []int32 `json:"redirectPorts,omitempty"`
}

// GroupLookupSpec resolves group membership out of band.
type GroupLookupSpec struct {
	// Provider: "cloud-identity".
	// +kubebuilder:validation:Enum=cloud-identity
	Provider string `json:"provider"`
	// CacheTTL of results (default 10m).
	// +optional
	CacheTTL *metav1.Duration `json:"cacheTTL,omitempty"`
}

// TrustPolicyStatus is written by the controller's STS.
type TrustPolicyStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
