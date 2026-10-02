// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package config

/*
#cgo LDFLAGS: -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>

// cucina_copy_pref returns the value of key in the preference domain as XML
// plist data (NULL if unset) and whether it is forced by a managed profile.
// CFPreferencesCopyAppValue resolves the search list itself: forced managed
// preferences (/Library/Managed Preferences) win over local preferences.
static CFDataRef cucina_copy_pref(const char *domain, const char *key, int *forced) {
	CFStringRef d = CFStringCreateWithCString(kCFAllocatorDefault, domain, kCFStringEncodingUTF8);
	CFStringRef k = CFStringCreateWithCString(kCFAllocatorDefault, key, kCFStringEncodingUTF8);
	CFDataRef out = NULL;
	*forced = 0;
	if (d != NULL && k != NULL) {
		CFPropertyListRef v = CFPreferencesCopyAppValue(k, d);
		*forced = CFPreferencesAppValueIsForced(k, d) ? 1 : 0;
		if (v != NULL) {
			out = CFPropertyListCreateData(kCFAllocatorDefault, v, kCFPropertyListXMLFormat_v1_0, 0, NULL);
			CFRelease(v);
		}
	}
	if (k != NULL) CFRelease(k);
	if (d != NULL) CFRelease(d);
	return out;
}

static void cucina_sync_prefs(const char *domain) {
	CFStringRef d = CFStringCreateWithCString(kCFAllocatorDefault, domain, kCFStringEncodingUTF8);
	if (d != NULL) {
		CFPreferencesAppSynchronize(d);
		CFRelease(d);
	}
}
*/
import "C"

import (
	"unsafe"

	"howett.net/plist"
)

// CFSource reads preference values through CFPreferences (root daemon mode).
// Keys() comes from the managed plist file because CFPreferences cannot
// enumerate managed keys.
type CFSource struct {
	Domain string
	// KeyLister supplies Keys() (normally the managed plist file).
	KeyLister Source
}

// CFAvailable reports whether the CFPreferences reader is compiled in.
const CFAvailable = true

// NewCFSource returns a CFPreferences-backed source for domain.
func NewCFSource(domain string, keys Source) Source {
	cd := C.CString(domain)
	defer C.free(unsafe.Pointer(cd))
	C.cucina_sync_prefs(cd)
	return &CFSource{Domain: domain, KeyLister: keys}
}

// Value implements Source.
func (s *CFSource) Value(key string) (any, bool, bool) {
	cd := C.CString(s.Domain)
	defer C.free(unsafe.Pointer(cd))
	ck := C.CString(key)
	defer C.free(unsafe.Pointer(ck))
	var forced C.int
	data := C.cucina_copy_pref(cd, ck, &forced)
	if data == 0 {
		return nil, false, false
	}
	defer C.CFRelease(C.CFTypeRef(data))
	n := C.CFDataGetLength(data)
	b := C.GoBytes(unsafe.Pointer(C.CFDataGetBytePtr(data)), C.int(n))
	v, err := valueFromXML(b)
	if err != nil {
		return nil, false, false
	}
	return v, forced != 0, true
}

// Keys implements Source.
func (s *CFSource) Keys() []string {
	if s.KeyLister == nil {
		return nil
	}
	return s.KeyLister.Keys()
}

// valueFromXML decodes a single plist value serialised by CFPropertyListCreateData.
func valueFromXML(data []byte) (any, error) {
	var v any
	if _, err := plist.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v, nil
}
