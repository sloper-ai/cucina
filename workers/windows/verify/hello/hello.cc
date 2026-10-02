// SPDX-License-Identifier: FSL-1.1-ALv2
#include <cstdio>
#include <string>

#include <windows.h>

// The SDK's own newest NTDDI value (sdkddkver.h), when this SDK defines it.
#ifdef WDK_NTDDI_VERSION
#define CUCINA_SDK_NTDDI static_cast<unsigned>(WDK_NTDDI_VERSION)
#else
#define CUCINA_SDK_NTDDI 0u
#endif

int main() {
  std::string greeting = "hello from rules_cc + MSVC";
  std::printf("%s _MSC_FULL_VER=%d _WIN32_WINNT=0x%04x WDK_NTDDI_VERSION=0x%08x\n", greeting.c_str(), _MSC_FULL_VER,
              _WIN32_WINNT, CUCINA_SDK_NTDDI);
  return 0;
}
