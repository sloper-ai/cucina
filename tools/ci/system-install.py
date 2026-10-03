#!/usr/bin/env python3
# SPDX-License-Identifier: FSL-1.1-ALv2
"""CI-owned install and capture steps; raw state is private, never an artifact.

`install` prepares a transparent Helm recorder and execs ct. GitHub owns that
process tree and its original exit status. `capture` is a separate always step.
"""
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import stat
import subprocess
import sys
from urllib.parse import urlsplit

CONTEXT = "kind-cucina"
COMPONENTS = {"frontend", "storage", "scheduler", "controller", "sts"}
STORES = {"frontend": {"contentAddressableStorage", "actionCache", "fileSystemAccessCache"},
          "scheduler": {"contentAddressableStorage"}}
WORKSPACE = Path(__file__).resolve().parents[2]


def directory(path, create=False):
    path = path.absolute()
    resolved = path.resolve()
    if path != resolved or resolved == WORKSPACE or WORKSPACE in resolved.parents:
        raise ValueError("destination must be nonsymlinked and outside checkout")
    if create:
        path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o700:
        raise ValueError("unsafe directory")
    return path


def state_dir(create=False):
    identity = [os.environ.get(key, "") for key in ("GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT")]
    if any(not re.fullmatch(r"[0-9]{1,20}", value) for value in identity):
        raise ValueError("missing run identity")
    root = directory(Path.home() / ".config/cucina", create)
    state = root / ("system-install-" + "-".join(identity))
    if create:
        state.mkdir(mode=0o700)  # Refuse a pre-existing attempt; never overwrite evidence.
    return directory(state)


def private_file(path, mode="rb"):
    directory(path.parent)
    flags = os.O_NOFOLLOW | (os.O_RDONLY if mode == "rb" else os.O_WRONLY | os.O_CREAT)
    flags |= os.O_APPEND if mode == "ab" else (os.O_EXCL if mode == "wb" else 0)
    fd = os.open(path, flags, 0o600)
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600:
        os.close(fd)
        raise ValueError("unsafe file")
    return os.fdopen(fd, mode)


def read_private(path):
    with private_file(path) as stream:
        data = stream.read(8 * 1024 * 1024 + 1)
    if len(data) > 8 * 1024 * 1024:
        raise ValueError("capture exceeds size bound")
    return data


def write_json(path, value):
    with private_file(path, "wb") as stream:
        stream.write((json.dumps(value, indent=2) + "\n").encode())


def run_private(argv, name, state, env):
    # Short preparation/read commands only; never detach or supervise ct here.
    with private_file(state / (name + ".stdout"), "wb") as out, private_file(state / (name + ".stderr"), "wb") as err:
        result = subprocess.run(argv, stdout=out, stderr=err, env=env, timeout=30)
    if result.returncode:
        raise ValueError("diagnostic command failed")
    return state / (name + ".stdout")


def reject_overrides(env, bound=None):
    for key, value in env.items():
        if value and (key.startswith("HELM_KUBE") or key in {"KUBERNETES_MASTER", "HELM_DRIVER", "HELM_DRIVER_SQL_CONNECTION_STRING"}):
            if bound and key == "HELM_KUBECONTEXT" and value == CONTEXT:
                continue
            raise ValueError("route or identity override refused")
    if bound and (env.get("KUBECONFIG") != str(bound) or env.get("HELM_KUBECONTEXT") != CONTEXT):
        raise ValueError("target binding changed")


def validate_config(config):
    if set(config) - {"apiVersion", "kind", "current-context", "contexts", "clusters", "users", "preferences"}:
        raise ValueError("unsupported kubeconfig field")
    if config.get("apiVersion") != "v1" or config.get("kind") != "Config" or config.get("current-context") != CONTEXT or config.get("preferences", {}):
        raise ValueError("invalid dedicated configuration")
    parts = {}
    for plural, field in (("contexts", "context"), ("clusters", "cluster"), ("users", "user")):
        entries = config.get(plural, [])
        if len(entries) != 1 or set(entries[0]) != {"name", field} or entries[0]["name"] != CONTEXT:
            raise ValueError("ambiguous dedicated configuration")
        parts[field] = entries[0][field]
    if parts["context"] != {"cluster": CONTEXT, "user": CONTEXT}:
        raise ValueError("context binding refused")
    cluster, user = parts["cluster"], parts["user"]
    if set(cluster) != {"server", "certificate-authority-data"} or set(user) != {"client-certificate-data", "client-key-data"}:
        raise ValueError("route or identity configuration refused")
    endpoint = urlsplit(cluster["server"])
    if endpoint.scheme != "https" or endpoint.hostname not in {"127.0.0.1", "::1"} or not endpoint.port or endpoint.username or endpoint.password or endpoint.path not in {"", "/"} or endpoint.query or endpoint.fragment:
        raise ValueError("nonlocal dedicated endpoint refused")
    for encoded in (cluster["certificate-authority-data"], user["client-certificate-data"], user["client-key-data"]):
        if not base64.b64decode(encoded, validate=True):
            raise ValueError("missing inline identity")


def binding(state, metadata, env):
    config = state / "config.stdout"
    data = read_private(config)
    if hashlib.sha256(data).hexdigest() != metadata["binding"]["config_sha256"]:
        raise ValueError("dedicated configuration changed")
    validate_config(json.loads(data))
    return dict(env, KUBECONFIG=str(config), HELM_KUBECONTEXT=CONTEXT)


def safe_argv(args):
    name = r"cucina(?:-[a-z0-9]{1,32})?"
    if args in (["version", "--template", "{{ .Version }}"], ["version", "--short"], ["dependency", "build", "charts/cucina"]):
        return True
    if len(args) == 4 and args[0] == "test" and args[2] == "--namespace":
        return bool(re.fullmatch(name, args[1]) and re.fullmatch(name, args[3]))
    return bool(len(args) == 8 and args[0] == "install" and re.fullmatch(name, args[1])
                and args[2:4] == ["charts/cucina", "--namespace"] and re.fullmatch(name, args[4])
                and args[5:] == ["--wait", "--values", "charts/cucina/ci/kind-values.yaml"])


def install():
    reject_overrides(os.environ)
    state = state_dir(create=True)
    tools = {name: shutil.which(name) for name in ("helm", "ct", "kubectl", "mise")}
    if not all(tools.values()):
        raise ValueError("missing tool")
    tools = {name: os.path.abspath(path) for name, path in tools.items()}
    env = dict(os.environ)
    # Verified kind v0.33.0 get/kubeconfig source: provider.KubeConfig(name, internal).
    # Obtain the already-owned cluster's config, not a similarly named ambient context.
    owned = run_private([tools["mise"], "exec", "kind@0.33.0", "--", "kind", "get", "kubeconfig", "--name", "cucina"], "owned-kind", state, env)
    config = run_private([tools["kubectl"], "--kubeconfig", str(owned), "--context", CONTEXT, "config", "view", "--minify", "--flatten", "--raw", "--output=json"], "config", state, env)
    config_bytes = read_private(config)
    validate_config(json.loads(config_bytes))
    metadata = {"binding": {"context": CONTEXT, "config_sha256": hashlib.sha256(config_bytes).hexdigest(), "kind_source_sha256": hashlib.sha256(read_private(owned)).hexdigest()},
                "original_path": os.environ["PATH"], "kubectl": tools["kubectl"]}
    env = binding(state, metadata, env)
    version_file = run_private([tools["helm"], "version", "--template", "{{ .Version }}"], "version", state, env)
    version = read_private(version_file).decode().strip()
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?", version):
        raise ValueError("invalid Helm version")
    metadata["helm"] = {"selected_path": tools["helm"], "resolved_path": str(Path(tools["helm"]).resolve()), "version": version}
    metadata["input_sha256"] = {p: hashlib.sha256(Path(p).read_bytes()).hexdigest() for p in (
        "charts/cucina/values.yaml", "charts/cucina/files/profiles.yaml", "charts/cucina/ci/kind-values.yaml")}
    # Verified ct v3.14.0 flag. Keep original install --wait, five-minute default,
    # and Helm test semantics; the workflow's always kind-delete owns final cleanup.
    argv = [tools["ct"], "install", "--charts", "charts/cucina", "--chart-dirs", "charts", "--skip-clean-up"]
    metadata["ct"] = {"path": tools["ct"], "argv": argv}
    write_json(state / "metadata.json", metadata)
    bindir = directory(state / "bin", create=True)
    wrapper = bindir / "helm"
    with private_file(wrapper, "wb") as stream:
        stream.write(("#!/bin/sh\nexec " + shlex.quote(sys.executable) + " -I " + shlex.quote(str(Path(__file__).resolve())) + ' --helm "$@"\n').encode())
    wrapper.chmod(0o700)
    env["PATH"] = str(bindir) + os.pathsep + metadata["original_path"]
    with private_file(state / "ct.stdout", "wb") as out, private_file(state / "ct.stderr", "wb") as err:
        os.dup2(out.fileno(), 1)
        os.dup2(err.fileno(), 2)
        os.execve(tools["ct"], argv, env)  # No parent supervisor or detached process group.


def helm():
    state = state_dir()
    metadata = json.loads(read_private(state / "metadata.json"))
    args = sys.argv[2:]
    with private_file(state / "helm-argv.jsonl", "ab") as stream:
        stream.write((json.dumps(args) + "\n").encode())
    reject_overrides(os.environ, state / "config.stdout")
    if any(arg.startswith("--kube") for arg in args):
        raise ValueError("Helm target override refused")
    env = binding(state, metadata, dict(os.environ, PATH=metadata["original_path"]))
    binary = metadata["helm"]["selected_path"]
    os.execve(binary, [binary] + args, env)


def topology(items, release, namespace):
    workloads, routes, hashes, seen = [], [], {}, set()
    for item in items:
        meta = item.get("metadata", {})
        component = meta.get("labels", {}).get("app.kubernetes.io/component")
        kind = item.get("kind")
        relevant = kind in {"Deployment", "StatefulSet"} and component in COMPONENTS
        relevant |= kind == "ConfigMap" and component in STORES
        if not relevant:
            continue
        key = (kind, component)
        expected_kind = "StatefulSet" if component == "storage" else "Deployment"
        if key in seen or meta.get("name") != release + "-" + component or meta.get("namespace") != namespace or meta.get("labels", {}).get("app.kubernetes.io/instance") != release or (kind != "ConfigMap" and kind != expected_kind):
            raise ValueError("ambiguous object identity")
        seen.add(key)
        if kind != "ConfigMap":
            spec, status = item.get("spec", {}), item.get("status", {})
            row = {"component": component, "kind": kind}
            for output, value in (("replicas", spec.get("replicas")), ("ready_replicas", status.get("readyReplicas", 0))):
                if type(value) is not int or value < 0:
                    raise ValueError("invalid count")
                row[output] = value
            # Installed image references may contain credentials or private registry IDs.
            # Keep them only in the private raw capture, never in public evidence.
            workloads.append(row)
            continue
        text = item["data"][component + ".jsonnet"]
        hashes[component] = hashlib.sha256(text.encode()).hexdigest()
        config = json.loads(re.sub(r'importstr "[^"\n]+"', '"PRIVATE_IMPORT"', text))
        for store in sorted(STORES[component] | ({"initialSizeClassCache"} if component == "scheduler" else set())):
            if store not in config:
                if store in STORES[component]:
                    raise ValueError("missing required store")
                continue
            found = []
            def walk(node):
                if isinstance(node, dict):
                    if "sharding" in node:
                        found.append(node["sharding"]["shards"])
                    for value in node.values():
                        walk(value)
                elif isinstance(node, list):
                    for value in node:
                        walk(value)
            walk(config[store])
            if len(found) != 1 or not isinstance(found[0], dict):
                raise ValueError("missing or ambiguous store topology")
            weights = []
            for key, shard in sorted(found[0].items()):
                weight = shard.get("weight")
                if not re.fullmatch(r"[0-9]{1,2}", key) or type(weight) not in (int, float) or not 0 <= weight <= 1000000:
                    raise ValueError("invalid shard weight")
                weights.append({"key": int(key), "weight": weight})
            routes.append({"component": component, "store": store, "shard_count": len(found[0]), "shards": weights})
    required = {("ConfigMap", c) for c in STORES} | {("StatefulSet" if c == "storage" else "Deployment", c) for c in COMPONENTS}
    if seen != required:
        raise ValueError("incomplete object inventory")
    return workloads, routes, hashes


def capture(evidence, outcome):
    if outcome not in {"success", "failure", "cancelled", "skipped"}:
        raise ValueError("invalid install outcome")
    report = {"schema_version": 1, "install_outcome": outcome, "diagnostics_complete": False, "errors": [], "helm_calls": []}
    try:
        reject_overrides(os.environ)
        state = state_dir()
        metadata = json.loads(read_private(state / "metadata.json"))
        report.update({key: metadata[key] for key in ("helm", "ct", "binding", "input_sha256")})
        calls = [json.loads(line) for line in read_private(state / "helm-argv.jsonl").splitlines()]
        for args in calls:
            if safe_argv(args):
                report["helm_calls"].append({"argv": args})
            else:
                report["helm_calls"].append({"classification": "unrecognized-or-sensitive-argv", "argument_count": len(args)})
                report["errors"].append("unrecognized-or-sensitive-argv")
        installs = [c["argv"] for c in report["helm_calls"] if c.get("argv", [])[:1] == ["install"]]
        if len(installs) != 1:
            raise ValueError("missing or ambiguous install invocation")
        release, namespace = installs[0][1], installs[0][4]
        if outcome == "success" and not any(c.get("argv") == ["test", release, "--namespace", namespace] for c in report["helm_calls"]):
            report["errors"].append("missing-helm-test-argv")
        env = binding(state, metadata, dict(os.environ))
        objects = run_private([metadata["kubectl"], "--kubeconfig", str(state / "config.stdout"), "--context", CONTEXT, "get", "configmaps,statefulsets,deployments", "--namespace", namespace, "--selector=app.kubernetes.io/instance=" + release + ",app.kubernetes.io/name=cucina", "--output=json", "--request-timeout=20s"], "objects", state, env)
        report["workloads"], report["topology"], report["config_sha256"] = topology(json.loads(read_private(objects))["items"], release, namespace)
        report["diagnostics_complete"] = not report["errors"]
    except Exception as exc:
        report["errors"].append("capture-failed-" + type(exc).__name__)  # Never exception values.
    output = Path(evidence)
    directory(output.parent, create=True)
    write_json(output, report)  # Exclusive no-follow write; any failure fails this separate step.
    print("system evidence complete" if report["diagnostics_complete"] else "system evidence incomplete")
    # Publish only a file this step created safely, even when its contents say incomplete.
    # Never let the artifact action follow a refused pre-existing evidence symlink.
    marker = Path(os.environ["GITHUB_OUTPUT"]).absolute()
    if marker != marker.resolve() or marker == WORKSPACE or WORKSPACE in marker.parents:
        raise ValueError("unsafe output handle")
    fd = os.open(marker, os.O_WRONLY | os.O_APPEND | os.O_NOFOLLOW)
    with os.fdopen(fd, "wb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_size:
            raise ValueError("pre-existing output data")
        stream.write(b"evidence_ready=true\n")
    return 0 if report["diagnostics_complete"] else 1


if __name__ == "__main__":
    os.umask(0o077)
    try:
        if sys.argv[1:] == ["install"]:
            install()
        elif sys.argv[1:2] == ["--helm"]:
            helm()
        elif len(sys.argv) == 4 and sys.argv[1] == "capture":
            sys.exit(capture(sys.argv[2], sys.argv[3]))
        else:
            raise ValueError("invalid invocation")
    except Exception:
        print("system diagnostic operation failed", file=sys.stderr)
        sys.exit(1)
