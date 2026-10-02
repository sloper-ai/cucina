# SPDX-License-Identifier: FSL-1.1-ALv2
"""Pinned third-party tool binaries (R-BUILD-1, R-BUILD-2; spec §0.5 "pin everything").

Every tool is fetched by URL + checksum (SHA-256; SHA-512 integrity for envtest,
as published in controller-tools' envtest-releases.yaml) for darwin_arm64,
linux_amd64, linux_arm64 and windows_amd64. A hub repository per tool selects
the binary for the platform the target is configured for, so the same label
works as `sh_test` data (target platform) and as a `genrule` tool (exec
platform):

    @helm//:helm                 @kubeconform//:kubeconform   @buf//:buf
    @helm_unittest//:plugin      (files; @helm_unittest//:plugin.yaml is the manifest,
                                  HELM_PLUGINS = dirname(dirname(<its path>)))
    @controller_gen//:controller-gen   @gitleaks//:gitleaks   @promtool//:promtool
    @tflint//:tflint             @opentofu//:tofu
    @protoc_gen_buffa//:protoc-gen-buffa   @protoc_gen_buffa_packaging//:protoc-gen-buffa-packaging
    @protoc_gen_connect_rust//:protoc-gen-connect-rust
    @envtest//:binaries          (etcd, kube-apiserver, kubectl; see //bazel:envtest.bzl)
    @bb_release//:bb_storage     @bb_release//:bb_scheduler
    @bb_release//:bb_worker      @bb_release//:bb_runner

Short aliases live in //tools (e.g. //tools:helm). Bump a tool by editing the
tables below (re-verify checksums from the upstream release); the extension is
reproducible, so MODULE.bazel.lock does not change.
"""

load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive", "http_file")

_PLATFORMS = {
    "darwin_arm64": ("@platforms//os:macos", "@platforms//cpu:aarch64"),
    "linux_amd64": ("@platforms//os:linux", "@platforms//cpu:x86_64"),
    "linux_arm64": ("@platforms//os:linux", "@platforms//cpu:aarch64"),
    "windows_amd64": ("@platforms//os:windows", "@platforms//cpu:x86_64"),
}

# kind: "file" (raw binary), "tar.gz" or "zip"; path: binary inside the archive
# after strip_prefix. helm_unittest is a Helm plugin directory, not one binary.
_TOOLS = {
    "helm": struct(
        version = "v4.3.0",
        target = "helm",
        platforms = {
            "darwin_arm64": struct(url = "https://get.helm.sh/helm-v4.3.0-darwin-arm64.tar.gz", sha256 = "d3870437e1e95b67f8edbde964156c84a26503f560821d40c542441658934fba", kind = "tar.gz", strip_prefix = "darwin-arm64", path = "helm"),
            "linux_amd64": struct(url = "https://get.helm.sh/helm-v4.3.0-linux-amd64.tar.gz", sha256 = "86584a54def73570558f66f5111cc53dfed56689637ae32c1201205d494f54fb", kind = "tar.gz", strip_prefix = "linux-amd64", path = "helm"),
            "linux_arm64": struct(url = "https://get.helm.sh/helm-v4.3.0-linux-arm64.tar.gz", sha256 = "31c5794dd55c66a51e6b7d2e2ac7a114ae8b1de41ff1d9ba51748ac973b06a08", kind = "tar.gz", strip_prefix = "linux-arm64", path = "helm"),
            "windows_amd64": struct(url = "https://get.helm.sh/helm-v4.3.0-windows-amd64.zip", sha256 = "304ea163cce4d9ad14e189c01846c6a34de9cfdfe48536ae54b2e8ba7884e67c", kind = "zip", strip_prefix = "windows-amd64", path = "helm.exe"),
        },
    ),
    "helm_unittest": struct(
        version = "v1.2.0",
        target = None,
        platforms = {
            "darwin_arm64": struct(url = "https://github.com/helm-unittest/helm-unittest/releases/download/v1.2.0/helm-unittest-macos-arm64-1.2.0.tgz", sha256 = "f9d1ea0a25455b8aa387865486dd9f49355b600ac875ac504c662a19d18fe09c", kind = "tar.gz", strip_prefix = "", path = "untt-macos-arm64"),
            "linux_amd64": struct(url = "https://github.com/helm-unittest/helm-unittest/releases/download/v1.2.0/helm-unittest-linux-amd64-1.2.0.tgz", sha256 = "115c690234847d316f0a814beb9cceaf9c21bb407173179c0e82076bdce1efc0", kind = "tar.gz", strip_prefix = "", path = "untt-linux-amd64"),
            "linux_arm64": struct(url = "https://github.com/helm-unittest/helm-unittest/releases/download/v1.2.0/helm-unittest-linux-arm64-1.2.0.tgz", sha256 = "4a5cb6b35773734fd438c841e2553f3dbed2206cc027e94e4b093a43652cf888", kind = "tar.gz", strip_prefix = "", path = "untt-linux-arm64"),
            "windows_amd64": struct(url = "https://github.com/helm-unittest/helm-unittest/releases/download/v1.2.0/helm-unittest-windows-amd64-1.2.0.tgz", sha256 = "f15c260b20cb316553eddc34c0792872f5559740ca456f3df33cabf353f47ea1", kind = "tar.gz", strip_prefix = "", path = "untt-windows-amd64.exe"),
        },
    ),
    "kubeconform": struct(
        version = "v0.8.0",
        target = "kubeconform",
        platforms = {
            "darwin_arm64": struct(url = "https://github.com/yannh/kubeconform/releases/download/v0.8.0/kubeconform-darwin-arm64.tar.gz", sha256 = "f84f4dfbebf4a6b0b230385fa065a39ea35e02608c2b50d025dcf64775a69d67", kind = "tar.gz", strip_prefix = "", path = "kubeconform"),
            "linux_amd64": struct(url = "https://github.com/yannh/kubeconform/releases/download/v0.8.0/kubeconform-linux-amd64.tar.gz", sha256 = "9bc2bffbf71f261128533edaf912153948b7ff238f9a531ae6d34466ec287883", kind = "tar.gz", strip_prefix = "", path = "kubeconform"),
            "linux_arm64": struct(url = "https://github.com/yannh/kubeconform/releases/download/v0.8.0/kubeconform-linux-arm64.tar.gz", sha256 = "1f53fc8e81258197a35e8603054162a5af1de8c5af13746c71ab680d9534ed87", kind = "tar.gz", strip_prefix = "", path = "kubeconform"),
            "windows_amd64": struct(url = "https://github.com/yannh/kubeconform/releases/download/v0.8.0/kubeconform-windows-amd64.zip", sha256 = "e3f56102bcf4f50b034a567e2482a1c5330799983ddd655952310211aef73d93", kind = "zip", strip_prefix = "", path = "kubeconform.exe"),
        },
    ),
    "buf": struct(
        version = "v1.73.0",
        target = "buf",
        platforms = {
            "darwin_arm64": struct(url = "https://github.com/bufbuild/buf/releases/download/v1.73.0/buf-Darwin-arm64", sha256 = "6e6df0fef4522e4e43dfe7c341873c3f2c29ceb45a9dfa5e0bad5580b8b2022f", kind = "file", strip_prefix = "", path = "buf-Darwin-arm64"),
            "linux_amd64": struct(url = "https://github.com/bufbuild/buf/releases/download/v1.73.0/buf-Linux-x86_64", sha256 = "8f2986298ad08f0cc1bf999b9797b7c383adf32d7edf0f73d6f1e1a701baeac1", kind = "file", strip_prefix = "", path = "buf-Linux-x86_64"),
            "linux_arm64": struct(url = "https://github.com/bufbuild/buf/releases/download/v1.73.0/buf-Linux-aarch64", sha256 = "902b75267db7f4391e99b7fa0756050e5354234cc0437ef50eee9c788950c7a3", kind = "file", strip_prefix = "", path = "buf-Linux-aarch64"),
            "windows_amd64": struct(url = "https://github.com/bufbuild/buf/releases/download/v1.73.0/buf-Windows-x86_64.exe", sha256 = "13542f2892c4f774150ddb525266d6421d457b3e741297056b64427853526e36", kind = "file", strip_prefix = "", path = "buf-Windows-x86_64.exe"),
        },
    ),
    "controller_gen": struct(
        version = "v0.21.0",
        target = "controller-gen",
        platforms = {
            "darwin_arm64": struct(url = "https://github.com/kubernetes-sigs/controller-tools/releases/download/v0.21.0/controller-gen-darwin-arm64", sha256 = "13ac7abcd90dd2129da972024bea7da5be7df2c5404c8f715010490996f4ab43", kind = "file", strip_prefix = "", path = "controller-gen-darwin-arm64"),
            "linux_amd64": struct(url = "https://github.com/kubernetes-sigs/controller-tools/releases/download/v0.21.0/controller-gen-linux-amd64", sha256 = "3ec7994b7a41515a7ad8aef8e517429c0a78060df59d5587ac8662db6a8b035f", kind = "file", strip_prefix = "", path = "controller-gen-linux-amd64"),
            "linux_arm64": struct(url = "https://github.com/kubernetes-sigs/controller-tools/releases/download/v0.21.0/controller-gen-linux-arm64", sha256 = "1af64bb144658c61f1b61c6392e17d56dd4f7364497625613ff435d7e468f60b", kind = "file", strip_prefix = "", path = "controller-gen-linux-arm64"),
            "windows_amd64": struct(url = "https://github.com/kubernetes-sigs/controller-tools/releases/download/v0.21.0/controller-gen-windows-amd64.exe", sha256 = "a49ec014273de590ea1d898179fe5c07876b4bdb84f6b9b3474fab6dc43c554a", kind = "file", strip_prefix = "", path = "controller-gen-windows-amd64.exe"),
        },
    ),
    "gitleaks": struct(
        version = "v8.30.1",
        target = "gitleaks",
        platforms = {
            "darwin_arm64": struct(url = "https://github.com/gitleaks/gitleaks/releases/download/v8.30.1/gitleaks_8.30.1_darwin_arm64.tar.gz", sha256 = "b40ab0ae55c505963e365f271a8d3846efbc170aa17f2607f13df610a9aeb6a5", kind = "tar.gz", strip_prefix = "", path = "gitleaks"),
            "linux_amd64": struct(url = "https://github.com/gitleaks/gitleaks/releases/download/v8.30.1/gitleaks_8.30.1_linux_x64.tar.gz", sha256 = "551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb", kind = "tar.gz", strip_prefix = "", path = "gitleaks"),
            "linux_arm64": struct(url = "https://github.com/gitleaks/gitleaks/releases/download/v8.30.1/gitleaks_8.30.1_linux_arm64.tar.gz", sha256 = "e4a487ee7ccd7d3a7f7ec08657610aa3606637dab924210b3aee62570fb4b080", kind = "tar.gz", strip_prefix = "", path = "gitleaks"),
            "windows_amd64": struct(url = "https://github.com/gitleaks/gitleaks/releases/download/v8.30.1/gitleaks_8.30.1_windows_x64.zip", sha256 = "d29144deff3a68aa93ced33dddf84b7fdc26070add4aa0f4513094c8332afc4e", kind = "zip", strip_prefix = "", path = "gitleaks.exe"),
        },
    ),
    "promtool": struct(
        version = "v3.15.0",
        target = "promtool",
        platforms = {
            "darwin_arm64": struct(url = "https://github.com/prometheus/prometheus/releases/download/v3.15.0/prometheus-3.15.0.darwin-arm64.tar.gz", sha256 = "920df4d17e78b3b0175af144eb318b0c74d1cf7b1d1251b326966f0e81977260", kind = "tar.gz", strip_prefix = "prometheus-3.15.0.darwin-arm64", path = "promtool"),
            "linux_amd64": struct(url = "https://github.com/prometheus/prometheus/releases/download/v3.15.0/prometheus-3.15.0.linux-amd64.tar.gz", sha256 = "2a542df32eac02ee17b9d844fb2aa1de00dafa5476579ba8a3ba862e9d572ea0", kind = "tar.gz", strip_prefix = "prometheus-3.15.0.linux-amd64", path = "promtool"),
            "linux_arm64": struct(url = "https://github.com/prometheus/prometheus/releases/download/v3.15.0/prometheus-3.15.0.linux-arm64.tar.gz", sha256 = "f1f90ec08e849d494ca66c611470afc50192f0355f1a61c33f2cbde02d067823", kind = "tar.gz", strip_prefix = "prometheus-3.15.0.linux-arm64", path = "promtool"),
            "windows_amd64": struct(url = "https://github.com/prometheus/prometheus/releases/download/v3.15.0/prometheus-3.15.0.windows-amd64.zip", sha256 = "5d333b385557d9adc2ff015d13da9809baccc52fb800a82d1e5c94b79258f87e", kind = "zip", strip_prefix = "prometheus-3.15.0.windows-amd64", path = "promtool.exe"),
        },
    ),
    "tflint": struct(
        version = "v0.64.0",
        target = "tflint",
        platforms = {
            "darwin_arm64": struct(url = "https://github.com/terraform-linters/tflint/releases/download/v0.64.0/tflint_darwin_arm64.zip", sha256 = "2496e9cb3d24992d553b45e7c87a0fdc9449ca975233876247a9bfeda857e6c0", kind = "zip", strip_prefix = "", path = "tflint"),
            "linux_amd64": struct(url = "https://github.com/terraform-linters/tflint/releases/download/v0.64.0/tflint_linux_amd64.zip", sha256 = "cca9d13e2e1d7a2c627af60ff899a3c9b74212899416aeb96ec764d2ef954537", kind = "zip", strip_prefix = "", path = "tflint"),
            "linux_arm64": struct(url = "https://github.com/terraform-linters/tflint/releases/download/v0.64.0/tflint_linux_arm64.zip", sha256 = "560da89aacf59389d4eb029730dd5b109b7288096c32f2726a0d9e783a5ea8eb", kind = "zip", strip_prefix = "", path = "tflint"),
            "windows_amd64": struct(url = "https://github.com/terraform-linters/tflint/releases/download/v0.64.0/tflint_windows_amd64.zip", sha256 = "fb42fb859d844b156a8ea9d3363078c4d8b85ca78782e60876b08c9b8e59f303", kind = "zip", strip_prefix = "", path = "tflint.exe"),
        },
    ),
    "opentofu": struct(
        version = "v1.13.1",
        target = "tofu",
        platforms = {
            "darwin_arm64": struct(url = "https://github.com/opentofu/opentofu/releases/download/v1.13.1/tofu_1.13.1_darwin_arm64.tar.gz", sha256 = "be78f659f04ef06a9dbd9b3934d46af95d787a3aa38396d459dea395261816a9", kind = "tar.gz", strip_prefix = "", path = "tofu"),
            "linux_amd64": struct(url = "https://github.com/opentofu/opentofu/releases/download/v1.13.1/tofu_1.13.1_linux_amd64.tar.gz", sha256 = "378ada19d4bc70c43732004e8159be771b23b9a5afdf059e5f8a2b3fa2c70a69", kind = "tar.gz", strip_prefix = "", path = "tofu"),
            "linux_arm64": struct(url = "https://github.com/opentofu/opentofu/releases/download/v1.13.1/tofu_1.13.1_linux_arm64.tar.gz", sha256 = "9c1ef375aa1852db0b2888aa921b640c71f8140d4682aa4fec99378a64fa7dc3", kind = "tar.gz", strip_prefix = "", path = "tofu"),
            "windows_amd64": struct(url = "https://github.com/opentofu/opentofu/releases/download/v1.13.1/tofu_1.13.1_windows_amd64.zip", sha256 = "5b653d1d95719eeec4ccf55f0e2102081b6f589d788197f1cabeb89cbd4078a7", kind = "zip", strip_prefix = "", path = "tofu.exe"),
        },
    ),
    # Rust protobuf/RPC codegen (ADR 0003, ADR 0106): buffa + connect-rust plugins.
    "protoc_gen_buffa": struct(
        version = "v0.9.2",
        target = "protoc-gen-buffa",
        platforms = {
            "darwin_arm64": struct(url = "https://github.com/anthropics/buffa/releases/download/v0.9.2/protoc-gen-buffa-v0.9.2-darwin-aarch64", sha256 = "654f4a5d58212afbdb3dec7475d99d6bc6cba2c10807c3751652cd3806cf5a5d", kind = "file", strip_prefix = "", path = "protoc-gen-buffa-v0.9.2-darwin-aarch64"),
            "linux_amd64": struct(url = "https://github.com/anthropics/buffa/releases/download/v0.9.2/protoc-gen-buffa-v0.9.2-linux-x86_64", sha256 = "e8d87b0cc34beb5524835888da694901deeeccaefd834fd5c03300375b629688", kind = "file", strip_prefix = "", path = "protoc-gen-buffa-v0.9.2-linux-x86_64"),
            "linux_arm64": struct(url = "https://github.com/anthropics/buffa/releases/download/v0.9.2/protoc-gen-buffa-v0.9.2-linux-aarch64", sha256 = "14f8285cc558f136aaa8a401e802f0e9e7a655e86dbf85ded7841f35fdf62b42", kind = "file", strip_prefix = "", path = "protoc-gen-buffa-v0.9.2-linux-aarch64"),
            "windows_amd64": struct(url = "https://github.com/anthropics/buffa/releases/download/v0.9.2/protoc-gen-buffa-v0.9.2-windows-x86_64.exe", sha256 = "d6346bda11fa0845f46e43935a2c17beb5dcfe56b96db9109a4db88ea0f47814", kind = "file", strip_prefix = "", path = "protoc-gen-buffa-v0.9.2-windows-x86_64.exe"),
        },
    ),
    "protoc_gen_buffa_packaging": struct(
        version = "v0.9.2",
        target = "protoc-gen-buffa-packaging",
        platforms = {
            "darwin_arm64": struct(url = "https://github.com/anthropics/buffa/releases/download/v0.9.2/protoc-gen-buffa-packaging-v0.9.2-darwin-aarch64", sha256 = "7f2213ebb3de60361b5ef9c91aa76e092f61c70bd9b0cb631302fecf677f78ec", kind = "file", strip_prefix = "", path = "protoc-gen-buffa-packaging-v0.9.2-darwin-aarch64"),
            "linux_amd64": struct(url = "https://github.com/anthropics/buffa/releases/download/v0.9.2/protoc-gen-buffa-packaging-v0.9.2-linux-x86_64", sha256 = "965e5b7b2ef6847f6597d32565468a093d70ab82ee74c326ff5397b04d419161", kind = "file", strip_prefix = "", path = "protoc-gen-buffa-packaging-v0.9.2-linux-x86_64"),
            "linux_arm64": struct(url = "https://github.com/anthropics/buffa/releases/download/v0.9.2/protoc-gen-buffa-packaging-v0.9.2-linux-aarch64", sha256 = "37a51a9dc9d62db6070f202f12e9a0bd9a8bf37cfa4adbfeb603cfabe0f9ff58", kind = "file", strip_prefix = "", path = "protoc-gen-buffa-packaging-v0.9.2-linux-aarch64"),
            "windows_amd64": struct(url = "https://github.com/anthropics/buffa/releases/download/v0.9.2/protoc-gen-buffa-packaging-v0.9.2-windows-x86_64.exe", sha256 = "0db4cdfc67fe8a3b45f3690cc766abdaf2a10f67122c6ce94b46d6c62abd6015", kind = "file", strip_prefix = "", path = "protoc-gen-buffa-packaging-v0.9.2-windows-x86_64.exe"),
        },
    ),
    "protoc_gen_connect_rust": struct(
        version = "v0.9.1",
        target = "protoc-gen-connect-rust",
        platforms = {
            "darwin_arm64": struct(url = "https://github.com/connectrpc/connect-rust/releases/download/v0.9.1/protoc-gen-connect-rust-v0.9.1-darwin-aarch64", sha256 = "8efc5fcce492d9e9e4bc540bfefc9b7651c7b6780ba9b833d22dbd2358c7286c", kind = "file", strip_prefix = "", path = "protoc-gen-connect-rust-v0.9.1-darwin-aarch64"),
            "linux_amd64": struct(url = "https://github.com/connectrpc/connect-rust/releases/download/v0.9.1/protoc-gen-connect-rust-v0.9.1-linux-x86_64", sha256 = "580c2a0690b9ad48364bd455cf0b7f8f0153bcbffe85140dd2ce07bfc0b1da24", kind = "file", strip_prefix = "", path = "protoc-gen-connect-rust-v0.9.1-linux-x86_64"),
            "linux_arm64": struct(url = "https://github.com/connectrpc/connect-rust/releases/download/v0.9.1/protoc-gen-connect-rust-v0.9.1-linux-aarch64", sha256 = "c945e5c2755d24d5fb81cc3bc9f00a2ca4778d4d687d598d9c84e6256067d685", kind = "file", strip_prefix = "", path = "protoc-gen-connect-rust-v0.9.1-linux-aarch64"),
            "windows_amd64": struct(url = "https://github.com/connectrpc/connect-rust/releases/download/v0.9.1/protoc-gen-connect-rust-v0.9.1-windows-x86_64.exe", sha256 = "5887b8f240a48d8009e868dbe08096b5a73e343d9140979e1bc844d7b49e3e0e", kind = "file", strip_prefix = "", path = "protoc-gen-connect-rust-v0.9.1-windows-x86_64.exe"),
        },
    ),
}

# envtest envtest-v1.36.2: integrity is the SHA-512 from controller-tools' envtest-releases.yaml.
_ENVTEST = {
    "darwin_arm64": struct(url = "https://github.com/kubernetes-sigs/controller-tools/releases/download/envtest-v1.36.2/envtest-v1.36.2-darwin-arm64.tar.gz", integrity = "sha512-knj55a9Vay8fLROXacHw1xfHtEJpF/3r26iYvLclqRbkkQ1hYIhhlP+b6Viep8XDLCuK4HVHA4dLfIuo3fxBzg==", strip_prefix = "controller-tools/envtest", exe = ""),
    "linux_amd64": struct(url = "https://github.com/kubernetes-sigs/controller-tools/releases/download/envtest-v1.36.2/envtest-v1.36.2-linux-amd64.tar.gz", integrity = "sha512-6nQxhsinmfXPj68Wlp+GGJ0APLfRMOCsS1h4nx5XSNzzDr6RyDehDVrEFTg9o+ELnmTWV4XJOMI+c5eBz7dvCA==", strip_prefix = "controller-tools/envtest", exe = ""),
    "linux_arm64": struct(url = "https://github.com/kubernetes-sigs/controller-tools/releases/download/envtest-v1.36.2/envtest-v1.36.2-linux-arm64.tar.gz", integrity = "sha512-LXLumFqOJio8V9yffw/Ykfaox79+uqLbbcbY6siuKBga/lHB82i2d1bNtAsQ3psgVgnhcm5fJ/fG2CTdnGZJrA==", strip_prefix = "controller-tools/envtest", exe = ""),
    "windows_amd64": struct(url = "https://github.com/kubernetes-sigs/controller-tools/releases/download/envtest-v1.36.2/envtest-v1.36.2-windows-amd64.tar.gz", integrity = "sha512-YGAJVxpbA55t3dNDO6/+vvJv2cWZkWCrH+lO5a4LmnYy+tSlaUlCdJfOCA+A/JX0bnbxarqWsBI8EuS7f27hKg==", strip_prefix = "controller-tools/envtest", exe = ".exe"),
}

_BB_RELEASE = {
    "bb_storage": struct(version = "20260930T153215Z-086b011", platforms = {
        "darwin_arm64": struct(url = "https://github.com/buildbarn/bb-storage/releases/download/20260930T153215Z-086b011/bb_storage.darwin_arm64", sha256 = "ca879430c4383cb723144eef3e3069ff8cf58422bd7f0a54c7e4290603139bc3"),
        "linux_amd64": struct(url = "https://github.com/buildbarn/bb-storage/releases/download/20260930T153215Z-086b011/bb_storage.linux_amd64", sha256 = "a8cad9ce449f59d23004a76fc133eb06cfde6c61e0f6ccad42db317a344da61c"),
        "linux_arm64": struct(url = "https://github.com/buildbarn/bb-storage/releases/download/20260930T153215Z-086b011/bb_storage.linux_arm64", sha256 = "eac6d9784d0a862d0c3e902d4ba351cd3c0421af3b197f9077e44390872b3f1d"),
        "windows_amd64": struct(url = "https://github.com/buildbarn/bb-storage/releases/download/20260930T153215Z-086b011/bb_storage.windows_amd64.exe", sha256 = "0095a3471c151dd56c950b478ed849e65e23267766ac082dd258e0d627816dc2"),
    }),
    "bb_scheduler": struct(version = "20260930T173749Z-1a3be95", platforms = {
        "darwin_arm64": struct(url = "https://github.com/buildbarn/bb-remote-execution/releases/download/20260930T173749Z-1a3be95/bb_scheduler.darwin_arm64", sha256 = "6d28e3852ca8ece61c23c7146690ca6313fb01f62b836b2120771bb49812c7ce"),
        "linux_amd64": struct(url = "https://github.com/buildbarn/bb-remote-execution/releases/download/20260930T173749Z-1a3be95/bb_scheduler.linux_amd64", sha256 = "5005fddb8005266f0c1970af54c0693bf792e3c55a686716d3a4d0367f017767"),
        "linux_arm64": struct(url = "https://github.com/buildbarn/bb-remote-execution/releases/download/20260930T173749Z-1a3be95/bb_scheduler.linux_arm64", sha256 = "28d23d4981ed789ed0d7f192f406eaed4dd26692ddf12b7bcc149317b61894b3"),
        "windows_amd64": struct(url = "https://github.com/buildbarn/bb-remote-execution/releases/download/20260930T173749Z-1a3be95/bb_scheduler.windows_amd64.exe", sha256 = "31c282a61d07a07bf757845e99f08532b233e5ac37d114ad1eb9bc8d506888fc"),
    }),
    "bb_worker": struct(version = "20260930T173749Z-1a3be95", platforms = {
        "darwin_arm64": struct(url = "https://github.com/buildbarn/bb-remote-execution/releases/download/20260930T173749Z-1a3be95/bb_worker.darwin_arm64", sha256 = "67fbcf00951b7e1ca90aa07781b496795b3d9f7b588c8f03e59ecbe3298656ff"),
        "linux_amd64": struct(url = "https://github.com/buildbarn/bb-remote-execution/releases/download/20260930T173749Z-1a3be95/bb_worker.linux_amd64", sha256 = "af5c37d2bd3b792328ce70eeac8b72d344517f929c5088ea61f76cecf31fd0ef"),
        "linux_arm64": struct(url = "https://github.com/buildbarn/bb-remote-execution/releases/download/20260930T173749Z-1a3be95/bb_worker.linux_arm64", sha256 = "4212d2565044c91caa9dff20a69f1945b637ce81f084fd2cdc02ff1389259783"),
        "windows_amd64": struct(url = "https://github.com/buildbarn/bb-remote-execution/releases/download/20260930T173749Z-1a3be95/bb_worker.windows_amd64.exe", sha256 = "f28f91fd36737bfa076ca4eedc4da6e0039877af26b900becaaf6759b9fc8b23"),
    }),
    "bb_runner": struct(version = "20260930T173749Z-1a3be95", platforms = {
        "darwin_arm64": struct(url = "https://github.com/buildbarn/bb-remote-execution/releases/download/20260930T173749Z-1a3be95/bb_runner.darwin_arm64", sha256 = "0c8ffbf17da304808a1580d430e0eaeea4dba47fd04b5f2434348980ec181105"),
        "linux_amd64": struct(url = "https://github.com/buildbarn/bb-remote-execution/releases/download/20260930T173749Z-1a3be95/bb_runner.linux_amd64", sha256 = "add6b25428c2654315bcee74e298f5eb023fd90afd0b13dd830417c83806a26c"),
        "linux_arm64": struct(url = "https://github.com/buildbarn/bb-remote-execution/releases/download/20260930T173749Z-1a3be95/bb_runner.linux_arm64", sha256 = "5b35d72335b31db346c8e89901ce048c78e8b84bfe73bd183369e4afd4454bc6"),
        "windows_amd64": struct(url = "https://github.com/buildbarn/bb-remote-execution/releases/download/20260930T173749Z-1a3be95/bb_runner.windows_amd64.exe", sha256 = "d5c0d2d37843a499049d11a0688f10c54a4fa85fcaf6be5359451f6681a53710"),
    }),
}

def _exe(plat):
    return ".exe" if plat.startswith("windows") else ""

def _hub_impl(rctx):
    rctx.file("BUILD.bazel", rctx.attr.build_file_content)

_hub = repository_rule(
    implementation = _hub_impl,
    attrs = {"build_file_content": attr.string(mandatory = True)},
    doc = "A hub repository whose aliases select the per-platform pinned binary.",
)

def _hub_build(tool, aliases):
    """BUILD content with one config_setting per platform and `aliases`.

    Args:
      tool: tool name for error messages.
      aliases: {alias name: {platform: actual label}}.
    """
    lines = ['package(default_visibility = ["//visibility:public"])', ""]
    for plat, (os, cpu) in _PLATFORMS.items():
        lines.append('config_setting(name = "%s", constraint_values = ["%s", "%s"], visibility = ["//visibility:private"])' % (plat, os, cpu))
    for name, by_plat in aliases.items():
        branches = "".join(['        ":%s": "%s",\n' % (plat, by_plat[plat]) for plat in _PLATFORMS])
        lines.append('alias(\n    name = "%s",\n    actual = select({\n%s    }, no_match_error = "%s: no pinned binary for this platform (darwin_arm64, linux_amd64, linux_arm64, windows_amd64)"),\n)' % (name, branches, tool))
    return "\n".join(lines) + "\n"

def _pinned_impl(mctx):
    hubs = []

    for repo, tool in _TOOLS.items():
        by_plat = {}
        manifest_by_plat = {}
        for plat, p in tool.platforms.items():
            name = "%s_%s" % (repo, plat)
            if p.kind == "file":
                http_file(
                    name = name,
                    url = p.url,
                    sha256 = p.sha256,
                    executable = True,
                    downloaded_file_path = tool.target + _exe(plat),
                )
                by_plat[plat] = "@%s//file" % name
            elif tool.target == None:
                # helm-unittest: a flat plugin archive -> <repo>/unittest/{plugin.yaml,untt-*}.
                http_archive(
                    name = name,
                    url = p.url,
                    sha256 = p.sha256,
                    add_prefix = "unittest",
                    build_file_content = 'filegroup(name = "plugin", srcs = glob(["unittest/**"]), visibility = ["//visibility:public"])\nexports_files(["unittest/plugin.yaml"], visibility = ["//visibility:public"])\n',
                )
                by_plat[plat] = "@%s//:plugin" % name
                manifest_by_plat[plat] = "@%s//:unittest/plugin.yaml" % name
            else:
                http_archive(
                    name = name,
                    url = p.url,
                    sha256 = p.sha256,
                    strip_prefix = p.strip_prefix,
                    build_file_content = 'exports_files(["%s"], visibility = ["//visibility:public"])\n' % p.path,
                )
                by_plat[plat] = "@%s//:%s" % (name, p.path)
        aliases = {tool.target or "plugin": by_plat}
        if manifest_by_plat:
            aliases["plugin.yaml"] = manifest_by_plat
        _hub(
            name = repo,
            build_file_content = _hub_build(repo, aliases),
        )
        hubs.append(repo)

    envtest = {"binaries": {}, "etcd": {}, "kube-apiserver": {}, "kubectl": {}}
    for plat, p in _ENVTEST.items():
        name = "envtest_" + plat
        bins = ["etcd" + p.exe, "kube-apiserver" + p.exe, "kubectl" + p.exe]
        http_archive(
            name = name,
            url = p.url,
            integrity = p.integrity,
            strip_prefix = p.strip_prefix,
            build_file_content = 'filegroup(name = "binaries", srcs = %r, visibility = ["//visibility:public"])\nexports_files(%r, visibility = ["//visibility:public"])\n' % (bins, bins),
        )
        envtest["binaries"][plat] = "@%s//:binaries" % name
        for b in ["etcd", "kube-apiserver", "kubectl"]:
            envtest[b][plat] = "@%s//:%s%s" % (name, b, p.exe)
    _hub(name = "envtest", build_file_content = _hub_build("envtest", envtest))
    hubs.append("envtest")

    bb = {}
    for binary, rel in _BB_RELEASE.items():
        bb[binary] = {}
        for plat, p in rel.platforms.items():
            name = "%s_%s" % (binary, plat)
            http_file(
                name = name,
                url = p.url,
                sha256 = p.sha256,
                executable = True,
                downloaded_file_path = binary + _exe(plat),
            )
            bb[binary][plat] = "@%s//file" % name
    _hub(name = "bb_release", build_file_content = _hub_build("bb_release", bb))
    hubs.append("bb_release")

    return mctx.extension_metadata(
        reproducible = True,
        root_module_direct_deps = hubs,
        root_module_direct_dev_deps = [],
    )

pinned = module_extension(
    implementation = _pinned_impl,
    doc = "Pinned third-party tool binaries; see the module docstring.",
)
