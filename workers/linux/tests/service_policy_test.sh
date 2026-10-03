#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# Guards: NFR-M2 / R-CACHE-2 — fresh resolved disk L1 gets worker-only reclaim;
# memory L1 and unrelated policy survive; bootstrap cannot start on policy failure.
set -euo pipefail
if [[ -n "${TEST_SRCDIR:-}" ]]; then
  root="$TEST_SRCDIR/$TEST_WORKSPACE/workers/linux"
else
  root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
fi
exec python3 - "$root" <<'PY'
import configparser
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

source = Path(sys.argv[1])

def unit(name):
    # systemd allows repeated list directives (e.g. ConditionPathExists).
    cfg = configparser.ConfigParser(interpolation=None, strict=False)
    cfg.read(source / 'files/systemd' / name)
    return cfg

worker, runner = unit('bb-worker.service'), unit('bb-runner.service')
assert 'bb-runner.service' in worker['Unit']['After'].split()
assert 'bb-runner.service' in worker['Unit']['Requires'].split()
assert runner['Service']['ExecStartPre'] == '/opt/cucina/bin/cucina-bootstrap'
for cfg in (worker, runner):
    assert 'MemoryHigh' not in cfg['Service'], 'no unconditional/shared memory policy'
    assert 'MemoryMax' not in cfg['Service'], 'hard limits are not part of this fix'

# These command fakes expose resulting state, not expected call sequences.
agent = '''#!/bin/sh
[ "$#" = 1 ] && [ "$1" = bootstrap ] || exit 90
if [ "${FAKE_AGENT_EXIT:-0}" != 0 ]; then exit "$FAKE_AGENT_EXIT"; fi
# A stale pre-bootstrap env must never choose the policy.
cp "$FAKE_ROOT/enrolled.env" "$FAKE_ROOT/etc/cucina/env"
'''
systemctl = '''#!/usr/bin/env python3
import configparser,json,os,sys
from pathlib import Path
assert sys.argv[1:] == ['daemon-reload'], 'only a manager reload is permitted'
root=Path(ROOT_LITERAL)
if (root/'reject-reload').exists(): sys.exit(7)
policy=root/'run/systemd/system/bb-worker.service.d/20-cucina-memory-high.conf'
c=configparser.ConfigParser(interpolation=None)
if policy.exists(): c.read(policy)
(root/'reloaded.json').write_text(json.dumps({'worker_high':c.get('Service','MemoryHigh',fallback=None)}))
'''

def executable(path, content):
    path.write_text(content)
    path.chmod(0o755)

# One acceptance table covers the public bootstrap -> installed helper ->
# shipped drop-in boundary. No mounts, real systemctl, cloud or live services.
cases = [
    ('ebs', "CUCINA_L1_PLACEMENT='ebs'\n", 'disk', ''),
    ('root disk', "CUCINA_L1_PLACEMENT='root-disk'\n", 'disk', ''),
    ('instance store', "CUCINA_L1_PLACEMENT='instance-store'\n", 'disk', ''),
    ('memory', "CUCINA_L1_PLACEMENT='memory'\n", 'memory', ''),
    ('disk to memory', "CUCINA_L1_PLACEMENT='memory'\n", 'memory', 'prime'),
    ('disk reapply', "CUCINA_L1_PLACEMENT='ebs'\n", 'disk', 'prime'),
    ('missing', "UNRELATED='value'\n", 'reject', ''),
    ('unresolved auto', "CUCINA_L1_PLACEMENT='auto'\n", 'reject', ''),
    ('unknown', "CUCINA_L1_PLACEMENT='unknown'\n", 'reject', ''),
    ('duplicate', "CUCINA_L1_PLACEMENT='memory'\nCUCINA_L1_PLACEMENT='ebs'\n", 'reject', ''),
    ('duplicate identical', "CUCINA_L1_PLACEMENT='ebs'\nCUCINA_L1_PLACEMENT='ebs'\n", 'reject', ''),
    ('noncanonical assignment', "export CUCINA_L1_PLACEMENT='ebs'\n", 'reject', ''),
    ('shell expression', 'CUCINA_L1_PLACEMENT=$(touch "$FAKE_ROOT/evaluated")\n', 'reject', ''),
    ('unrelated env not evaluated', 'UNRELATED=$(touch "$FAKE_ROOT/evaluated")\nCUCINA_L1_PLACEMENT=\'ebs\'\n', 'disk', ''),
    ('symlink file', "CUCINA_L1_PLACEMENT='ebs'\n", 'reject', 'symlink-file'),
    ('symlink directory', "CUCINA_L1_PLACEMENT='ebs'\n", 'reject', 'symlink-dir'),
    ('symlink unit root', "CUCINA_L1_PLACEMENT='memory'\n", 'reject', 'symlink-root'),
    ('unowned file', "CUCINA_L1_PLACEMENT='memory'\n", 'reject', 'unowned'),
    ('hardlinked file', "CUCINA_L1_PLACEMENT='memory'\n", 'reject', 'hardlink'),
    ('unsafe writable directory', "CUCINA_L1_PLACEMENT='ebs'\n", 'reject', 'writable-dir'),
    ('symlink env', "CUCINA_L1_PLACEMENT='ebs'\n", 'reject', 'symlink-env'),
    ('reload failure and retry', "CUCINA_L1_PLACEMENT='ebs'\n", 'reload-failure', ''),
    ('not a worker', "CUCINA_L1_PLACEMENT='auto'\n", 'not-worker', ''),
    ('agent absent', "CUCINA_L1_PLACEMENT='auto'\n", 'agent-absent', ''),
    ('enrollment failure', "CUCINA_L1_PLACEMENT='ebs'\n", 'agent-failure', ''),
]

for name, fresh_env, expected, setup in cases:
    with tempfile.TemporaryDirectory(prefix='cucina-policy-') as tmp:
        root = Path(tmp).resolve()  # avoid macOS's /var -> /private/var alias
        bindir = root / 'opt/cucina/bin'
        etc = root / 'etc/cucina'
        units = root / 'run/systemd/system'
        dropdir = units / 'bb-worker.service.d'
        for d in (bindir, etc, dropdir, root / 'usr/bin'):
            d.mkdir(parents=True, mode=0o755)
        owned = dropdir / '20-cucina-memory-high.conf'
        unrelated = dropdir / '90-operator.conf'
        unrelated.write_text('[Service]\nCPUWeight=123\n')
        runner_dir = units / 'bb-runner.service.d'
        runner_dir.mkdir()
        runner_policy = runner_dir / '90-operator.conf'
        runner_policy.write_text('[Service]\nMemoryMax=123456\n')
        shutil.copyfile(source/'files/bin/cucina-bootstrap', bindir/'cucina-bootstrap')
        shutil.copyfile(source/'files/bin/cucina-worker-memory-policy', bindir/'cucina-worker-memory-policy')
        for p in bindir.iterdir(): p.chmod(0o755)
        executable(bindir/'cucina-worker-agent', agent)
        executable(root/'usr/bin/systemctl', systemctl.replace('ROOT_LITERAL', repr(str(root))))
        (etc/'env').write_text("CUCINA_L1_PLACEMENT='memory'\n")
        (root/'enrolled.env').write_text(fresh_env)
        env = dict(os.environ, FAKE_ROOT=str(root), CUCINA_L1_PLACEMENT='memory')
        # Do not let a developer's unrelated shell values control the fakes.
        env.pop('FAKE_AGENT_EXIT', None)
        env.pop('FAKE_RELOAD_FAIL', None)
        def boot():
            return subprocess.run(['bash', str(bindir/'cucina-bootstrap'), str(root)],
                                  env=env, capture_output=True, text=True, timeout=20)
        if setup in ('prime', 'hardlink'):
            (root/'enrolled.env').write_text("CUCINA_L1_PLACEMENT='ebs'\n")
            r = boot()
            assert r.returncode == 0 and owned.is_file(), (name, 'disk precondition', r.stderr)
            (root/'enrolled.env').write_text(fresh_env)
            (root/'reloaded.json').unlink()
        if setup == 'symlink-file':
            (root/'outside').write_text('not owned\n')
            owned.symlink_to(root/'outside')
        elif setup == 'symlink-dir':
            moved = root/'outside-dir'
            dropdir.rename(moved)
            dropdir.symlink_to(moved, target_is_directory=True)
        elif setup == 'symlink-root':
            moved = root/'outside-units'
            units.rename(moved)
            units.symlink_to(moved, target_is_directory=True)
        elif setup == 'unowned':
            owned.write_text('[Service]\nMemoryHigh=1024M\n')
        elif setup == 'hardlink':
            os.link(owned, root/'outside')
        elif setup == 'writable-dir':
            dropdir.chmod(0o777)
        elif setup == 'symlink-env':
            # The fake agent writes through it; the policy must still refuse
            # symlinked configuration rather than accepting that destination.
            (etc/'env').unlink()
            (root/'outside').write_text('not owned\n')
            (etc/'env').symlink_to(root/'outside')
        if expected == 'not-worker': env['FAKE_AGENT_EXIT'] = '2'
        if expected == 'agent-failure': env['FAKE_AGENT_EXIT'] = '23'
        if expected == 'agent-absent': (bindir/'cucina-worker-agent').unlink()
        if expected == 'reload-failure': (root/'reject-reload').touch()
        prior_owned = owned.read_bytes() if owned.exists() else None
        r = boot()
        if expected in ('reject', 'reload-failure', 'agent-failure'):
            assert r.returncode != 0, (name, 'unsafe/bootstrap failure accepted')
            assert not (root/'reloaded.json').exists(), (name, 'failed policy authorized worker start')
            if expected == 'reject':
                assert (owned.read_bytes() if owned.exists() else None) == prior_owned, (name, 'unowned destination changed')
            if expected == 'agent-failure': assert r.returncode == 23
            if expected == 'reload-failure':
                (root/'reject-reload').unlink()
                r = boot()
                assert r.returncode == 0, (name, r.stderr)
                assert json.loads((root/'reloaded.json').read_text())['worker_high'] == '768M'
        elif expected in ('not-worker', 'agent-absent'):
            assert r.returncode == 0 and not owned.exists() and not (root/'reloaded.json').exists(), (name, r.stderr)
            if expected == 'not-worker': assert (root/'run/cucina/not-a-worker').is_file()
        else:
            assert r.returncode == 0, (name, r.stderr)
            assert owned.exists() == (expected == 'disk'), (name, 'resolved disk placement needs its worker-only policy')
            state = json.loads((root/'reloaded.json').read_text())
            assert state['worker_high'] == ('768M' if expected == 'disk' else None), (name, state)
        assert not (root/'evaluated').exists(), (name, 'env was evaluated as shell')
        assert unrelated.read_text() == '[Service]\nCPUWeight=123\n', name
        assert runner_policy.read_text() == '[Service]\nMemoryMax=123456\n', name
        if setup == 'symlink-file': assert (root/'outside').read_text() == 'not owned\n'
    print('PASS', name)
PY
