"""Fail-closed path selection and aggregation for the always-emitted required check."""
import json,os,re,subprocess,sys
from pathlib import Path

def needs_runtime(paths):
    # Only known documentation/report files avoid expensive runtime jobs.
    # Unknown paths (including future services, tests, scripts and locks) run all jobs.
    return any(not ((p.endswith('.md') and ('/' not in p or p.startswith(('docs/','compat/')))) or p in {'.github/coverage.svg','.github/goreportcard.svg','.github/go-test-report.json'} or p.startswith('docs/') and p.endswith(('.png','.svg'))) for p in paths)

def verify(needs, runtime):
    if runtime not in ('true','false') or not {'changes','policy'}<=set(needs) or len(needs)<3:
        raise ValueError('Missing runtime decision or runtime jobs')
    for name,job in needs.items():
        expected='success' if name in ('changes','policy') or runtime=='true' else 'skipped'
        if job.get('result')!=expected:
            raise ValueError('Required job did not finish as expected: '+name)

def select():
    event=os.environ['GITHUB_EVENT_NAME'];base=os.environ.get('BASE_SHA','');head=os.environ['GITHUB_SHA']
    if event not in ('push','pull_request') or not re.fullmatch('[0-9a-f]{40}',base) or set(base)=={'0'}:
        result=True
    else:
        if not re.fullmatch('[0-9a-f]{40}',head):raise ValueError('Invalid source revision')
        paths=subprocess.check_output(['git','diff','--no-renames','--name-only','-z',base,head]).decode().split('\0')
        result=needs_runtime([p for p in paths if p])
    with open(os.environ['GITHUB_OUTPUT'],'a') as f:f.write('runtime='+str(result).lower()+'\n')

if __name__=='__main__':
    if sys.argv[1:] == ['select']:select()
    elif sys.argv[1:] == ['verify']:verify(json.loads(os.environ['NEEDS_JSON']),os.environ['RUNTIME_REQUIRED'])
    else:raise SystemExit('Use select or verify')
