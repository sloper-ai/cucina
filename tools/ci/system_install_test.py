#!/usr/bin/env python3
# SPDX-License-Identifier: FSL-1.1-ALv2
"""Public two-step workflow harness. Every client is an owned executable fixture."""
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).absolute().parents[2]
SECRET = "PRIVATE-FIXTURE-MATERIAL-DO-NOT-EXPORT"
PRIVATE_REGISTRY = "synthetic-private-registry.example.invalid"
FAKE = r'''import hashlib, json, os, pathlib, shlex, shutil, subprocess, sys
name, args = pathlib.Path(sys.argv[0]).name, sys.argv[1:]
marker = pathlib.Path(os.environ["FIXTURE_STATE"])
secret = "PRIVATE-FIXTURE-MATERIAL-DO-NOT-EXPORT"
private_registry = "synthetic-private-registry.example.invalid"
fixture_bin = pathlib.Path(os.environ["FIXTURE_BIN"])
state = pathlib.Path(os.environ["HOME"]) / ".config/cucina/system-install-123-1"
def refuse():
    marker.with_suffix(".containment-failed").write_text("fixture dispatch refused\n")
    raise SystemExit(86)
def owned_tool(tool):
    path = fixture_bin / tool
    if shutil.which(tool) != str(path) or path.is_symlink() or hashlib.sha256(path.read_bytes()).hexdigest() != os.environ["FIXTURE_TOOL_SHA256"]:
        refuse()
    return str(path)
def run_helm(arguments):
    backend, wrapper = fixture_bin / "helm", state / "bin/helm"
    meta = state / "metadata.json"
    if meta.is_symlink() or meta.stat().st_mode & 0o777 != 0o600:
        refuse()
    metadata = json.loads(meta.read_text())
    if metadata["helm"]["selected_path"] != str(backend) or backend.is_symlink() or hashlib.sha256(backend.read_bytes()).hexdigest() != os.environ["FIXTURE_TOOL_SHA256"]:
        refuse()
    if shutil.which("helm") != str(wrapper) or wrapper.is_symlink():
        refuse()
    lines = wrapper.read_text().splitlines()
    command = shlex.split(lines[1]) if len(lines) == 2 and lines[0] == "#!/bin/sh" else []
    if len(command) != 6 or command[0] != "exec" or not pathlib.Path(command[1]).is_absolute() or pathlib.Path(command[1]).resolve() != pathlib.Path(os.environ["FIXTURE_PYTHON"]).resolve() or command[2] != "-I" or pathlib.Path(command[3]).resolve() != pathlib.Path(os.environ["FIXTURE_HELPER"]).resolve() or command[4:] != ["--helm", "$@"]:
        refuse()
    if any(path.stat().st_mode & 0o777 != 0o700 for path in [state.parent, state, wrapper.parent, wrapper]):
        refuse()
    if os.environ.get("KUBECONFIG") != str(state / "config.stdout") or os.environ.get("HELM_KUBECONTEXT") != "kind-cucina":
        refuse()
    subprocess.run([str(wrapper)] + arguments, check=True)
if name == "mise":
    for tool in ["mise", "ct", "helm", "kubectl", "kind"]:
        owned_tool(tool)
    if args == ["exec", "kind@0.33.0", "--", "kind", "get", "kubeconfig", "--name", "cucina"]:
        binary = owned_tool("kind")
        os.execve(binary, [binary] + args[4:], dict(os.environ))
    python = fixture_bin / "python3"
    prefix = ["exec", "helm@3.22.0", os.environ["CT"], "--", "python3", os.environ["FIXTURE_HELPER"]]
    suffix = ["install"] if os.environ["FIXTURE_PHASE"] == "install" else ["capture", str(pathlib.Path(os.environ["RUNNER_TEMP"]) / "system-diagnostics/evidence.json"), os.environ["INSTALL_OUTCOME"]]
    if args != prefix + suffix or shutil.which("python3") != str(python) or python.resolve() != pathlib.Path(os.environ["FIXTURE_PYTHON"]).resolve():
        refuse()
    os.execve(str(python), [str(python)] + args[5:], dict(os.environ))
if name == "kind":
    owned_tool("kind")
    if args != ["get", "kubeconfig", "--name", "cucina"]:
        refuse()
    # Inline fixture strings, not certificates, keys or usable credentials.
    cluster = {"server": "https://127.0.0.1:6443", "certificate-authority-data": "Zml4dHVyZQ=="}
    if os.environ["CASE"] == "config-override":
        cluster["proxy-url"] = secret
    print(json.dumps({"apiVersion": "v1", "kind": "Config", "current-context": "kind-cucina", "contexts": [{"name": "kind-cucina", "context": {"cluster": "kind-cucina", "user": "kind-cucina"}}], "clusters": [{"name": "kind-cucina", "cluster": cluster}], "users": [{"name": "kind-cucina", "user": {"client-certificate-data": "Zml4dHVyZQ==", "client-key-data": "Zml4dHVyZQ=="}}]}))
    sys.exit(0)
if name == "helm":
    owned_tool("helm")
    if args[0] == "version":
        print("v3.17.1")  # Observed version, deliberately different from the install pin.
    else:
        print(secret)
        print(secret, file=sys.stderr)
    sys.exit(0)
if name == "ct":
    owned_tool("ct")
    owned_tool("kubectl")
    if args != ["install", "--charts", "charts/cucina", "--chart-dirs", "charts", "--skip-clean-up"]:
        refuse()
    run_helm(["version", "--template", "{{ .Version }}"])
    install = ["install", "cucina-fixture", "charts/cucina", "--namespace", "cucina-fixture", "--wait", "--values", "charts/cucina/ci/kind-values.yaml"]
    if os.environ["CASE"] == "unsafe-argv":
        install += ["--set", "password=" + secret]
    if os.environ["CASE"] == "argv-override":
        install += ["--kube-context", secret]
    run_helm(install)
    marker.write_text("installed")
    code = int(os.environ["CT_CODE"])
    if code == 0:
        run_helm(["test", "cucina-fixture", "--namespace", "cucina-fixture"])
    print(secret)
    sys.exit(code)
if name == "kubectl":
    owned_tool("kubectl")
    if args == ["--kubeconfig", str(state / "owned-kind.stdout"), "--context", "kind-cucina", "config", "view", "--minify", "--flatten", "--raw", "--output=json"]:
        print((state / "owned-kind.stdout").read_text())
        sys.exit(0)
    expected = ["--kubeconfig", str(state / "config.stdout"), "--context", "kind-cucina", "get", "configmaps,statefulsets,deployments", "--namespace", "cucina-fixture", "--selector=app.kubernetes.io/instance=cucina-fixture,app.kubernetes.io/name=cucina", "--output=json", "--request-timeout=20s"]
    if args != expected or os.environ.get("KUBECONFIG") != str(state / "config.stdout") or os.environ.get("HELM_KUBECONTEXT") != "kind-cucina":
        refuse()
    if not marker.exists() or os.environ["CASE"] == "missing":
        print(secret, file=sys.stderr)
        sys.exit(1)
    count = 0 if os.environ["CT_CODE"] != "0" else 2
    shards = {str(i): {"weight": 1, "backend": {"grpc": {"client": {"address": secret}}}} for i in range(count)}
    def metadata(component):
        return {"name": "cucina-fixture-" + component, "namespace": "cucina-fixture", "labels": {"app.kubernetes.io/component": component, "app.kubernetes.io/instance": "cucina-fixture"}}
    items = []
    for component in ["frontend", "scheduler"]:
        stores = ["contentAddressableStorage", "actionCache", "fileSystemAccessCache"] if component == "frontend" else ["contentAddressableStorage"]
        config = {store: {"sharding": {"shards": shards}} for store in stores}
        config["privateKey"] = secret
        if component == "frontend" and os.environ["CASE"] == "missing-store":
            del config["actionCache"]
        items.append({"kind": "ConfigMap", "metadata": metadata(component), "data": {component + ".jsonnet": json.dumps(config), "password": secret}})
    for component in ["frontend", "scheduler", "storage", "controller", "sts"]:
        items.append({"kind": "StatefulSet" if component == "storage" else "Deployment", "metadata": metadata(component), "spec": {"replicas": 2 if component in ["storage", "sts"] else 1, "template": {"spec": {"containers": [{"image": "synthetic-user:" + secret + "@" + private_registry + "/fixture:v1" if component == "frontend" else "example.invalid/fixture:v1", "env": [{"name": "PASSWORD", "value": secret}]}]}}}, "status": {"readyReplicas": 0}})
    if os.environ["CASE"] == "duplicate":
        items.append(items[0])
    print(json.dumps({"items": items}))
    sys.exit(0)
refuse()
'''


def workflow_step(name):
    lines = (ROOT / ".github/workflows/nightly.yml").read_text().splitlines()
    start = lines.index("      - name: " + name)
    start = lines.index("        run: |", start) + 1
    body = []
    for line in lines[start:]:
        if line and not line.startswith("          "):
            break
        body.append(line[10:])
    return "\n".join(body)


@contextmanager
def fixture_workspace():
    old_mask = os.umask(0o077)
    # These captures contain only owned-fixture data. Bazel retains its declared
    # output directory, whereas an external HOME is not writable in the sandbox.
    outputs = os.environ.get("TEST_UNDECLARED_OUTPUTS_DIR")
    parent = (Path(outputs).resolve(strict=True) / "cucina-fixture-evidence"
              if outputs else Path.home() / ".config/cucina")
    if parent.absolute() != parent.resolve() or ROOT.resolve() in parent.resolve().parents:
        raise ValueError("fixture evidence must be nonsymlinked and outside checkout")
    parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    if parent.stat().st_mode & 0o777 != 0o700:
        raise ValueError("fixture evidence parent must be private")
    work = Path(tempfile.mkdtemp(prefix="system-install-fixture-", dir=parent))
    try:
        yield work
    finally:
        os.umask(old_mask)
        print("Private fixture evidence: " + str(work), file=sys.stderr)


class SystemInstallEvidence(unittest.TestCase):
    # Guards: R-TEST-2/-6 — same-install evidence is private, complete and bound to
    # owned kind; capture cannot replace ct's status or turn missing evidence green.
    def test_public_install_and_capture_steps(self):
        cases = [("complete", 17, 17, 0), ("missing", 17, 17, 1),
                 ("complete", 0, 0, 0), ("missing", 0, 0, 1),
                 ("unsafe-argv", 0, 0, 1), ("argv-override", 0, 1, 1),
                 ("env-override", 0, 1, 1), ("config-override", 0, 1, 1),
                 ("missing-store", 0, 0, 1), ("duplicate", 0, 0, 1),
                 ("unsafe-evidence", 17, 17, 1)]
        for case, ct_code, install_code, capture_code in cases:
            # No subTest catch: the first unexpected failure stops all later attempts.
            with fixture_workspace() as work:
                for directory in ["bin", "home", "runner", "charts/cucina/ci", "charts/cucina/files", "release"]:
                    (work / directory).mkdir(parents=True, exist_ok=True)
                (work / "release/kind-values.yaml").write_text("sizeProfile: small\n")
                (work / "charts/cucina/values.yaml").write_text("storage: {shards: null}\n")
                (work / "charts/cucina/files/profiles.yaml").write_text("small: {storage: {shards: 2}}\n")
                (work / "bin/python3").symlink_to(sys.executable)
                for tool in ["mise", "helm", "ct", "kubectl", "kind"]:
                    path = work / "bin" / tool
                    path.write_text("#!" + sys.executable + "\n" + FAKE)
                    path.chmod(0o755)
                env = dict(PATH=str(work / "bin") + ":/usr/bin:/bin", LANG="C", LC_ALL="C",
                           HOME=str(work / "home"), RUNNER_TEMP=str(work / "runner"),
                           GITHUB_WORKSPACE=str(ROOT), GITHUB_RUN_ID="123", GITHUB_RUN_ATTEMPT="1",
                           CT="aqua:helm/chart-testing@3.14.0", SYSTEM_HELM="helm@3.22.0", FIXTURE_STATE=str(work / "state"),
                           CT_CODE=str(ct_code), CASE=case, FIXTURE_BIN=str(work / "bin"),
                           FIXTURE_PYTHON=sys.executable, FIXTURE_HELPER=str(ROOT / "tools/ci/system-install.py"),
                           FIXTURE_TOOL_SHA256=hashlib.sha256((work / "bin/mise").read_bytes()).hexdigest(),
                           BASH_ENV="/dev/null", ENV="/dev/null", KUBECONFIG=str(work / "no-kubeconfig"))
                if case == "env-override":
                    env["HELM_KUBECONTEXT"] = SECRET
                preflight = '''export PATH="$FIXTURE_BIN:/usr/bin:/bin"
for tool in mise ct helm kubectl kind python3; do
  test "$(command -v "$tool")" = "$FIXTURE_BIN/$tool" || exit 86
done
'''
                outputs = ""
                for phase, step, expected in [("install", "ct install and helm test (bare-kind profile)", install_code),
                                               ("capture", "capture sanitized system evidence", capture_code)]:
                    env["FIXTURE_PHASE"] = phase
                    env["INSTALL_OUTCOME"] = "success" if install_code == 0 else "failure"
                    env["GITHUB_OUTPUT"] = str(work / (phase + ".outputs"))
                    Path(env["GITHUB_OUTPUT"]).touch(mode=0o600)
                    evidence = work / "runner/system-diagnostics/evidence.json"
                    if phase == "capture" and case == "unsafe-evidence":
                        evidence.parent.mkdir(mode=0o700)
                        (work / "sentinel").write_text("unchanged")
                        # Keep the same forbidden symlink after Bazel relocates
                        # retained outputs out of its temporary sandbox.
                        evidence.symlink_to(os.path.relpath(work / "sentinel", evidence.parent))
                    argv = ["/bin/bash", "--noprofile", "--norc", "-e", "-c", preflight + workflow_step(step)]
                    (work / (phase + "-attempt.json")).write_text(json.dumps({"argv": argv, "environment": env}, indent=2))
                    stdout, stderr = work / (phase + ".stdout"), work / (phase + ".stderr")
                    with stdout.open("xb") as out, stderr.open("xb") as err:
                        result = subprocess.run(argv, cwd=work, env=env, stdout=out, stderr=err, timeout=15)
                    (work / (phase + "-result.json")).write_text(json.dumps({"exit_code": result.returncode, "stdout_sha256": hashlib.sha256(stdout.read_bytes()).hexdigest(), "stderr_sha256": hashlib.sha256(stderr.read_bytes()).hexdigest()}))
                    self.assertFalse((work / "state.containment-failed").exists(), "containment refused; stop")
                    self.assertNotEqual(result.returncode, 86, "preflight refused; stop")
                    self.assertEqual(result.returncode, expected, (case, phase))
                    outputs += stdout.read_text() + stderr.read_text()
                self.assertNotIn(SECRET, outputs)
                self.assertNotIn(PRIVATE_REGISTRY, outputs)
                if case == "unsafe-evidence":
                    self.assertEqual((work / "sentinel").read_text(), "unchanged")
                    self.assertEqual((work / "capture.outputs").read_text(), "")
                    continue
                self.assertEqual((work / "capture.outputs").read_text(), "evidence_ready=true\n")
                report = json.loads(evidence.read_text())
                self.assertEqual(report["install_outcome"], env["INSTALL_OUTCOME"])
                self.assertEqual(report["diagnostics_complete"], capture_code == 0)
                self.assertNotIn(SECRET, evidence.read_text())
                self.assertNotIn(PRIVATE_REGISTRY, evidence.read_text())
                if case not in {"env-override", "config-override"}:
                    self.assertEqual(report["helm"]["version"], "v3.17.1")
                    self.assertEqual(report["helm"]["selected_path"], str(work / "bin/helm"))
                    self.assertEqual(report["binding"]["context"], "kind-cucina")
                if case == "complete":
                    self.assertEqual({item["shard_count"] for item in report["topology"]}, {0 if ct_code else 2})
                    self.assertEqual(len(report["topology"]), 4)
                    self.assertEqual({item["component"] for item in report["workloads"]}, {"frontend", "scheduler", "storage", "controller", "sts"})
                    self.assertTrue(all("images" not in item for item in report["workloads"]))
                for path in (work / "home/.config/cucina").rglob("*"):
                    executable = path.name == "helm" and path.parent.name == "bin"
                    self.assertEqual(path.stat().st_mode & 0o777, 0o700 if path.is_dir() or executable else 0o600)


if __name__ == "__main__":
    unittest.main(failfast=True)
