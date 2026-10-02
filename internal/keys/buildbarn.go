// SPDX-License-Identifier: FSL-1.1-ALv2

package keys

import "strings"

// The Buildbarn `jwt` authentication policy and authorizer expressions for Cucina JWTs
// (R-AUTH-4, R-AUTH-9; docs/security.md §Deny-list). The chart renders exactly these
// strings; tests evaluate them with go-jmespath v0.4.0, the library bb-storage uses.

// BuildbarnClaimsValidation is claimsValidationJmespathExpression for issuer (the STS URL
// without a trailing slash): iss and aud must match, exp must be a number, and sub, sid
// and cucina must be present with the right types.
func BuildbarnClaimsValidation(issuer string) string {
	return "payload.iss == '" + strings.ReplaceAll(issuer, "'", `\'`) + "' && payload.aud == '" + Audience +
		"' && type(payload.exp) == 'number' && type(payload.sub) == 'string' && type(payload.sid) == 'string'" +
		" && type(payload.cucina) == 'object'"
}

// BuildbarnMetadataExtraction is metadataExtractionJmespathExpression: the verb lists of
// the `cucina` claim plus sid and sub become the private metadata (so certificate
// metadata, which carries `sub` = URI SAN, shares the authorizers); sub is also public.
const BuildbarnMetadataExtraction = `{"public": {"user": payload.sub}, "private": merge(payload.cucina, {"sid": payload.sid, "sub": payload.sub})}`

// DenyListFileKey is the `files[].key` under which authorizers load denylist.json.
const DenyListFileKey = "denylist"

// BuildbarnDenyFragment is ANDed into every authorizer: false when the caller's sid or sub
// is deny-listed. Callers without sid (certificates) pass the sid half.
const BuildbarnDenyFragment = `!contains(files.denylist, join('', ['"sid:', to_string(authenticationMetadata.private.sid), '"'])) && ` +
	`!contains(files.denylist, join('', ['"sub:', to_string(authenticationMetadata.private.sub), '"']))`

// BuildbarnAuthorizer is the authorizer expression for one `cucina` claim key
// (cas_read, cas_write, ac_read, ac_write, execute).
func BuildbarnAuthorizer(claimKey string) string {
	return "contains(authenticationMetadata.private." + claimKey + ", instanceName) && " + BuildbarnDenyFragment
}
