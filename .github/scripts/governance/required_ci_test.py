import importlib.util,json,os,re,unittest
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
if __name__=='__main__':unittest.main()
