// SPDX-License-Identifier: FSL-1.1-ALv2

package imds_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/workeragent/imds"
	"github.com/sloper-ai/cucina/internal/workeragent/imds/imdsfake"
)

const doc = `{"accountId":"000000000000","architecture":"x86_64","availabilityZone":"us-west-1b",` +
	`"imageId":"ami-0123456789abcdef0","instanceId":"i-0123456789abcdef0","instanceType":"c7i.xlarge",` +
	`"pendingTime":"2026-10-02T10:00:00Z","privateIp":"192.0.2.10","region":"us-west-1","version":"2017-09-30"}`

// Guards: R-POOL-3/R-SEC-3 inputs — the agent reads its identity, boot data,
// tags and the Spot notice from IMDSv2 only (session token, no IMDSv1).
func TestClientAgainstFakeIMDS(t *testing.T) {
	ctx := context.Background()
	f := imdsfake.New(t)
	c := imds.New(f.URL)

	f.SetIdentity([]byte(doc), "MIAGCSqGSIb3DQEHAqCAMIACAQEx\nDzANBglghkgBZQMEAgEFADCABgkq\n", "i-0123456789abcdef0")
	raw, d, sig, err := c.Identity(ctx)
	require.NoError(t, err)
	require.JSONEq(t, doc, string(raw), "the raw document is passed through byte for byte")
	require.Equal(t, "i-0123456789abcdef0", d.InstanceID)
	require.Equal(t, "us-west-1", d.Region)
	require.Equal(t, time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC), d.PendingTime)
	require.Equal(t, "MIAGCSqGSIb3DQEHAqCAMIACAQExDzANBglghkgBZQMEAgEFADCABgkq", sig, "line breaks of the base64 PKCS#7 are removed")
	require.Equal(t, 1, f.GetRequests(imds.IdentitySignaturePath), "the RSA-2048 PKCS#7 form, never the RSA-1024 /signature")

	_, err = c.UserData(ctx)
	require.ErrorIs(t, err, imds.ErrNotFound)
	f.SetUserData([]byte(`{"cucinaBootData":1}`))
	ud, err := c.UserData(ctx)
	require.NoError(t, err)
	require.Equal(t, `{"cucinaBootData":1}`, string(ud))

	_, err = c.Tags(ctx, "cucina:")
	require.ErrorIs(t, err, imds.ErrNotFound, "instance-metadata tags disabled")
	f.SetTags(map[string]string{"cucina:pool": "linux-x86-64", "cucina:generation": "g7", "Name": "worker"})
	tags, err := c.Tags(ctx, "cucina:")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"cucina:pool": "linux-x86-64", "cucina:generation": "g7"}, tags)

	a, err := c.SpotInstanceAction(ctx)
	require.NoError(t, err)
	require.Nil(t, a, "no interruption pending")
	f.SetSpotAction([]byte(`{"action":"terminate","time":"2026-10-02T12:00:00Z"}`))
	a, err = c.SpotInstanceAction(ctx)
	require.NoError(t, err)
	require.Equal(t, "terminate", a.Action)

	require.Equal(t, 1, f.TokenRequests(), "one session token serves every request")
	f.ExpireTokens()
	id, err := c.InstanceID(ctx)
	require.NoError(t, err, "a 401 refreshes the token once")
	require.Equal(t, "i-0123456789abcdef0", id)

	f.FailNext("/latest/meta-data/instance-id", http.StatusServiceUnavailable)
	_, err = c.InstanceID(ctx)
	require.True(t, imds.IsTransient(err), "5xx is transient: %v", err)
	_, err = c.UserData(ctx)
	require.NoError(t, err)
	f.SetUserData(nil)
	_, err = c.UserData(ctx)
	require.False(t, imds.IsTransient(err), "404 is not transient")
}

// Guards: IMDS parsing of the documents the agent depends on.
func TestParse(t *testing.T) {
	_, err := imds.ParseIdentityDocument([]byte(`{"region":"us-west-1"}`))
	require.Error(t, err, "no instanceId")
	_, err = imds.ParseIdentityDocument([]byte(`<html>`))
	require.Error(t, err)
	_, err = imds.ParseInstanceAction([]byte(`{"time":"2026-10-02T12:00:00Z"}`))
	require.Error(t, err, "no action")
	require.Equal(t, []string{"a", "cucina:pool"}, imds.ParseTagKeys([]byte("a\n\ncucina:pool\n")))
}
