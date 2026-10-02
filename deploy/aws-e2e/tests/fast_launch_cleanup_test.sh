#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# Guards: R-POOL-2 / §12 — exact child ownership, read-only inventory, and no parent deletion before child cleanup.
set -euo pipefail
if [[ -n "${TEST_SRCDIR:-}" ]]; then
  root="$TEST_SRCDIR/$TEST_WORKSPACE"
else
  root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
fi
python3 - "$root" <<'PY'
import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

# Stateful EC2 CLI fake. It never delegates to a real CLI; unknown operations fail.
fake = r'''#!/usr/bin/env python3
# SPDX-License-Identifier: FSL-1.1-ALv2
import json, os, sys
from pathlib import Path
p = Path(os.environ['FAKE_EC2_STATE'])
s = json.loads(p.read_text())
a = sys.argv[1:]
def val(k, default=None): return a[a.index(k)+1] if k in a else default
def words(k):
    if k not in a: return []
    out=[]
    for v in a[a.index(k)+1:]:
        if v.startswith('--'): break
        out.append(v)
    return out
def tags(o): return {t['Key']:t['Value'] for t in o.get('Tags',[])}
def matches(o):
    for f in words('--filters'):
        k,v=f.removeprefix('Name=').split(',Values=',1)
        got = tags(o).get(k[4:]) if k.startswith('tag:') else o.get('Description') if k=='description' else None
        if got not in v.split(','): return False
    return True
query=val('--query','')
result={}
if Path(sys.argv[0]).name=='sleep':
    if s.get('keep_children'): sys.exit(19) # deterministic bounded failure: no real wait
    if s['state']=='none': s['snapshots']=[x for x in s['snapshots'] if x['SnapshotId']=='snap-root']
elif a[0:2]==['sts','get-caller-identity']:
    result='123456789012'
else:
    if a[0]=='ec2': a=a[1:]
    if a and a[0]=='--region': a=a[2:]
    op=a[0]
    if op=='describe-images':
        rows=[s['image']] if s['image'] and matches(s['image']) else []
        result=(rows[0]['ImageId'] if rows else 'None') if query=='Images[0].ImageId' else {'Images':rows}
    elif op=='describe-launch-templates':
        rows=[s['template']] if matches(s['template']) else []
        result=[x['LaunchTemplateId'] for x in rows] if query=='LaunchTemplates[].LaunchTemplateId' else {'LaunchTemplates':rows}
    elif op=='describe-fast-launch-images':
        rows=[] if s['state']=='none' else [{'ImageId':'ami-owned','State':s['state'],'LaunchTemplate':{'LaunchTemplateId':'lt-owned'}}]
        result=('None' if not rows else rows[0]['State']) if '.State' in query else {'FastLaunchImages':rows}
    elif op=='describe-snapshots':
        rows=[x for x in s['snapshots'] if matches(x) and (not words('--snapshot-ids') or x['SnapshotId'] in words('--snapshot-ids'))]
        # Model the pre-fix substring query too, so its unwanted adoption is observable.
        result='\t'.join(x['SnapshotId'] for x in rows if 'ami-owned' in x.get('Description','')) if query.startswith('Snapshots[?contains') else {'Snapshots':rows}
    elif op=='create-tags':
        wanted={x[4:].split(',Value=',1)[0]:x.split(',Value=',1)[1] for x in words('--tags')}
        for x in s['snapshots']:
            if x['SnapshotId'] in words('--resources'):
                merged=tags(x); merged.update(wanted); x['Tags']=[{'Key':k,'Value':v} for k,v in merged.items()]
    elif op=='disable-fast-launch': s['state']='none'
    elif op=='deregister-image':
        if s['state']!='none' or any(x['SnapshotId']!='snap-root' for x in s['snapshots']): s['unsafe_delete']=True
        s['image']=None
    elif op=='delete-snapshot': s['snapshots']=[x for x in s['snapshots'] if x['SnapshotId']!=val('--snapshot-id')]
    else:
        allowed={'describe-instances','describe-volumes','describe-network-interfaces','describe-addresses','describe-security-groups','describe-subnets','describe-route-tables','describe-internet-gateways','describe-egress-only-internet-gateways','describe-vpc-endpoints','describe-vpcs','describe-key-pairs','describe-nat-gateways', 'iam','ecr','ssm','secretsmanager','resourcegroupstaggingapi'}
        if op not in allowed: raise RuntimeError('unexpected fake AWS command '+repr(a))
        result={k:[] for k in ['Reservations','Volumes','NetworkInterfaces','Addresses','SecurityGroups','Subnets','RouteTables','InternetGateways','EgressOnlyInternetGateways','VpcEndpoints','Vpcs','KeyPairs','NatGateways','RoleDetailList','Policies','InstanceProfiles','repositories','Parameters','SecretList','ResourceTagMappingList']}
p.write_text(json.dumps(s))
if isinstance(result,str): print(result)
else: print(json.dumps(result))
'''

root=Path(sys.argv[1])
cli=root/'workers/windows/scripts/fast-launch.sh'
sweep=root/'deploy/aws-e2e/scripts/sweep.sh'
campaign={'cucina:env':'e2e','cucina:run':'fixture-run','cucina:expires':'2099-01-01T00:00:00Z'}
def taglist(t): return [{'Key':k,'Value':v} for k,v in t.items()]
child={'SnapshotId':'snap-child','State':'completed','Description':'This is Fast Launch snapshot for image ami-owned',
       'Tags':taglist({'CreatedBy':'EC2 Fast Launch','CreatedByLaunchTemplateId':'lt-owned'})}
base={'state':'none','unsafe_delete':False,
      'image':{'ImageId':'ami-owned','Name':'fixture','State':'available','Tags':taglist(campaign),'BlockDeviceMappings':[{'Ebs':{'SnapshotId':'snap-root'}}]},
      'template':{'LaunchTemplateId':'lt-owned','LaunchTemplateName':'fixture','Tags':taglist(campaign)},
      'snapshots':[{'SnapshotId':'snap-root','State':'completed','Tags':taglist(campaign)},child]}
with tempfile.TemporaryDirectory(prefix='cucina-fast-launch-', dir=os.environ.get('TEST_TMPDIR')) as tmp:
    tmp=Path(tmp); (tmp/'bin').mkdir(); state=tmp/'state.json'
    for name in ['aws','sleep']:
        path=tmp/'bin'/name; path.write_text(fake); path.chmod(0o755)
    # Never expose developer credentials, SSO configuration or the real home to a subprocess, even a fake.
    home=tmp/'home'; home.mkdir(mode=0o700)
    env={key:os.environ[key] for key in ['PATH','TMPDIR','TMP','TEMP','TEST_TMPDIR','SYSTEMROOT'] if key in os.environ}
    env.update({'HOME':str(home), 'BASH_ENV':'/dev/null','PATH':str(tmp/'bin')+':'+env.get('PATH',os.defpath), 'FAKE_EC2_STATE':str(state),
                'AWS_CONFIG_FILE':str(home/'nonexistent-aws-config'),'AWS_SHARED_CREDENTIALS_FILE':str(home/'nonexistent-aws-credentials'),
                'CUCINA_RUN_ID':campaign['cucina:run'],'CUCINA_EXPIRES':campaign['cucina:expires'],'CUCINA_ENV':'e2e',
                'CUCINA_SECRETS_DIR':str(tmp/'private'),'TF_DATA_DIR':str(tmp/'tfdata'),'AWS_REGION':'us-west-1','AWS_DEFAULT_REGION':'us-west-1','AWS_PROFILE':'default',
                'AWS_EC2_METADATA_DISABLED':'true'})
    # Reporting missing-tag children must never perform tag reconciliation as a side effect.
    state.write_text(json.dumps(base))
    report=subprocess.run(['bash',str(sweep),'--report'],env=env,capture_output=True,text=True,timeout=20)
    assert report.returncode==1, report.stderr
    assert json.loads(state.read_text())==base, 'read-only sweep modified a resource'
    for action, script in [('disable',cli),('--delete-amis',sweep)]:
        for retained in [False,True]:
            data=copy.deepcopy(base); data['keep_children']=retained; state.write_text(json.dumps(data))
            cmd=['bash',str(script),action]+(['--ami','ami-owned'] if script==cli else [])
            run=subprocess.run(cmd,env=env,capture_output=True,text=True,timeout=20)
            after=json.loads(state.read_text())
            assert not after['unsafe_delete'], (action,'deregistered parent before replacement cleanup')
            if retained: assert run.returncode!=0 and after['image'], (action,'succeeded with leftover children')
            else: assert not any(x['SnapshotId']=='snap-child' for x in after['snapshots']), (action,'disabled state hid remaining child')
    for case in ['matching','prefix-image','foreign-template','conflicting-run','conflicting-env','conflicting-expires','untagged-parent','foreign-template-parent']:
        data=copy.deepcopy(base)
        if case=='prefix-image': data['snapshots'][1]['Description']+='-other'
        if case=='foreign-template': data['snapshots'][1]['Tags'][1]['Value']='lt-foreign'
        if case.startswith('conflicting-'): data['snapshots'][1]['Tags'].append({'Key':'cucina:'+case.split('-')[1], 'Value':'other'})
        if case=='untagged-parent': data['image']['Tags']=[]
        if case=='foreign-template-parent': data['template']['Tags']=taglist({**campaign,'cucina:run':'other'})
        before=copy.deepcopy(data['snapshots'][1]['Tags']); state.write_text(json.dumps(data))
        run=subprocess.run(['bash',str(cli),'tag','--ami','ami-owned'],env=env,capture_output=True,text=True,timeout=20)
        after=json.loads(state.read_text())['snapshots'][1]
        if case=='matching':
            assert run.returncode==0, run.stderr
            assert campaign.items() <= {t['Key']:t['Value'] for t in after['Tags']}.items()
        else:
            assert after['Tags']==before,(case,'changed a foreign/conflicting child')
            if case.startswith('conflicting-') or case in ['untagged-parent','foreign-template-parent']: assert run.returncode!=0,(case,'ownership failure was hidden')
print('Fast Launch CLI and teardown ownership cases passed (fake AWS only)')
PY
