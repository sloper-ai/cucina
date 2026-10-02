// SPDX-License-Identifier: FSL-1.1-ALv2
// Guards R-BUILD-1: the hermetic C++ toolchain (libc++, <format>) compiles,
// links and runs a test on the exec platform.
#include <cstdio>
#include <string_view>

#include "tools/hello/cc/greet.h"

int main() {
  struct Case {
    std::string_view name, want;
  };
  int failures = 0;
  for (const Case& c : {Case{"", "hello, world"}, Case{"cucina", "hello, cucina"}}) {
    if (const std::string got = cucina::hello::Greet(c.name); got != c.want) {
      std::fprintf(stderr, "Greet(%.*s) = %s, want %.*s\n", static_cast<int>(c.name.size()),
                   c.name.data(), got.c_str(), static_cast<int>(c.want.size()), c.want.data());
      ++failures;
    }
  }
  return failures == 0 ? 0 : 1;
}
