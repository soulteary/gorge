import importlib.util,json,os,re,unittest,subprocess,tempfile
from pathlib import Path
HERE=Path(__file__).parent
spec=importlib.util.spec_from_file_location('gate',HERE/'required_ci.py');gate=importlib.util.module_from_spec(spec);spec.loader.exec_module(gate)
class RequiredCITests(unittest.TestCase):
 def test_unknown_runtime_workflow_and_lock_paths_require_full_ci(self):
  for path in ['go/internal/fixture.md','go/internal/new/file.go','tests/new.py','.github/workflows/new.yml','deploy/release/source-pair.json','new-service.conf','Makefile']:
   with self.subTest(path=path):self.assertTrue(gate.needs_runtime([path]))
 def test_only_known_docs_and_generated_reports_skip_runtime(self):
  self.assertFalse(gate.needs_runtime(['README.md','docs/testing.md','.github/go-test-report.json','.github/coverage.svg']))
  self.assertTrue(gate.needs_runtime(['docs/testing.md','go/go.sum']))
 def test_success_and_intentional_docs_skip_are_distinct(self):
  gate.verify({'changes':{'result':'success'},'policy':{'result':'success'},'test':{'result':'success'}},'true')
  gate.verify({'changes':{'result':'success'},'policy':{'result':'success'},'test':{'result':'skipped'}},'false')
 def test_missing_failure_cancellation_and_unexpected_skip_fail_closed(self):
  for state in ['failure','cancelled','skipped',None]:
   with self.subTest(state=state),self.assertRaises(ValueError):gate.verify({'changes':{'result':'success'},'policy':{'result':'success'},'test':{'result':state}},'true')
  with self.assertRaises(ValueError):gate.verify({'changes':{'result':'success'},'policy':{'result':'failure'},'test':{'result':'skipped'}},'false')
  with self.assertRaises(ValueError):gate.verify({'changes':{'result':'success'},'policy':{'result':'success'}},'false')
 def test_every_workflow_runtime_job_is_aggregated(self):
  text=(HERE.parents[1]/'workflows/ci.yml').read_text();jobs=text.split('\njobs:\n',1)[1]
  names=set(re.findall(r'^  ([a-z][a-z0-9-]*):$',jobs,re.M));required=jobs.split('  required:\n',1)[1]
  match=re.search(r'    needs: \[([^\]]+)\]',required);self.assertIsNotNone(match)
  self.assertEqual(set(x.strip() for x in match[1].split(',')),names-{'required'})
  trigger=text.split('\nenv:',1)[0];self.assertNotIn('paths:',trigger)
  for name in names-{'changes','policy','required'}:
   block=re.search(r'^  '+re.escape(name)+r':\n(.*?)(?=^  [a-z][a-z0-9-]*:|\Z)',jobs,re.M|re.S)[1]
   self.assertIn('needs: changes',block);self.assertIn("needs.changes.outputs.runtime == 'true'",block)
 def test_existing_required_checks_and_stronger_review_count_are_preserved(self):
  spec=importlib.util.spec_from_file_location('protection',HERE/'protect_main.py');m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
  policy={'check':'Gorge required CI','app_id':15368,'required_approving_review_count':0}
  old={'required_status_checks':{'checks':[{'context':'existing','app_id':42}]},'required_pull_request_reviews':{'required_approving_review_count':2,'require_code_owner_reviews':True}}
  result=m.desired(old,policy)
  self.assertIn({'context':'existing','app_id':42},result['required_status_checks']['checks'])
  self.assertEqual(result['required_pull_request_reviews']['required_approving_review_count'],2)
  self.assertTrue(result['required_pull_request_reviews']['require_code_owner_reviews'])
  self.assertTrue(result['enforce_admins']);self.assertFalse(result['allow_force_pushes'])
 def test_actual_git_rename_from_runtime_to_documentation_runs_full_ci(self):
  with tempfile.TemporaryDirectory() as tmp:
   root=Path(tmp)
   def git(*args):return subprocess.check_output(['git','-c','commit.gpgsign=false','-c','core.hooksPath=/dev/null',*args],cwd=root,stderr=subprocess.DEVNULL).decode().strip()
   git('init');(root/'go/cmd/service').mkdir(parents=True);(root/'go/cmd/service/main.go').write_text('package main\nfunc main() {}\n')
   git('add','.');git('-c','user.name=Fixture','-c','user.email=fixture@example.invalid','commit','-m','runtime fixture');base=git('rev-parse','HEAD')
   (root/'docs').mkdir();git('mv','go/cmd/service/main.go','docs/archived-main.md');git('-c','user.name=Fixture','-c','user.email=fixture@example.invalid','commit','-am','move fixture');head=git('rev-parse','HEAD')
   output=root/'output';env={**os.environ,'GITHUB_EVENT_NAME':'pull_request','BASE_SHA':base,'GITHUB_SHA':head,'GITHUB_OUTPUT':str(output)}
   subprocess.run(['python3',str((HERE/'required_ci.py').resolve()),'select'],cwd=root,env=env,check=True)
   self.assertEqual(output.read_text(),'runtime=true\n')
 def test_effective_policy_refuses_each_protection_drift(self):
  import copy
  spec=importlib.util.spec_from_file_location('protection',HERE/'protect_main.py');m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
  policy={'check':'Gorge required CI','app_id':15368,'required_approving_review_count':0}
  actual={'required_status_checks':{'strict':True,'checks':[{'context':policy['check'],'app_id':15368}]},'enforce_admins':{'enabled':True},'required_conversation_resolution':{'enabled':True},'required_pull_request_reviews':{'required_approving_review_count':0,'dismiss_stale_reviews':True},'required_linear_history':{'enabled':True},'allow_force_pushes':{'enabled':False},'allow_deletions':{'enabled':False}}
  self.assertTrue(m.effective(actual,policy))
  for key,value in [('required_linear_history',False),('allow_force_pushes',True),('allow_deletions',True)]:
   changed=copy.deepcopy(actual);changed[key]['enabled']=value
   with self.subTest(key=key):self.assertFalse(m.effective(changed,policy))
  actual['required_pull_request_reviews']['dismiss_stale_reviews']=False;self.assertFalse(m.effective(actual,policy))
if __name__=='__main__':unittest.main()
