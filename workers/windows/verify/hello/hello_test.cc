// SPDX-License-Identifier: FSL-1.1-ALv2
#include <cstdio>
#include <filesystem>
#include <memory>

int main() {
  // C++17 library + runfiles directory exercise the CRT and the test runner (tw.exe) on Windows.
  auto p = std::make_unique<int>(42);
  std::filesystem::path cwd = std::filesystem::current_path();
  std::printf("cwd=%s value=%d\n", cwd.string().c_str(), *p);
  return *p == 42 ? 0 : 1;
}
