#!/usr/bin/env python3
"""Fresh, Linux-only Docker regression. Never reads deployment config or credentials."""
import argparse,base64,email,email.policy,hashlib,hmac,json,os,pathlib,re,secrets,subprocess,sys,tempfile,time,urllib.error,urllib.parse,urllib.request,uuid
HERE=pathlib.Path(__file__).resolve().parent
SERVICES={'redis','stargate','warden','herald','owlmail','webhook'}
class Failure(Exception):pass
class NoRedirect(urllib.request.HTTPRedirectHandler):
 def redirect_request(self,*args):return None

def validate_lock(lock):
 if set(lock['services'])!=SERVICES:raise ValueError('Six exact fixture services required')
 for name,item in lock['services'].items():
  prefix='docker.io/library/redis' if name=='redis' else 'ghcr.io/soulteary/'+name
  if not re.fullmatch(re.escape(prefix)+r'@sha256:[0-9a-f]{64}',item['image']):raise ValueError('Immutable official image required')
  if name!='redis' and not re.fullmatch('[0-9a-f]{40}',item.get('revision','')):raise ValueError('Exact source revision required')

def validate_docker_target(env,endpoint,context="default"):
 if context!='default' or any(env.get(k) for k in ('DOCKER_HOST','DOCKER_CONTEXT','DOCKER_TLS_VERIFY','DOCKER_CERT_PATH')) or not endpoint.startswith('unix://'):
  raise ValueError('Use the local default Linux Docker socket without context/host overrides')

def fixture(root,lock):
 validate_lock(lock);secret={k:secrets.token_hex(32) for k in ['redis','warden','herald','exchange','mail','smtp','hook']}
 def file(name,value,mode=0o644):
  p=root/name;p.write_text(json.dumps(value) if isinstance(value,(list,dict)) else value);p.chmod(mode);return str(p)
 user='fixture@example.invalid';file('users.json',[{'mail':user,'user_id':'synthetic-regression-user','status':'active','scope':['read'],'role':'user','name':'Synthetic Fixture'}])
 hook=[{'id':'owlmail-validation','execute-command':'/usr/bin/webhook','command-working-directory':'/','pass-arguments-to-command':[{'source':'string','name':'-version'}],'http-methods':['POST'],'incoming-payload-content-type':'application/json','include-command-output-in-response':True,'trigger-rule':{'match':{'type':'payload-hmac-sha256','secret':secret['hook'],'parameter':{'source':'header','name':'X-OwlMail-Signature'}}}}];file('hooks.json',hook)
 file('webhook.json',{'version':1,'targets':[{'name':'fixture-webhook','url':'http://webhook:9000/hooks/owlmail-validation','secret':secret['hook'],'timeout':'5s','retries':2}]})
 def mount(source,target):return {'type':'bind','source':str(root/source),'target':target,'read_only':True}
 common={'restart':'no','read_only':True,'cap_drop':['ALL'],'security_opt':['no-new-privileges:true'],'labels':{'traefik.enable':'false'}}
 out={'services':{},'networks':{'default':{'internal':True}},'volumes':{'mail':{},'audit':{}}}
 env={
 'redis':{},
 'warden':{'PORT':'8081','ENVIRONMENT':'production','MERGE_MODE':'ONLY_LOCAL','DATA_FILE':'/fixture/users.json','REDIS':'redis:6379','REDIS_PASSWORD':secret['redis'],'API_KEY':secret['warden']},
 'herald':{'PORT':':8082','ENVIRONMENT':'production','REDIS_ADDR':'redis:6379','REDIS_PASSWORD':secret['redis'],'REDIS_DB':'1','API_KEY':secret['herald'],'REQUEST_AUTH_MODE':'api_key','PROVIDER_FAILURE_POLICY':'strict','SMTP_HOST':'owlmail','SMTP_PORT':'1025','SMTP_USER':'fixture','SMTP_PASSWORD':secret['smtp'],'SMTP_FROM':'sender@example.invalid','SSL_CERT_FILE':'/fixture/cert.pem','AUDIT_STORAGE_TYPE':'file','AUDIT_FILE_PATH':'/var/lib/herald/audit/events.jsonl'},
 'stargate':{'AUTH_HOST':'auth.example.invalid','CALLBACK_ALLOWED_HOSTS':'app.example.invalid','SESSION_EXCHANGE_SECRET':secret['exchange'],'PORT':'8080','WARDEN_ENABLED':'true','WARDEN_URL':'http://warden:8081','WARDEN_API_KEY':secret['warden'],'HERALD_ENABLED':'true','HERALD_URL':'http://herald:8082','HERALD_API_KEY':secret['herald'],'LOGIN_EMAIL_ENABLED':'true','SESSION_STORAGE_ENABLED':'true','SESSION_STORAGE_REDIS_ADDR':'redis:6379','SESSION_STORAGE_REDIS_PASSWORD':secret['redis'],'SESSION_STORAGE_REDIS_DB':'2','COOKIE_SECURE':'true'},
 'owlmail':{'OWLMAIL_WEB_USER':'fixture','OWLMAIL_WEB_PASSWORD':secret['mail'],'OWLMAIL_TLS_ENABLED':'true','OWLMAIL_TLS_CERT':'/fixture/cert.pem','OWLMAIL_TLS_KEY':'/fixture/key.pem','OWLMAIL_SMTPS_PORT':'0','OWLMAIL_SMTP_USER':'fixture','OWLMAIL_SMTP_PASSWORD':secret['smtp'],'OWLMAIL_SMTP_AUTH_REQUIRE_TLS':'true','OWLMAIL_WEBHOOK_CONFIG':'/fixture/webhook.json'},
 'webhook':{}}
 for name in SERVICES:
  out['services'][name]={**common,'image':lock['services'][name]['image'],'environment':env[name],'tmpfs':['/tmp:rw,nosuid,size=32m']}
 out['services']['redis'].update(command=['redis-server','--requirepass',secret['redis'],'--save',''],read_only=False,healthcheck={'test':['CMD','redis-cli','-a',secret['redis'],'ping'],'interval':'2s','timeout':'2s','retries':30})
 out['services']['warden']['volumes']=[mount('users.json','/fixture/users.json')]
 out['services']['herald']['volumes']=[mount('cert.pem','/fixture/cert.pem'),{'type':'volume','source':'audit','target':'/var/lib/herald/audit'}]
 # The named audit volume initializes from the image-owned directory if available.
 out['services']['owlmail']['volumes']=[mount(x,'/fixture/'+x) for x in ['cert.pem','key.pem','webhook.json']]+[{'type':'volume','source':'mail','target':'/app/mail'}]
 out['services']['webhook'].update(command=['-hooks','/fixture/hooks.json','-port','9000'],volumes=[mount('hooks.json','/fixture/hooks.json')])
 return out,secret,user

def certificate(root,log):
 subprocess.run(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-keyout',str(root/'key.pem'),'-out',str(root/'cert.pem'),'-days','1','-subj','/CN=owlmail','-addext','subjectAltName=DNS:owlmail','-addext','basicConstraints=critical,CA:TRUE'],check=True,stdout=log,stderr=log)
 # Bind mounts preserve host modes; consumers have different container UIDs.
 for name in ('cert.pem','key.pem'):(root/name).chmod(0o644)

class Suite:
 def __init__(self,root,lock,report):
  self.root=root;self.lock=lock;self.report=report;self.project='gorge-interop-'+uuid.uuid4().hex[:12];self.cmd=['docker','compose','-p',self.project,'-f',str(root/'compose.json')];self.log=(root/'private.log').open('a');self.checks=[];self.phase='initial';self.op=urllib.request.build_opener(urllib.request.ProxyHandler({}),NoRedirect())
 def call(self,*args):return subprocess.check_output(list(args),stderr=self.log)
 def dc(self,*args):return self.call(*self.cmd,*args)
 def check(self,id,ok):
  self.checks.append({'id':id,'passed':bool(ok)})
  if not ok:raise Failure(id)
 def inspect(self):
  ids=self.dc('ps','-aq').decode().split();xs=json.loads(self.call('docker','inspect',*ids));return {x['Config']['Labels']['com.docker.compose.service']:x for x in xs}
 def ready(self):
  deadline=time.monotonic()+180
  while time.monotonic()<deadline:
   xs=self.inspect()
   if len(xs)==6 and all(x['State']['Running'] and x['State'].get('Health',{}).get('Status','healthy')=='healthy' for x in xs.values()):return xs
   time.sleep(2)
  raise Failure('six-service-readiness-timeout')
 def req(self,name,path,data=None,form=False,auth=False,headers=None):
  x=self.inspect()[name];self.check_owned(x)
  ip=next(iter(x['NetworkSettings']['Networks'].values()))['IPAddress'];port={'owlmail':1080,'herald':8082,'stargate':8080,'webhook':9000}[name]
  h={'Accept':'application/json'};h.update(headers or {})
  if auth:h['Authorization']='Basic '+base64.b64encode(('fixture:'+self.secret['mail']).encode()).decode()
  if isinstance(data,bytes):body=data
  elif data is None:body=None
  else:body=(urllib.parse.urlencode(data) if form else json.dumps(data)).encode();h['Content-Type']='application/x-www-form-urlencoded' if form else 'application/json'
  try:r=self.op.open(urllib.request.Request('http://'+ip+':'+str(port)+path,data=body,headers=h),timeout=35)
  except urllib.error.HTTPError as e:r=e
  with r:return r.status,r.read(4*1024*1024)
 def check_owned(self,x):
  if x['Config']['Labels']['com.docker.compose.project']!=self.project:raise Failure('unexpected-project')
 def exercise(self):
  cfg,self.secret,user=fixture(self.root,self.lock)
  # Fresh per-run certificate only. The key is never returned or uploaded.
  certificate(self.root,self.log)
  (self.root/'compose.json').write_text(json.dumps(cfg));(self.root/'compose.json').chmod(0o600)
  self.phase='pull';self.dc('pull')
  # Initialize only this fresh project's audit volume for Herald's runtime UID.
  self.phase='create';self.dc('create')
  audit_volume=self.project+'_audit'
  self.phase='audit-volume-init'
  self.call('docker','run','--rm','--network','none','--read-only','--cap-drop','ALL','--cap-add','FOWNER','--cap-add','CHOWN','--security-opt','no-new-privileges:true','--entrypoint','sh','-v',audit_volume+':/audit',self.lock['services']['redis']['image'],'-c','chmod 700 /audit && chown 10001:10001 /audit')
  self.phase='start';self.dc('up','-d','--pull','never');self.phase='readiness';xs=self.ready()
  self.check('six-services-running',len(xs)==6)
  self.check('no-published-ports',all(not x['HostConfig'].get('PortBindings') for x in xs.values()))
  self.check('gateway-disabled',all(x['Config']['Labels'].get('traefik.enable')=='false' for x in xs.values()))
  self.check('all-image-healthchecks-healthy',all(x['State'].get('Health',{}).get('Status','healthy')=='healthy' for x in xs.values()))
  for name,x in xs.items():
   self.check_owned(x);expected=self.lock['services'][name]
   if x['Config']['Image']!=expected['image']:raise Failure('image-digest-mismatch')
   if name!='redis':
    labels=x['Config']['Labels'];version=labels.get('org.opencontainers.image.version','')
    if labels.get('org.opencontainers.image.revision')!=expected['revision'] or version.removeprefix('v')!=expected['version'].removeprefix('v'):raise Failure('image-source-version-mismatch')
  # Herald uses an image-owned persistent audit volume; no copied deployment path.
  audit_stat=self.call('docker','run','--rm','--network','none','--read-only','--cap-drop','ALL','--entrypoint','stat','-v',audit_volume+':/audit:ro',self.lock['services']['redis']['image'],'-c','%a:%u:%g','/audit').decode().strip()
  self.check('herald-audit-spool-private',audit_stat=='700:10001:10001')
  status,_=self.req('owlmail','/api/v1/emails');self.check('mail-api-requires-authentication',status==401)
  status,b=self.req('owlmail','/api/v1/emails/cursor',auth=True);self.check('authenticated-mail-cursor',status==200);cursor=json.loads(b)['cursor']
  headers={'Host':'auth.example.invalid'}
  status,_=self.req('stargate','/_send_verify_code',{'mail':user},True,headers={**headers,'Origin':'https://evil.invalid'});self.check('cross-site-otp-request-rejected',status==403)
  status,_=self.req('stargate','/_send_verify_code',{'mail':'unlisted@example.invalid','deliver_via':'email'},True,headers=headers);self.check('warden-denies-unlisted',status==401)
  status,b=self.req('stargate','/_send_verify_code',{'mail':user,'deliver_via':'email'},True,headers=headers);obj=json.loads(b);self.check('warden-herald-creates-challenge',status==200 and obj.get('success') and 'debug_code' not in obj);challenge=obj['challenge_id']
  status,b=self.req('owlmail','/api/v1/emails/wait',{'after':cursor,'recipient':user,'count':1,'timeout_seconds':20},auth=True);obj=json.loads(b);self.check('herald-tls-smtp-owlmail-delivery',status==200 and not obj.get('timed_out') and len(obj.get('emails',[]))==1);eid=obj['emails'][0]['id']
  status,b=self.req('owlmail','/api/v1/emails/'+urllib.parse.quote(eid,safe='')+'/source',auth=True);self.check('captured-mime-source',status==200)
  msg=email.message_from_bytes(b,policy=email.policy.default);body='\n'.join(str(p.get_content()) for p in msg.walk() if p.get_content_type() in ('text/plain','text/html'));codes=set(re.findall(r'(?<!\d)(\d{6})(?!\d)',body));self.check('otp-only-from-captured-mail',len(codes)==1);code=codes.pop()
  params={'auth_method':'warden','mail':user,'challenge_id':challenge,'verify_code':'000000' if code!='000000' else '111111','callback':'app.example.invalid'}
  status,_=self.req('stargate','/_login',params,True,headers=headers);self.check('incorrect-otp-rejected',status in (400,401,403))
  params['verify_code']=code;status,b=self.req('stargate','/_login',params,True,headers=headers);self.check('correct-otp-authenticates',status==200 and json.loads(b).get('success'))
  status,b=self.req('herald','/v1/otp/verifications',{'challenge_id':challenge,'code':code},headers={'X-API-Key':self.secret['herald']});self.check('consumed-otp-replay-rejected',status in (200,400,401,403) and not json.loads(b).get('ok',True))
  data=b'{"event":"isolated.validation"}';path='/hooks/owlmail-validation'
  bad={'Content-Type':'application/json','X-OwlMail-Signature':'sha256='+'0'*64}
  status,b=self.req('webhook',path,data,headers=bad);self.check('webhook-invalid-hmac-native-500-no-command-output',status==500 and b==b'Error occurred while evaluating hook rules.')
  good={**bad,'X-OwlMail-Signature':'sha256='+hmac.new(self.secret['hook'].encode(),data,hashlib.sha256).hexdigest()}
  status,b=self.req('webhook',path,data,headers=good);self.check('webhook-valid-hmac-version-command',status==200 and self.lock['services']['webhook']['version'].removeprefix('v').encode() in b)
  status,b=self.req('webhook',path,data+b' ',headers=good);self.check('webhook-altered-body-native-500-no-command-output',status==500 and b==b'Error occurred while evaluating hook rules.')
  for _ in range(20):
   status,b=self.req('owlmail','/api/v1/webhook-deliveries?email_id='+urllib.parse.quote(eid,safe=''),auth=True);d=json.loads(b)
   if any(row.get('status')=='succeeded' for row in d.get('deliveries',[])):break
   time.sleep(1)
  self.check('owlmail-signed-webhook-delivered',status==200 and d.get('persistence_healthy') and any(row['status']=='succeeded' and row.get('outcomes') and all(o['status_code']==200 for o in row['outcomes']) for row in d.get('deliveries',[])))
  before=self.req('owlmail','/api/v1/emails/'+urllib.parse.quote(eid,safe='')+'/source',auth=True)[1];receipts=d['deliveries'];started={n:(x['Id'],x['State']['StartedAt']) for n,x in self.inspect().items()};self.dc('restart','owlmail');after=self.ready()
  self.check('test-container-only-restarted',all((after[n]['Id'],after[n]['State']['StartedAt'])==state for n,state in started.items() if n!='owlmail') and after['owlmail']['State']['StartedAt']!=started['owlmail'][1])
  self.check('healthy-after-restart',self.inspect()['owlmail']['State']['Health']['Status']=='healthy')
  status,b=self.req('owlmail','/api/v1/emails',auth=True);self.check('message-set-preserved',status==200 and json.loads(b)['total']==1 and {x['id'] for x in json.loads(b)['emails']}=={eid})
  status,b=self.req('owlmail','/api/v1/emails/'+urllib.parse.quote(eid,safe='')+'/source',auth=True);self.check('raw-message-hash-preserved',status==200 and hashlib.sha256(before).digest()==hashlib.sha256(b).digest())
  status,b=self.req('owlmail','/api/v1/webhook-deliveries?email_id='+urllib.parse.quote(eid,safe=''),auth=True);self.check('webhook-receipts-preserved',status==200 and json.loads(b)['deliveries']==receipts)
 def run(self):
  ok=False;error=None;clean=False;diagnostics=[];categories=[]
  try:
   self.exercise()
   if len(self.checks)!=25 or len({x['id'] for x in self.checks})!=25 or not all(x['passed'] for x in self.checks):raise Failure('incomplete-check-set')
   ok=True
  except Exception as e:
   error=str(e) if isinstance(e,Failure) else type(e).__name__+':'+self.phase
   self.log.flush();raw=(self.root/'private.log').read_text(errors='replace').lower()
   categories=[label for label in ['permission denied','operation not permitted','manifest unknown','unauthorized','invalid mount config','no matching manifest','no such file or directory'] if label in raw]
   try:diagnostics=[{'service':n,'state':x['State']['Status'],'exitCode':x['State']['ExitCode'],'health':x['State'].get('Health',{}).get('Status')} for n,x in self.inspect().items()]
   except Exception:pass
  finally:
   try:
    self.dc('down','--volumes','--remove-orphans','--timeout','10');clean=not self.call('docker','ps','-aq','--filter','label=com.docker.compose.project='+self.project).strip() and not self.call('docker','volume','ls','-q','--filter','label=com.docker.compose.project='+self.project).strip()
   except Exception:clean=False
   report={'schemaVersion':1,'passed':ok and clean,'checks':self.checks,'failure':error,'diagnosticCategories':categories,'serviceStates':diagnostics,'cleanupPassed':clean,'sourceRevision':subprocess.check_output(['git','-C',str(HERE),'rev-parse','HEAD'],text=True).strip(),'sourceTree':subprocess.check_output(['git','-C',str(HERE),'rev-parse','HEAD^{tree}'],text=True).strip(),'runnerSHA256':hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest(),'images':self.lock['services'],'scope':'Fresh six-service internal Docker project; synthetic allowlist, per-run credentials/TLS, no published ports or copied deployment data. No live/browser/permission-model or external relay activation.','rawMessagesExported':False}
   self.report.parent.mkdir(parents=True,exist_ok=True);self.report.write_text(json.dumps(report,indent=2)+'\n');self.log.close()
  return ok and clean

def main():
 parser=argparse.ArgumentParser();parser.add_argument('--output',type=pathlib.Path,required=True);args=parser.parse_args()
 if sys.platform!='linux':raise SystemExit('Requires a Linux Docker runner; no host-port fallback')
 validate_docker_target(os.environ,subprocess.check_output(['docker','context','inspect','default','--format','{{.Endpoints.docker.Host}}'],text=True).strip(),subprocess.check_output(['docker','context','show'],text=True).strip())
 os.umask(0o077);lock=json.loads((HERE/'images.json').read_text())
 with tempfile.TemporaryDirectory(prefix='gorge-interop-') as tmp:ok=Suite(pathlib.Path(tmp),lock,args.output.resolve()).run()
 print(json.dumps({'passed':ok,'receipt':str(args.output)}));return 0 if ok else 1
if __name__=='__main__':raise SystemExit(main())
