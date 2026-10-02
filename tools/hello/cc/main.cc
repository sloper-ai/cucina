// SPDX-License-Identifier: FSL-1.1-ALv2
// C/C++ slice of the build-system smoke test (R-BUILD-1): hermetic-llvm builds
// this for macOS arm64 natively and cross-compiles it for Linux (glibc and musl).
#include <cstdio>

#include "tools/hello/cc/greet.h"

int main(int argc, char** argv) {
  std::puts(cucina::hello::Greet(argc > 1 ? argv[1] : "").c_str());
  return 0;
}
