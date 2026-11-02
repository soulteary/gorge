"""Inspect/apply additive main protection after observing the exact required check.
No credentials are accepted or printed; gh uses its existing authenticated session.
"""
import argparse,json,re,subprocess
from pathlib import Path
REPO='repos/soulteary/gorge'
def api(path,method='GET',body=None):
 cmd=['gh','api',REPO+path,'--method',method]
 if body is not None:cmd+=['--input','-']
 return json.loads(subprocess.check_output(cmd,input=json.dumps(body).encode() if body is not None else None))
def desired(previous,policy):
 old=(previous or {}).get('required_status_checks') or {}
 checks=old.get('checks') or [{'context':c} for c in old.get('contexts',[])]
 checks=[c for c in checks if c['context']!=policy['check']]+[{'context':policy['check'],'app_id':policy['app_id']}]
 reviews=(previous or {}).get('required_pull_request_reviews') or {}
 return {'required_status_checks':{'strict':True,'checks':[{k:v for k,v in c.items() if k in ('context','app_id')} for c in checks]},'enforce_admins':True,'required_pull_request_reviews':{'dismiss_stale_reviews':True,'require_code_owner_reviews':reviews.get('require_code_owner_reviews',False),'required_approving_review_count':max(reviews.get('required_approving_review_count',0),policy['required_approving_review_count'])},'restrictions':(previous or {}).get('restrictions'),'required_linear_history':True,'allow_force_pushes':False,'allow_deletions':False,'required_conversation_resolution':True}
def main():
 parser=argparse.ArgumentParser();parser.add_argument('--apply',action='store_true');parser.add_argument('--verified-ref');args=parser.parse_args()
 policy=json.loads((Path(__file__).parents[2]/'governance/required-main.json').read_text())
 branch=api('/branches/main');previous=api('/branches/main/protection') if branch['protected'] else None
 if args.apply:
  if not args.verified_ref or not re.fullmatch('[a-f0-9]{40}',args.verified_ref):raise SystemExit('Provide the exact tested revision')
  runs=api('/commits/'+args.verified_ref+'/check-runs?per_page=100')['check_runs']
  matching=[r for r in runs if r['name']==policy['check'] and r['app']['id']==policy['app_id']]
  if not matching or matching[0]['conclusion']!='success' or matching[0]['status']!='completed':raise SystemExit('Exact required-check identity has not succeeded')
  api('/branches/main/protection','PUT',desired(previous,policy))
 actual=api('/branches/main/protection') if args.apply or previous else None
 ok=bool(actual and actual['required_status_checks']['strict'] and any(c['context']==policy['check'] and c['app_id']==policy['app_id'] for c in actual['required_status_checks']['checks']) and actual['enforce_admins']['enabled'] and actual['required_conversation_resolution']['enabled'] and actual['required_pull_request_reviews']['required_approving_review_count']>=policy['required_approving_review_count'])
 print(json.dumps({'repository':'soulteary/gorge','branch':'main','applied':args.apply,'effective':ok,'requiredCheck':policy['check'],'checkApplication':policy['app_id'],'manualFinalHeadReviewStillRequired':True}))
 if not ok:raise SystemExit(1)
if __name__=='__main__':main()
