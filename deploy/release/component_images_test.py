import copy
import json
import unittest
from unittest.mock import Mock

from component_images import accepted_image, inspect_component
from manifest import SERVICES
from registry_metadata import digest

REPO='soulteary/gorge';TAG='2026.11.02-r1';SHA='a'*40;PAIR='b'*40


def fixture():
    records={};children=[]
    for arch in ('amd64','arm64'):
        config={'architecture':arch,'os':'linux','config':{'Labels':{
            'org.opencontainers.image.revision':SHA,'org.opencontainers.image.source':'https://github.com/'+REPO,
            'org.opencontainers.image.version':TAG,'org.opencontainers.image.title':'gorge-image'}}}
        config_digest=digest(json.dumps(config).encode());records[config_digest]=config
        child=json.dumps({'schemaVersion':2,'config':{'digest':config_digest}}).encode();child_digest=digest(child);records[child_digest]=child
        children.append({'digest':child_digest,'platform':{'os':'linux','architecture':arch}})
    raw=json.dumps({'schemaVersion':2,'manifests':children}).encode();expected=digest(raw);records[expected]=raw
    manifest={'schemaVersion':1,'release':TAG,'gorgeCommit':SHA,'phorgeCommit':PAIR,'images':{
        name:{'name':name,'digest':expected,'image':'ghcr.io/'+REPO+'@'+expected,'gorgeCommit':SHA,'port':port} for name,port in SERVICES.items()}}
    registry=Mock();registry.manifest.side_effect=lambda ref,missing=False:records.get(ref);registry.config.side_effect=lambda ref:records[ref]
    return manifest,registry,records,raw


class ComponentAliases(unittest.TestCase):
    def test_absent_aliases_bind_the_accepted_digest(self):
        manifest,registry,_,raw=fixture();r=inspect_component(registry,manifest,REPO,TAG,SHA,'image')
        self.assertFalse(r['exists']);self.assertEqual(r['digest'],digest(raw));self.assertEqual(r['platforms'],['amd64','arm64'])
        self.assertEqual(r['aliases'],['image-'+TAG,'image-sha-'+SHA]);self.assertFalse(r['overwritePermitted'])

    def test_matching_complete_aliases_are_reusable(self):
        manifest,registry,records,raw=fixture();records['image-'+TAG]=raw;records['image-sha-'+SHA]=raw
        self.assertTrue(inspect_component(registry,manifest,REPO,TAG,SHA,'image',True)['exists'])

    def test_partial_conflicting_or_absent_required_aliases_fail(self):
        for mode in ('partial','conflict','missing'):
            manifest,registry,records,raw=fixture()
            if mode!='missing':records['image-'+TAG]=raw
            if mode=='conflict':records['image-sha-'+SHA]=b'other'
            with self.assertRaises(ValueError):inspect_component(registry,manifest,REPO,TAG,SHA,'image',True)

    def test_foreign_component_cannot_be_published(self):
        manifest,registry,_,_=fixture()
        with self.assertRaises(ValueError):inspect_component(registry,manifest,REPO,TAG,SHA,'render')

    def test_tampered_accepted_digest_fails_before_alias_access(self):
        manifest,registry,records,raw=fixture();records[digest(raw)]=b'tampered'
        with self.assertRaises(ValueError):inspect_component(registry,manifest,REPO,TAG,SHA,'image')
        self.assertEqual(registry.manifest.call_count,1)

    def test_all_manifest_identities_and_component_packaging_are_bound(self):
        base,_,_,_=fixture()
        for key,value in [('schemaVersion',2),('release','latest'),('gorgeCommit',PAIR),('phorgeCommit','short')]:
            m=copy.deepcopy(base);m[key]=value
            with self.assertRaises(ValueError):accepted_image(m,REPO,TAG,SHA,'image')
        for key,value in [('name','other'),('image','ghcr.io/other/gorge@sha256:'+'c'*64),('port',1),('gorgeCommit',PAIR),('digest','short')]:
            m=copy.deepcopy(base);m['images']['worker'][key]=value
            with self.assertRaises(ValueError):accepted_image(m,REPO,TAG,SHA,'image')

    def test_incomplete_and_unknown_sets_fail(self):
        m,_,_,_=fixture()
        with self.assertRaises(ValueError):accepted_image(m,REPO,TAG,SHA,'unknown')
        del m['images']['worker']
        with self.assertRaises(ValueError):accepted_image(m,REPO,TAG,SHA,'image')
