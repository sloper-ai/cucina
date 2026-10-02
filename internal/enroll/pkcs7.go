// SPDX-License-Identifier: FSL-1.1-ALv2

package enroll

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
)

// Minimal PKCS#7 / CMS SignedData verification for the EC2 instance identity
// document's RSA-2048 signature (IMDS …/instance-identity/rsa2048). Only what
// AWS produces is accepted: one signer, SHA-256, RSA PKCS#1 v1.5, content type
// id-data, optional signed attributes (then contentType and messageDigest are
// required). Everything else fails closed.

var (
	oidSignedData       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	oidData             = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
	oidSHA256           = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidRSAEncryption    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
	oidSHA256WithRSA    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}
	oidAttrContentType  = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3}
	oidAttrMessageDigst = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}
)

type p7ContentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue `asn1:"explicit,optional,tag:0"`
}

type p7SignedData struct {
	Version          int
	DigestAlgorithms []pkix.AlgorithmIdentifier `asn1:"set"`
	EncapContentInfo p7ContentInfo
	Certificates     asn1.RawValue  `asn1:"optional,tag:0"`
	CRLs             asn1.RawValue  `asn1:"optional,tag:1"`
	SignerInfos      []p7SignerInfo `asn1:"set"`
}

type p7IssuerAndSerial struct {
	Issuer       asn1.RawValue
	SerialNumber *big.Int
}

type p7SignerInfo struct {
	Version            int
	Sid                p7IssuerAndSerial
	DigestAlgorithm    pkix.AlgorithmIdentifier
	SignedAttrs        asn1.RawValue `asn1:"optional,tag:0"`
	SignatureAlgorithm pkix.AlgorithmIdentifier
	Signature          []byte
	UnsignedAttrs      asn1.RawValue `asn1:"optional,tag:1"`
}

type p7Attribute struct {
	Type   asn1.ObjectIdentifier
	Values asn1.RawValue `asn1:"set"`
}

// verifyPKCS7 verifies a DER/BER PKCS#7 SignedData with pub and returns the
// signed content. detached is used when the SignedData carries no content.
func verifyPKCS7(der []byte, pub *rsa.PublicKey, detached []byte) ([]byte, error) {
	der, err := berToDER(der)
	if err != nil {
		return nil, fmt.Errorf("pkcs7: %w", err)
	}
	var ci p7ContentInfo
	if err := unmarshalExact(der, &ci); err != nil {
		return nil, fmt.Errorf("pkcs7: malformed ContentInfo: %w", err)
	}
	if !ci.ContentType.Equal(oidSignedData) {
		return nil, fmt.Errorf("pkcs7: content type %s is not signedData", ci.ContentType)
	}
	var sd p7SignedData
	if err := unmarshalExact(ci.Content.Bytes, &sd); err != nil {
		return nil, fmt.Errorf("pkcs7: malformed SignedData: %w", err)
	}
	if !sd.EncapContentInfo.ContentType.Equal(oidData) {
		return nil, fmt.Errorf("pkcs7: encapsulated content type %s is not data", sd.EncapContentInfo.ContentType)
	}
	content := detached
	if len(sd.EncapContentInfo.Content.FullBytes) > 0 {
		var octets []byte
		// Content is the EXPLICIT [0]; its Bytes hold the OCTET STRING element.
		if err := unmarshalExact(sd.EncapContentInfo.Content.Bytes, &octets); err != nil {
			return nil, fmt.Errorf("pkcs7: encapsulated content is not an OCTET STRING: %w", err)
		}
		content = octets
	}
	if len(content) == 0 {
		return nil, errors.New("pkcs7: no content")
	}
	if len(sd.SignerInfos) != 1 {
		return nil, fmt.Errorf("pkcs7: %d signers, expected exactly 1", len(sd.SignerInfos))
	}
	si := sd.SignerInfos[0]
	if !si.DigestAlgorithm.Algorithm.Equal(oidSHA256) {
		return nil, fmt.Errorf("pkcs7: digest algorithm %s is not SHA-256", si.DigestAlgorithm.Algorithm)
	}
	if a := si.SignatureAlgorithm.Algorithm; !a.Equal(oidRSAEncryption) && !a.Equal(oidSHA256WithRSA) {
		return nil, fmt.Errorf("pkcs7: signature algorithm %s is not RSA", a)
	}
	contentDigest := sha256.Sum256(content)
	signed := contentDigest[:]
	if len(si.SignedAttrs.FullBytes) > 0 {
		if err := checkSignedAttrs(si.SignedAttrs.Bytes, contentDigest[:]); err != nil {
			return nil, err
		}
		// RFC 5652 §5.4: the signature covers the DER of the attributes with the
		// IMPLICIT [0] tag replaced by the SET OF tag.
		attrs := append([]byte{0x31}, si.SignedAttrs.FullBytes[1:]...)
		sum := sha256.Sum256(attrs)
		signed = sum[:]
	}
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, signed, si.Signature); err != nil {
		return nil, fmt.Errorf("pkcs7: signature verification failed: %w", err)
	}
	return content, nil
}

func checkSignedAttrs(b, contentDigest []byte) error {
	var sawType, sawDigest bool
	for len(b) > 0 {
		var a p7Attribute
		rest, err := asn1.Unmarshal(b, &a)
		if err != nil {
			return fmt.Errorf("pkcs7: malformed signed attribute: %w", err)
		}
		b = rest
		switch {
		case a.Type.Equal(oidAttrContentType):
			var oid asn1.ObjectIdentifier
			if r, err := asn1.Unmarshal(a.Values.Bytes, &oid); err != nil || len(r) != 0 || !oid.Equal(oidData) {
				return errors.New("pkcs7: signed contentType attribute is not data")
			}
			sawType = true
		case a.Type.Equal(oidAttrMessageDigst):
			var d []byte
			if r, err := asn1.Unmarshal(a.Values.Bytes, &d); err != nil || len(r) != 0 {
				return errors.New("pkcs7: malformed messageDigest attribute")
			}
			if subtle.ConstantTimeCompare(d, contentDigest) != 1 {
				return errors.New("pkcs7: messageDigest does not match the content")
			}
			sawDigest = true
		}
	}
	if !sawType || !sawDigest {
		return errors.New("pkcs7: signed attributes lack contentType or messageDigest")
	}
	return nil
}

// ---------------------------------------------------------------- BER -> DER

const maxBERDepth = 32

// berToDER rewrites a BER encoding as DER where it matters for parsing with
// encoding/asn1: indefinite lengths become definite (minimal) lengths and
// constructed OCTET STRINGs are flattened into primitive ones. Already-DER
// input is returned byte-identical.
func berToDER(b []byte) ([]byte, error) {
	out, rest, err := berNode(b, 0)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, errors.New("trailing data after the top-level element")
	}
	return out, nil
}

func berNode(b []byte, depth int) (der, rest []byte, err error) {
	if depth > maxBERDepth {
		return nil, nil, errors.New("BER nesting too deep")
	}
	if len(b) < 2 {
		return nil, nil, errors.New("truncated BER element")
	}
	i := 1
	if b[0]&0x1f == 0x1f { // high-tag-number form
		for {
			if i >= len(b) || i > 5 {
				return nil, nil, errors.New("malformed BER tag")
			}
			c := b[i]
			i++
			if c&0x80 == 0 {
				break
			}
		}
	}
	tag := b[:i]
	constructed := b[0]&0x20 != 0
	if i >= len(b) {
		return nil, nil, errors.New("truncated BER length")
	}
	lb := b[i]
	i++
	var children [][]byte
	var content []byte
	switch lb {
	case 0x80: // indefinite length
		if !constructed {
			return nil, nil, errors.New("indefinite length on a primitive BER element")
		}
		cur := b[i:]
		for {
			if len(cur) < 2 {
				return nil, nil, errors.New("missing BER end-of-contents")
			}
			if cur[0] == 0 && cur[1] == 0 {
				cur = cur[2:]
				break
			}
			child, r, err := berNode(cur, depth+1)
			if err != nil {
				return nil, nil, err
			}
			children = append(children, child)
			cur = r
		}
		rest = cur
	default:
		n := int(lb)
		if lb&0x80 != 0 {
			k := int(lb & 0x7f)
			if k == 0 || k > 4 || i+k > len(b) {
				return nil, nil, errors.New("malformed BER length")
			}
			n = 0
			for _, c := range b[i : i+k] {
				n = n<<8 | int(c)
			}
			i += k
		}
		if n < 0 || i+n > len(b) {
			return nil, nil, errors.New("BER length exceeds input")
		}
		raw := b[i : i+n]
		rest = b[i+n:]
		if !constructed {
			content = raw
			break
		}
		for len(raw) > 0 {
			child, r, err := berNode(raw, depth+1)
			if err != nil {
				return nil, nil, err
			}
			children = append(children, child)
			raw = r
		}
	}
	if constructed {
		if len(tag) == 1 && tag[0] == 0x24 { // constructed universal OCTET STRING -> primitive
			var payload []byte
			for _, c := range children {
				if c[0] != 0x04 {
					return nil, nil, errors.New("constructed OCTET STRING with a non-OCTET STRING segment")
				}
				_, body, err := splitDER(c)
				if err != nil {
					return nil, nil, err
				}
				payload = append(payload, body...)
			}
			return encodeTLV([]byte{0x04}, payload), rest, nil
		}
		content = bytes.Join(children, nil)
	}
	return encodeTLV(tag, content), rest, nil
}

func encodeTLV(tag, content []byte) []byte {
	out := append([]byte(nil), tag...)
	n := len(content)
	switch {
	case n < 0x80:
		out = append(out, byte(n))
	default:
		var l []byte
		for v := n; v > 0; v >>= 8 {
			l = append([]byte{byte(v)}, l...)
		}
		out = append(out, 0x80|byte(len(l)))
		out = append(out, l...)
	}
	return append(out, content...)
}

// splitDER returns the header and body of one DER element.
func splitDER(b []byte) (header, body []byte, err error) {
	var rv asn1.RawValue
	if err := unmarshalExact(b, &rv); err != nil {
		return nil, nil, fmt.Errorf("malformed DER element: %w", err)
	}
	return b[:len(b)-len(rv.Bytes)], rv.Bytes, nil
}

// unmarshalExact parses exactly one DER element filling all of b.
func unmarshalExact(b []byte, v any) error {
	rest, err := asn1.Unmarshal(b, v)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return errors.New("trailing data")
	}
	return nil
}
