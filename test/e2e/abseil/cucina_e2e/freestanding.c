// SPDX-License-Identifier: FSL-1.1-ALv2
// Build-only check of targets without an OS (wasm, BPF): no libc, no C++
// runtime, bounded loops only (the BPF verifier's constraint).
#include "cucina_e2e/freestanding.h"

int cucina_e2e_add(int a, int b) { return a + b; }

unsigned cucina_e2e_checksum(const unsigned char *data, unsigned len) {
  unsigned sum = 0;
  for (unsigned i = 0; i < len && i < 64; i++) {
    sum = (sum << 1 | sum >> 31) ^ data[i];
  }
  return sum;
}
