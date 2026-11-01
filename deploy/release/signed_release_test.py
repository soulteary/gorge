import copy
import json
import unittest

from component_images_test import fixture,REPO,TAG,SHA
from manifest import SERVICES
from signed_release import assemble,record


def evidence(manifest,service):
    expected=manifest['images'][service]['digest']
    published={'service':service,'repository':REPO,'release':TAG,'sourceRevision':SHA,
        'phorgeRevision':manifest['phorgeCommit'],'digest':expected,
        'aliases':[service+'-'+TAG,service+'-sha-'+SHA],'platforms':['amd64','arm64'],
        'exists':True,'overwritePermitted':False}
    cosign=json.dumps([{'critical':{'identity':{'docker-reference':'ghcr.io/'+REPO+'@'+expected},'image':{'docker-manifest-digest':expected}}}]).encode()
    attestation=json.dumps([{'verificationResult':{'statement':{'subject':[{'name':'ghcr.io/'+REPO,'digest':{'sha256':expected[7:]}}]}}}]).encode()
    return published,cosign,attestation


class SignedPublication(unittest.TestCase):
    def test_verified_subjects_bind_all_fourteen_components(self):
        manifest,*_=fixture();reports=[record(*evidence(manifest,s)) for s in SERVICES]
        value=assemble(manifest,reports,REPO,TAG,SHA)
        self.assertEqual(set(value['imageVerification']['components']),set(SERVICES))
        self.assertNotIn('imageVerification',manifest)
        self.assertEqual(value['images'],manifest['images'])

    def test_record_rejects_absent_or_partial_external_results(self):
        manifest,*_=fixture();p,c,a=evidence(manifest,'image')
        for left,right in [(b'[]',a),(c,b'[]'),(b'{}',a),(c,b'null')]:
            with self.assertRaises(ValueError):record(p,left,right)
        for key in ('exists','overwritePermitted'):
            bad=copy.deepcopy(p);bad[key]=not bad[key]
            with self.assertRaises(ValueError):record(bad,c,a)

    def test_record_rejects_wrong_signature_or_attestation_subject(self):
        manifest,*_=fixture();p,c,a=evidence(manifest,'image')
        for original in (c,a):
            wrong=original.replace(p['digest'][7:].encode(),b'c'*64)
            with self.assertRaises(ValueError):record(p,wrong if original is c else c,wrong if original is a else a)
        with self.assertRaises(ValueError):record(p,c.replace(REPO.encode(),b'other/gorge'),a)

    def test_legacy_bare_reference_remains_accepted(self):
        manifest,*_=fixture();p,c,a=evidence(manifest,'image')
        self.assertTrue(record(p,c.replace(('@'+p['digest']).encode(),b''),a)['signatureVerified'])

    def test_v3_reference_must_bind_the_exact_digest_not_a_tag(self):
        manifest,*_=fixture();p,c,a=evidence(manifest,'image')
        for reference in ('ghcr.io/'+REPO+'@sha256:'+'f'*64,'ghcr.io/'+REPO+':latest','ghcr.io/'+REPO+'-other@'+p['digest']):
            value=json.loads(c);value[0]['critical']['identity']['docker-reference']=reference
            with self.assertRaises(ValueError):record(p,json.dumps(value).encode(),a)

    def test_assembly_rejects_missing_duplicate_or_unknown_components(self):
        manifest,*_=fixture();reports=[record(*evidence(manifest,s)) for s in SERVICES]
        for values in (reports[:-1],reports+[reports[0]],[dict(reports[0],service='unknown')]+reports[1:]):
            with self.assertRaises(ValueError):assemble(manifest,values,REPO,TAG,SHA)

    def test_assembly_rejects_any_subject_identity_or_verification_drift(self):
        manifest,*_=fixture();reports=[record(*evidence(manifest,s)) for s in SERVICES]
        for key,value in [('signatureVerified',False),('attestationVerified',False),('sourceRevision','c'*40),
                          ('phorgeRevision','d'*40),('release','latest'),('exists',False),('overwritePermitted',True),
                          ('verificationWorkflow','other'),('platforms',['amd64']),('digest','sha256:'+'f'*64),
                          ('aliases',['latest']),('cosignVerificationSHA256','short'),('attestationVerificationSHA256','short')]:
            bad=copy.deepcopy(reports);bad[0][key]=value
            with self.assertRaises(ValueError):assemble(manifest,bad,REPO,TAG,SHA)

    def test_assembly_rejects_truthy_non_boolean_claims(self):
        manifest,*_=fixture();reports=[record(*evidence(manifest,s)) for s in SERVICES]
        for key in ('signatureVerified','attestationVerified','exists'):
            bad=copy.deepcopy(reports);bad[0][key]=1
            with self.assertRaises(ValueError):assemble(manifest,bad,REPO,TAG,SHA)
