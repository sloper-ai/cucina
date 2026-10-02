// SPDX-License-Identifier: FSL-1.1-ALv2
#ifndef CUCINA_TOOLS_HELLO_CC_GREET_H_
#define CUCINA_TOOLS_HELLO_CC_GREET_H_

#include <string>
#include <string_view>

namespace cucina::hello {

// Returns "hello, <name>" ("hello, world" for an empty name).
std::string Greet(std::string_view name);

}  // namespace cucina::hello

#endif  // CUCINA_TOOLS_HELLO_CC_GREET_H_
