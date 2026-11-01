import base64,importlib.util,json,unittest,urllib.error
from pathlib import Path
from unittest.mock import Mock
spec=importlib.util.spec_from_file_location('image_metadata',Path(__file__).with_name('registry_metadata.py'));m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
SHA='a'*40;TAG='2026.11.02-r1';REPO='soulteary/gorge'
def fixture():
    records={};children=[]
    for arch in ('amd64','arm64'):
        config={'architecture':arch,'os':'linux','config':{'Labels':{'org.opencontainers.image.revision':SHA,'org.opencontainers.image.source':'https://github.com/'+REPO,'org.opencontainers.image.version':TAG}}}
        config_raw=json.dumps(config).encode();config_digest=m.digest(config_raw);records[config_digest]=config
        child=json.dumps({'schemaVersion':2,'config':{'digest':config_digest}}).encode();child_digest=m.digest(child);records[child_digest]=child;children.append({'digest':child_digest,'platform':{'os':'linux','architecture':arch}})
    raw=json.dumps({'schemaVersion':2,'manifests':children}).encode();records[TAG]=raw;records['sha-'+SHA]=raw
    registry=Mock();registry.manifest.side_effect=lambda ref,missing=False:records.get(ref);registry.config.side_effect=lambda ref:records[ref]
    return registry,records,raw
class ImagePublicationTests(unittest.TestCase):
    def test_exact_version_source_and_two_architectures(self):
        registry,_,raw=fixture();r=m.inspect(registry,REPO,TAG,SHA);self.assertTrue(r['exists']);self.assertEqual(r['digest'],m.digest(raw));self.assertEqual(r['platforms'],['amd64','arm64'])
    def test_absent_pair_is_not_an_existing_publication(self):
        registry=Mock();registry.manifest.return_value=None;self.assertFalse(m.inspect(registry,REPO,TAG,SHA)['exists'])
    def test_partial_or_disagreeing_tags_refuse_overwrite(self):
        for value in (None,b'other-image'):
            registry,records,_=fixture();records['sha-'+SHA]=value
            with self.assertRaises(ValueError):m.inspect(registry,REPO,TAG,SHA)
    def test_wrong_source_version_platform_or_missing_architecture_refused(self):
        for change in ('source','version','platform','missing'):
            registry,records,raw=fixture();index=json.loads(raw)
            if change=='missing':index['manifests'].pop();raw=json.dumps(index).encode()
            else:
                first=json.loads(records[index['manifests'][0]['digest']]);config=records[first['config']['digest']]
                if change=='source':config['config']['Labels']['org.opencontainers.image.revision']='b'*40
                elif change=='version':config['config']['Labels']['org.opencontainers.image.version']='2026.11.01-r3'
                else:config['architecture']='arm64'
            with self.assertRaises(ValueError):m.validate_index(raw,registry,REPO,TAG,SHA)
    def test_child_digest_tampering_refused(self):
        registry,records,raw=fixture();records[json.loads(raw)['manifests'][0]['digest']]=b'tampered'
        with self.assertRaises(ValueError):m.validate_index(raw,registry,REPO,TAG,SHA)
    def test_only_explicit_404_is_missing(self):
        for code in (401,403,404,429,500,503):
            r=m.Registry(REPO);r.token='isolated-test';r.get=Mock(side_effect=urllib.error.HTTPError('https://ghcr.io/',code,'failure',{},None))
            if code==404:self.assertIsNone(r.manifest(TAG,missing=True))
            else:
                with self.assertRaises(urllib.error.HTTPError):r.manifest(TAG,missing=True)
    def test_config_digest_tampering_refused(self):
        r=m.Registry(REPO);r.token='isolated-test';r.get=Mock(return_value=b'tampered')
        with self.assertRaises(ValueError):r.config('sha256:'+'a'*64)
    def test_exact_existing_tag_identity_required(self):
        self.assertTrue(m.identity(REPO,TAG,SHA,'refs/tags/'+TAG))
        for tag,sha,ref in [('latest',SHA,'refs/heads/main'),('2026.02.30-r1',SHA,'refs/tags/2026.02.30-r1'),(TAG,'short','refs/tags/'+TAG),(TAG,SHA,'refs/heads/main')]:
            with self.assertRaises(ValueError):m.identity(REPO,tag,sha,ref)
    def test_redirected_blob_never_gets_registry_credentials(self):
        r=m.Registry(REPO);r.token='isolated-test';response=Mock();response.__enter__=Mock(return_value=response);response.__exit__=Mock(return_value=False);response.read.return_value=b'blob';r.opener=Mock();r.opener.open.side_effect=[urllib.error.HTTPError('https://ghcr.io/',302,'redirect',{'Location':'https://pkg-containers.githubusercontent.com/blob'},None),response]
        self.assertEqual(r.get('https://ghcr.io/v2/'+REPO+'/blobs/test',authorized=True,blob=True),b'blob')
        first=r.opener.open.call_args_list[0].args[0];second=r.opener.open.call_args_list[1].args[0];self.assertTrue(first.has_header('Authorization'));self.assertFalse(second.has_header('Authorization'))
    def test_unsafe_blob_redirect_refused(self):
        for target in ('http://objects.example.invalid/blob','https://user:password@objects.example.invalid/blob','file:///tmp/blob','https://127.0.0.1/blob','https://pkg-containers.githubusercontent.com:444/blob'):
            r=m.Registry(REPO);r.token='isolated-test';r.opener=Mock();r.opener.open.side_effect=urllib.error.HTTPError('https://ghcr.io/',302,'redirect',{'Location':target},None)
            with self.assertRaises(ValueError):r.get('https://ghcr.io/v2/'+REPO+'/blobs/test',authorized=True,blob=True)

    def test_private_token_exchange_uses_workflow_credentials(self):
        r=m.Registry(REPO,'release-user','isolated-package-token');response=Mock();response.__enter__=Mock(return_value=response);response.__exit__=Mock(return_value=False);response.read.return_value=b'{"token":"scoped-pull"}';r.opener=Mock();r.opener.open.return_value=response
        r.authenticate();request=r.opener.open.call_args.args[0]
        self.assertEqual(request.get_header('Authorization'),'Basic '+base64.b64encode(b'release-user:isolated-package-token').decode());self.assertTrue(request.full_url.startswith('https://ghcr.io/token?'));self.assertEqual(r.token,'scoped-pull')
    def test_token_exchange_redirect_does_not_forward_credentials(self):
        r=m.Registry(REPO,'release-user','isolated-package-token');r.opener=Mock();r.opener.open.side_effect=urllib.error.HTTPError('https://ghcr.io/token',302,'redirect',{'Location':'https://objects.example.invalid/token'},None)
        with self.assertRaises(urllib.error.HTTPError):r.authenticate()
        self.assertEqual(r.opener.open.call_count,1)
    def test_incomplete_exchange_credentials_refused(self):
        for user,password in [('release-user',None),(None,'isolated-package-token')]:
            with self.assertRaises(ValueError):m.Registry(REPO,user,password)
    def test_credentials_require_fixed_origins(self):
        r=m.Registry(REPO,'release-user','isolated-package-token');r.token='scoped-pull'
        for flag in ('exchange','authorized'):
            with self.assertRaises(ValueError):r.get('https://objects.example.invalid/token',**{flag:True})
