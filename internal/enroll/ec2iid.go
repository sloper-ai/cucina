// SPDX-License-Identifier: FSL-1.1-ALv2

package enroll

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrIdentityDocument is returned when an instance identity document or its
// signature does not verify.
var ErrIdentityDocument = errors.New("instance identity document rejected")

// MaxIdentityBytes bounds the document and signature sizes accepted from the network.
const MaxIdentityBytes = 16 << 10

// IdentityDocument is the signed EC2 instance identity document (fields Cucina uses).
type IdentityDocument struct {
	AccountID        string    `json:"accountId"`
	Architecture     string    `json:"architecture"`
	AvailabilityZone string    `json:"availabilityZone"`
	ImageID          string    `json:"imageId"`
	InstanceID       string    `json:"instanceId"`
	InstanceType     string    `json:"instanceType"`
	PendingTime      time.Time `json:"pendingTime"`
	PrivateIP        string    `json:"privateIp"`
	Region           string    `json:"region"`
	Version          string    `json:"version"`
}

// IdentityVerifier verifies EC2 instance identity documents against the AWS
// RSA-2048 public certificate of the document's Region (R-SEC-3).
type IdentityVerifier struct {
	keys map[string]*rsa.PublicKey
}

// NewIdentityVerifier returns a verifier for the embedded certificates of every
// commercial AWS Region.
func NewIdentityVerifier() (*IdentityVerifier, error) {
	certs := map[string][]byte{}
	for r, p := range awsRSA2048Certs {
		certs[r] = []byte(p)
	}
	return NewIdentityVerifierWithCerts(certs)
}

// NewIdentityVerifierWithCerts returns a verifier for explicit Region -> PEM
// certificates (tests; other AWS partitions).
func NewIdentityVerifierWithCerts(certs map[string][]byte) (*IdentityVerifier, error) {
	v := &IdentityVerifier{keys: map[string]*rsa.PublicKey{}}
	for region, p := range certs {
		block, _ := pem.Decode(p)
		if block == nil {
			return nil, fmt.Errorf("enroll: AWS certificate for %s is not PEM", region)
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("enroll: AWS certificate for %s: %w", region, err)
		}
		pub, ok := c.PublicKey.(*rsa.PublicKey)
		if !ok || pub.N.BitLen() != 2048 {
			return nil, fmt.Errorf("enroll: AWS certificate for %s is not RSA-2048", region)
		}
		v.keys[region] = pub
	}
	return v, nil
}

// Regions lists the Regions the verifier has certificates for.
func (v *IdentityVerifier) Regions() []string {
	out := make([]string, 0, len(v.keys))
	for r := range v.keys {
		out = append(out, r)
	}
	return out
}

// Verify checks the base64 PKCS#7 RSA-2048 signature (the body of IMDS
// /latest/dynamic/instance-identity/rsa2048) and returns the signed document.
// document, when non-empty, must be byte-identical to the signed content
// (prevents pairing a genuine signature with a forged document).
func (v *IdentityVerifier) Verify(document []byte, signature string) (*IdentityDocument, error) {
	if len(document) > MaxIdentityBytes || len(signature) > 2*MaxIdentityBytes {
		return nil, fmt.Errorf("%w: too large", ErrIdentityDocument)
	}
	sigDER, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(signature), ""))
	if err != nil {
		return nil, fmt.Errorf("%w: signature is not base64: %w", ErrIdentityDocument, err)
	}
	if len(sigDER) == 0 || sigDER[0] != 0x30 {
		return nil, fmt.Errorf("%w: signature must be the PKCS#7 body of /latest/dynamic/instance-identity/rsa2048 (the RSA-1024 /signature form is not accepted)", ErrIdentityDocument)
	}
	// The Region selects the certificate; it is re-checked after verification
	// because only the signed content is trusted.
	unverified := document
	if len(unverified) == 0 {
		if unverified, err = pkcs7Content(sigDER); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrIdentityDocument, err)
		}
	}
	var hint struct {
		Region string `json:"region"`
	}
	if err := json.Unmarshal(unverified, &hint); err != nil {
		return nil, fmt.Errorf("%w: document is not JSON: %w", ErrIdentityDocument, err)
	}
	pub, ok := v.keys[hint.Region]
	if !ok {
		return nil, fmt.Errorf("%w: no AWS certificate for Region %q", ErrIdentityDocument, hint.Region)
	}
	content, err := verifyPKCS7(sigDER, pub, document)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrIdentityDocument, err)
	}
	if len(document) > 0 && !bytes.Equal(content, document) {
		return nil, fmt.Errorf("%w: document differs from the signed content", ErrIdentityDocument)
	}
	var doc IdentityDocument
	dec := json.NewDecoder(bytes.NewReader(content))
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: signed content is not an identity document: %w", ErrIdentityDocument, err)
	}
	if doc.Region != hint.Region {
		return nil, fmt.Errorf("%w: Region changed between hint and signed content", ErrIdentityDocument)
	}
	return &doc, nil
}

// pkcs7Content extracts the (unverified) content of a SignedData.
func pkcs7Content(der []byte) ([]byte, error) {
	der, err := berToDER(der)
	if err != nil {
		return nil, err
	}
	var ci p7ContentInfo
	if _, err := asn1.Unmarshal(der, &ci); err != nil {
		return nil, err
	}
	var sd p7SignedData
	if _, err := asn1.Unmarshal(ci.Content.Bytes, &sd); err != nil {
		return nil, err
	}
	var octets []byte
	if _, err := asn1.Unmarshal(sd.EncapContentInfo.Content.Bytes, &octets); err != nil {
		return nil, errors.New("signature carries no document; send the document too")
	}
	return octets, nil
}
