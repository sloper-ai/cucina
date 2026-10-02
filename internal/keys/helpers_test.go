// SPDX-License-Identifier: FSL-1.1-ALv2

package keys_test

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/keys"
)

func authConfig() config.Auth {
	return config.Auth{
		SigningKeySecret: "cucina-signing-keys", JWKSConfigMap: "cucina-jwks", DenyListConfigMap: "cucina-denylist",
		ServiceKeysSecret: "cucina-service-keys", BreakGlassKeySecret: "cucina-break-glass",
		TokenTTL: config.Duration{Duration: 15 * time.Minute}, Audience: "buildbarn",
		KeyRotationPublishLead: config.Duration{Duration: 10 * time.Minute}, RateLimitPerMinute: 60,
	}
}

func secretWith(name string, kv ...string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name}, Data: map[string][]byte{}}
	for i := 0; i+1 < len(kv); i += 2 {
		s.Data[kv[i]] = []byte(kv[i+1])
	}
	return s
}

// TestValidateAuthConfig guards the fail-fast configuration check (R-TEST-7): the
// controller refuses to start with auth settings that would break R-AUTH-3/-9.
func TestValidateAuthConfig(t *testing.T) {
	require.NoError(t, keys.ValidateAuthConfig(authConfig()))
	for name, mutate := range map[string]func(*config.Auth){
		"missing signing secret":    func(c *config.Auth) { c.SigningKeySecret = "" },
		"missing deny-list":         func(c *config.Auth) { c.DenyListConfigMap = "" },
		"TTL above 15 min":          func(c *config.Auth) { c.TokenTTL.Duration = 16 * time.Minute },
		"wrong audience":            func(c *config.Auth) { c.Audience = "cucina" },
		"publish lead under 10 min": func(c *config.Auth) { c.KeyRotationPublishLead.Duration = 5 * time.Minute },
		"negative rate limit":       func(c *config.Auth) { c.RateLimitPerMinute = -1 },
	} {
		c := authConfig()
		mutate(&c)
		assert.Error(t, keys.ValidateAuthConfig(c), name)
	}
}
