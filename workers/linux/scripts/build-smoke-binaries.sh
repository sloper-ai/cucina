#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Builds the tiny foreign-architecture binaries for the qemu-user smoke test with a hermetic cross toolchain
# (zig cc, pinned in workers/linux/versions.json): hello-<arch>-static (musl, static) and hello-<arch>-dynamic
# (glibc 2.28, dynamically linked - needs the image's cross runtimes) for riscv64, s390x and armhf.
#   build-smoke-binaries.sh OUT_DIR
set -euo pipefail

out=${1:?usage: build-smoke-binaries.sh OUT_DIR}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
zig_version=$(jq -r '.pins.qemu_cross.zig_for_smoke_binaries' "$here/../versions.json")
mkdir -p "$out"
src="$out/hello.c"
cat >"$src" <<'C'
#include <stdio.h>
#include <sys/utsname.h>
int main(void) {
  struct utsname u;
  if (uname(&u) != 0) return 2;
  printf("hello from %s (uname -m: %s)\n", ARCH_NAME, u.machine);
  return 0;
}
C
build() { # arch static-target dynamic-target
  local arch=$1
  mise exec "zig@$zig_version" -- zig cc -target "$2" -static -O2 -DARCH_NAME="\"$arch\"" -o "$out/hello-$arch-static" "$src"
  mise exec "zig@$zig_version" -- zig cc -target "$3" -O2 -DARCH_NAME="\"$arch\"" -o "$out/hello-$arch-dynamic" "$src"
}
build riscv64 riscv64-linux-musl riscv64-linux-gnu.2.28
build s390x s390x-linux-musl s390x-linux-gnu.2.28
build armhf arm-linux-musleabihf arm-linux-gnueabihf.2.28
rm -f "$src"
file "$out"/hello-* | sed "s#$out/##"
