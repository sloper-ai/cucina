// SPDX-License-Identifier: FSL-1.1-ALv2
// Build-only check of targets without an OS (wasm, BPF): no libc headers.
#ifndef CUCINA_E2E_FREESTANDING_H_
#define CUCINA_E2E_FREESTANDING_H_

int cucina_e2e_add(int a, int b);
unsigned cucina_e2e_checksum(const unsigned char *data, unsigned len);

#endif  // CUCINA_E2E_FREESTANDING_H_
