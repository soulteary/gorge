import copy,importlib.util,json,pathlib,tempfile,unittest
HERE=pathlib.Path(__file__).resolve().parent
spec=importlib.util.spec_from_file_location('suite',HERE/'run.py');m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
class FixtureTests(unittest.TestCase):
 def setUp(self):self.lock=json.loads((HERE/'images.json').read_text())
 def test_six_exact_immutable_sources_are_required(self):
  m.validate_lock(self.lock)
  for change in ['tag','revision','extra','source']:
   lock=copy.deepcopy(self.lock)
   if change=='tag':lock['services']['owlmail']['image']='ghcr.io/soulteary/owlmail:latest'
   if change=='revision':lock['services']['stargate']['revision']='main'
   if change=='extra':lock['services']['extra']={}
   if change=='source':lock['services']['herald']['image']='untrusted.invalid/herald@sha256:'+'a'*64
   with self.subTest(change=change),self.assertRaises(ValueError):m.validate_lock(lock)
 def test_fixture_has_no_host_ports_or_deployment_inputs(self):
  with tempfile.TemporaryDirectory() as tmp:
   cfg,keys,user=m.fixture(pathlib.Path(tmp),self.lock)
   self.assertEqual(set(cfg['services']),m.SERVICES);self.assertTrue(cfg['networks']['default']['internal'])
   for service in cfg['services'].values():
    self.assertNotIn('ports',service);self.assertEqual(service['labels']['traefik.enable'],'false');self.assertEqual(service['restart'],'no')
   self.assertEqual(user,'fixture@example.invalid');self.assertEqual(len(set(keys.values())),7)
   self.assertEqual(cfg['services']['owlmail']['environment']['OWLMAIL_SMTP_AUTH_REQUIRE_TLS'],'true')
   self.assertEqual(cfg['services']['herald']['environment']['SSL_CERT_FILE'],'/fixture/cert.pem')
   self.assertNotIn('OWLMAIL_OUTGOING_HOST',cfg['services']['owlmail']['environment'])
 def test_each_run_generates_independent_fixture_keys(self):
  with tempfile.TemporaryDirectory() as a,tempfile.TemporaryDirectory() as b:
   first=m.fixture(pathlib.Path(a),self.lock)[1];second=m.fixture(pathlib.Path(b),self.lock)[1]
   self.assertTrue(all(first[k]!=second[k] for k in first))
 def test_remote_or_overridden_docker_targets_are_refused(self):
  m.validate_docker_target({},'unix:///var/run/docker.sock')
  with self.assertRaises(ValueError):m.validate_docker_target({},'unix:///var/run/docker.sock','remote-context')
  for env,target in [({'DOCKER_HOST':'tcp://remote.invalid'},'unix:///var/run/docker.sock'),({'DOCKER_CONTEXT':'other'},'unix:///var/run/docker.sock'),({},'tcp://remote.invalid')]:
   with self.subTest(env=env,target=target),self.assertRaises(ValueError):m.validate_docker_target(env,target)
if __name__=='__main__':unittest.main()
