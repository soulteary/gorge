#!/usr/bin/env python3
"""Record successful external cryptographic verification and gate complete publication.

The workflow must run cosign/gh verification successfully before `record`.
Parsing their output here binds those verified subjects to the accepted digest;
it is not an independent replacement for cryptographic verification.
"""
import argparse
import copy
import hashlib
import json
from pathlib import Path
import re

from component_images import accepted_image
from manifest import SERVICES
from source_pair import resolve_pair


def record(published, cosign_raw, attestation_raw):
    if published.get('exists') is not True or published.get('overwritePermitted') is not False:
        raise ValueError('Published immutable aliases must be verified first.')
    expected=published.get('digest','')
    if not re.fullmatch('sha256:[0-9a-f]{64}',expected):
        raise ValueError('Exact verified digest required.')
    reference='ghcr.io/'+published['repository']
    signatures=json.loads(cosign_raw);attestations=json.loads(attestation_raw)
    if not isinstance(signatures,list) or not signatures or not isinstance(attestations,list) or not attestations:
        raise ValueError('Both external verifiers must return nonempty results.')
    for signature in signatures:
        critical=signature.get('critical',{})
        if critical.get('image',{}).get('docker-manifest-digest')!=expected or critical.get('identity',{}).get('docker-reference')!=reference:
            raise ValueError('Verified signature subject disagrees with accepted image.')
    if not any(any(s.get('name')==reference and s.get('digest',{}).get('sha256')==expected[7:]
                   for s in a.get('verificationResult',{}).get('statement',{}).get('subject',[])) for a in attestations):
        raise ValueError('Verified attestation subject disagrees with accepted image.')
    result=copy.deepcopy(published)
    result.update(signatureVerified=True,attestationVerified=True,
        verificationWorkflow='https://github.com/'+published['repository']+'/.github/workflows/release.yml@refs/tags/'+published['release'],
        cosignVerificationSHA256=hashlib.sha256(cosign_raw).hexdigest(),
        attestationVerificationSHA256=hashlib.sha256(attestation_raw).hexdigest())
    return result


def assemble(manifest, reports, repository, tag, sha):
    components={}
    identity='https://github.com/'+repository+'/.github/workflows/release.yml@refs/tags/'+tag
    for item in reports:
        service=item.get('service')
        if service not in SERVICES or service in components:
            raise ValueError('Unknown or duplicate signed component.')
        expected=accepted_image(manifest,repository,tag,sha,service)
        wanted={'repository':repository,'release':tag,'sourceRevision':sha,'phorgeRevision':manifest['phorgeCommit'],
                'digest':expected,'aliases':[service+'-'+tag,service+'-sha-'+sha],
                'platforms':['amd64','arm64'],'exists':True,'overwritePermitted':False,
                'signatureVerified':True,'attestationVerified':True,'verificationWorkflow':identity}
        if any(type(item.get(k)) is not type(v) or item[k]!=v for k,v in wanted.items()):
            raise ValueError('Signed evidence does not bind the exact accepted release.')
        for key in ('cosignVerificationSHA256','attestationVerificationSHA256'):
            if not re.fullmatch('[0-9a-f]{64}',item.get(key,'')):
                raise ValueError('External verification receipt digest required.')
        components[service]=item
    if set(components)!=set(SERVICES):
        raise ValueError('All fourteen signed components are required.')
    result=copy.deepcopy(manifest)
    result['imageVerification']={'schemaVersion':1,'workflowIdentity':identity,'components':components}
    return result


def main():
    p=argparse.ArgumentParser(description=__doc__);sub=p.add_subparsers(dest='action',required=True)
    r=sub.add_parser('record')
    for name in ('published','cosign','attestation','output'):r.add_argument('--'+name,required=True,type=Path)
    a=sub.add_parser('assemble')
    for name in ('manifest','directory','output'):a.add_argument('--'+name,required=True,type=Path)
    for name in ('repository','tag','sha'):a.add_argument('--'+name,required=True)
    args=p.parse_args()
    if args.action=='record':
        value=record(json.loads(args.published.read_text()),args.cosign.read_bytes(),args.attestation.read_bytes())
    else:
        manifest=json.loads(args.manifest.read_text())
        resolve_pair(Path(__file__).with_name('source-pair.json'),args.tag,manifest.get('phorgeCommit',''))
        value=assemble(manifest,[json.loads(f.read_text()) for f in sorted(args.directory.glob('verified-*.json'))],args.repository,args.tag,args.sha)
    args.output.write_text(json.dumps(value,indent=2,sort_keys=True)+'\n')


if __name__=='__main__':main()
