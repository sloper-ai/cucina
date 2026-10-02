// SPDX-License-Identifier: FSL-1.1-ALv2
#include "tools/hello/cc/greet.h"

#include <format>

namespace cucina::hello {

std::string Greet(std::string_view name) {
  return std::format("hello, {}", name.empty() ? std::string_view("world") : name);
}

}  // namespace cucina::hello
