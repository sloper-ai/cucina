// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package identity

/*
#cgo LDFLAGS: -framework CoreFoundation -framework Security
#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>
#include <stdlib.h>

// cucina_find_identity looks up one identity (certificate + private key) by the
// certificate's label in the keychain search list (the System keychain for root).
static OSStatus cucina_find_identity(const char *label, SecIdentityRef *out) {
	CFStringRef l = CFStringCreateWithCString(kCFAllocatorDefault, label, kCFStringEncodingUTF8);
	if (l == NULL) return errSecParam;
	const void *keys[] = { kSecClass, kSecAttrLabel, kSecReturnRef, kSecMatchLimit };
	const void *vals[] = { kSecClassIdentity, l, kCFBooleanTrue, kSecMatchLimitOne };
	CFDictionaryRef q = CFDictionaryCreate(kCFAllocatorDefault, keys, vals, 4,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFTypeRef res = NULL;
	OSStatus st = SecItemCopyMatching(q, &res);
	CFRelease(q);
	CFRelease(l);
	if (st == errSecSuccess) *out = (SecIdentityRef)res;
	return st;
}

static CFDataRef cucina_identity_cert(SecIdentityRef id) {
	SecCertificateRef c = NULL;
	if (SecIdentityCopyCertificate(id, &c) != errSecSuccess) return NULL;
	CFDataRef d = SecCertificateCopyData(c);
	CFRelease(c);
	return d;
}

static SecKeyRef cucina_identity_key(SecIdentityRef id) {
	SecKeyRef k = NULL;
	if (SecIdentityCopyPrivateKey(id, &k) != errSecSuccess) return NULL;
	return k;
}

// alg: 0 ECDSA-SHA256, 1 ECDSA-SHA384, 2 RSA-PKCS1-SHA256, 3 RSA-PKCS1-SHA384, 4 RSA-PSS-SHA256, 5 RSA-PSS-SHA384.
static CFDataRef cucina_sign(SecKeyRef k, int alg, const void *digest, long n) {
	SecKeyAlgorithm a;
	switch (alg) {
	case 0: a = kSecKeyAlgorithmECDSASignatureDigestX962SHA256; break;
	case 1: a = kSecKeyAlgorithmECDSASignatureDigestX962SHA384; break;
	case 2: a = kSecKeyAlgorithmRSASignatureDigestPKCS1v15SHA256; break;
	case 3: a = kSecKeyAlgorithmRSASignatureDigestPKCS1v15SHA384; break;
	case 4: a = kSecKeyAlgorithmRSASignatureDigestPSSSHA256; break;
	default: a = kSecKeyAlgorithmRSASignatureDigestPSSSHA384; break;
	}
	CFDataRef d = CFDataCreate(kCFAllocatorDefault, digest, n);
	CFErrorRef err = NULL;
	CFDataRef sig = SecKeyCreateSignature(k, a, d, &err);
	CFRelease(d);
	if (err != NULL) CFRelease(err);
	return sig;
}

static void cucina_release(CFTypeRef r) { if (r != NULL) CFRelease(r); }
*/
import "C"

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"runtime"
	"unsafe"
)

// KeychainAvailable reports whether keychain identities can be read.
const KeychainAvailable = true

// keySigner signs with a non-extractable keychain key (SecKeyCreateSignature).
type keySigner struct {
	key C.SecKeyRef
	pub crypto.PublicKey
}

func (s *keySigner) Public() crypto.PublicKey { return s.pub }

func (s *keySigner) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	alg := -1
	_, pss := opts.(*rsa.PSSOptions)
	switch s.pub.(type) {
	case *ecdsa.PublicKey:
		switch opts.HashFunc() {
		case crypto.SHA256:
			alg = 0
		case crypto.SHA384:
			alg = 1
		}
	case *rsa.PublicKey:
		switch {
		case opts.HashFunc() == crypto.SHA256 && !pss:
			alg = 2
		case opts.HashFunc() == crypto.SHA384 && !pss:
			alg = 3
		case opts.HashFunc() == crypto.SHA256 && pss:
			alg = 4
		case opts.HashFunc() == crypto.SHA384 && pss:
			alg = 5
		}
	}
	if alg < 0 || len(digest) == 0 {
		return nil, fmt.Errorf("keychain identity: unsupported signature (%T, %v)", s.pub, opts.HashFunc())
	}
	sig := C.cucina_sign(s.key, C.int(alg), unsafe.Pointer(&digest[0]), C.long(len(digest)))
	if sig == 0 {
		return nil, errors.New("keychain identity: SecKeyCreateSignature failed")
	}
	defer C.cucina_release(C.CFTypeRef(sig))
	return C.GoBytes(unsafe.Pointer(C.CFDataGetBytePtr(sig)), C.int(C.CFDataGetLength(sig))), nil
}

// KeychainIdentity loads an MDM-issued (ACME/SCEP, non-hardware-bound) identity
// whose certificate label is label from the keychain search list — the System
// keychain for the root daemon (TN3137: daemons cannot use the data-protection
// keychain). The private key never leaves the keychain.
func KeychainIdentity(label string) (*tls.Certificate, error) {
	cl := C.CString(label)
	defer C.free(unsafe.Pointer(cl))
	var id C.SecIdentityRef
	if st := C.cucina_find_identity(cl, &id); st != C.errSecSuccess {
		return nil, fmt.Errorf("keychain identity %q not found (OSStatus %d)", label, int(st))
	}
	defer C.cucina_release(C.CFTypeRef(id))
	certData := C.cucina_identity_cert(id)
	if certData == 0 {
		return nil, fmt.Errorf("keychain identity %q has no certificate", label)
	}
	der := C.GoBytes(unsafe.Pointer(C.CFDataGetBytePtr(certData)), C.int(C.CFDataGetLength(certData)))
	C.cucina_release(C.CFTypeRef(certData))
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	key := C.cucina_identity_key(id)
	if key == 0 {
		return nil, fmt.Errorf("keychain identity %q: private key not accessible", label)
	}
	s := &keySigner{key: key, pub: leaf.PublicKey}
	runtime.SetFinalizer(s, func(s *keySigner) { C.cucina_release(C.CFTypeRef(s.key)) })
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: s, Leaf: leaf}, nil
}
