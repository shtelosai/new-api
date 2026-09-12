"""渠道二级菜单发布：逐阶段校验提交、备份、在途排空与镜像身份。"""
import hashlib,json,os,re,shutil,subprocess,sys,time,urllib.request
from pathlib import Path

os.umask(0o077)
ROOT=Path('/www/twork/releases/channel-submenus-20260912')
BACKUP=ROOT/'backup'
CONFIG=Path('/usr/local/nginx/conf/nginx-newapi-twork.conf')
COMPOSE=Path('/www/twork/new-api/docker-compose.shtlcloud.yml')
NGINX='/usr/local/nginx/sbin/nginx'
MAIN='/usr/local/nginx/conf/nginx.conf'
CANDIDATE='new-api-twork-channel-submenus-20260912'
IMAGE='new-api:twork-channel-submenus-20260912'
MANIFEST=json.loads((ROOT/'manifest.json').read_text())
SHA=MANIFEST['binary_sha256']
REVISION=MANIFEST['revision']
OLD=json.loads((BACKUP/'containers.json').read_text()) if (BACKUP/'containers.json').exists() else []

def run(*args):
    return subprocess.check_output(args,text=True).strip()
def inspect(name):
    return json.loads(run('docker','inspect',name))[0]
def env(item):
    return dict(x.split('=',1) for x in item['Config']['Env'])
def expected_env():
    return env(OLD[0])
def save(name,value):
    path=ROOT/(name+'.json')
    with path.open('x') as f:json.dump(value,f,indent=2)
def digest(data):return hashlib.sha256(data).hexdigest()
def health(port):
    with urllib.request.urlopen(f'http://127.0.0.1:{port}/api/status',timeout=8) as response:
        result=json.load(response)
    assert result.get('success') and result['data']['version']=='twork-channel-submenus-20260912'
def install(path,data):
    tmp=path.with_name(path.name+'.pi-release-tmp')
    assert not tmp.exists()
    shutil.copy2(path,tmp);tmp.write_bytes(data);os.replace(tmp,path)
def reload():
    subprocess.run([NGINX,'-c',MAIN,'-t'],check=True)
    subprocess.run([NGINX,'-c',MAIN,'-s','reload'],check=True)
def workers():
    return [int(l.split()[0]) for l in run('ps','-eo','pid,args').splitlines() if 'nginx: worker process' in l]
def connections(name):
    pid=str(inspect(name)['State']['Pid'])
    value=run('nsenter','-t',pid,'-n','ss','-Htn','state','established','sport = :3000')
    return len(value.splitlines()) if value else 0
def identity(name):
    item=inspect(name)
    assert item['Image']==inspect(IMAGE)['Id'] and item['State']['Running']
    assert item['Config']['Labels']['org.opencontainers.image.revision']==REVISION
    assert item['State']['Health']['Status']=='healthy'
    return item
def inflight(name):
    port=3001 if name=='new-api-twork' else 3002
    token=env(inspect(name))['METRICS_BEARER_TOKEN']
    req=urllib.request.Request(f'http://127.0.0.1:{port}/internal/metrics',headers={'Authorization':'Bearer '+token})
    with urllib.request.urlopen(req,timeout=8) as response:raw=response.read().decode()
    match=re.search(r'^newapi_http_requests_in_flight ([0-9.]+)$',raw,re.M)
    assert match,'缺少在途请求指标'
    return float(match[1])
def drain(name,since):
    logs=run('docker','logs','--since',str(since),name)
    business=[l for l in logs.splitlines() if re.search(r'\|\s+(POST|PUT|DELETE)\s+',l)]
    starts=logs.count('batch update started');ends=logs.count('batch update finished')
    errors=[l for l in logs.splitlines() if 'failed to batch update' in l.lower()]
    value={'inflight':inflight(name),'connections':connections(name),'batch_started':starts,'batch_finished':ends,'batch_errors':len(errors),'last_business':business[-1:]}
    return value

action=sys.argv[1]
if action=='snapshot':
    assert not BACKUP.exists(), '备份已存在，不得覆盖'
    BACKUP.mkdir(mode=0o700)
    names=run('docker','ps','--format','{{.Names}}').splitlines()
    assert 'new-api-twork' in names
    items=[inspect('new-api-twork')]+[inspect(name) for name in names if name!='new-api-twork']
    (BACKUP/'containers.json').write_text(json.dumps(items,indent=2))
    shutil.copy2(CONFIG,BACKUP/'nginx-gateway.conf')
    shutil.copy2(COMPOSE,BACKUP/'gateway-compose.yml')
    shutil.copy2(COMPOSE.parent/'.env.shtlcloud',BACKUP/'gateway.env')
    run('docker','tag',items[0]['Image'],'new-api:channel-submenus-rollback-20260912')
    save('snapshot',{'old_image':items[0]['Image'],'old_container':items[0]['Id']})
elif action=='build':
    assert digest((ROOT/'new-api-linux-amd64').read_bytes())==SHA
    assert inspect('new-api-twork')['Id']==OLD[0]['Id']
    assert inspect('new-api:channel-submenus-rollback-20260912')['Id']==OLD[0]['Image']
    dockerfile='FROM new-api:channel-submenus-rollback-20260912\nCOPY --chmod=755 new-api-linux-amd64 /new-api\nLABEL org.opencontainers.image.revision="'+REVISION+'"\n'
    (ROOT/'Dockerfile').write_text(dockerfile)
    subprocess.run(['docker','build','--network=none','--pull=false','-t',IMAGE,str(ROOT)],check=True)
    built=inspect(IMAGE);base=inspect(OLD[0]['Image'])
    assert built['RootFS']['Layers'][:-1]==base['RootFS']['Layers']
    save('image',{'tag':IMAGE,'id':built['Id'],'binary_sha':SHA,'base_layers_preserved':True})
elif action=='candidate':
    assert inspect('new-api-twork')['Id']==OLD[0]['Id']
    assert not run('docker','ps','-aq','--filter','name=^/'+CANDIDATE+'$')
    e=expected_env();assert e.get('SKIP_AUTO_MIGRATE')=='true'
    e['NODE_TYPE']='slave'
    ep=ROOT/'candidate.env';ep.write_text(''.join(f'{k}={v}\n' for k,v in e.items()))
    networks=list(OLD[0]['NetworkSettings']['Networks']);assert len(networks)==1
    command=['docker','run','-d','--name',CANDIDATE,'--restart','unless-stopped','--network',networks[0],'-p','127.0.0.1:3002:3000','--env-file',str(ep)]
    for dns in OLD[0]['HostConfig']['Dns']:command+=['--dns',dns]
    for mount in OLD[0]['Mounts']:
        source=mount['Source']
        if mount['Destination']=='/app/logs':
            source=str(ROOT/'candidate-logs');Path(source).mkdir()
        command+=['-v',source+':'+mount['Destination']]
    command+=['--log-driver','json-file','--log-opt','max-size=100m','--log-opt','max-file=3','--health-cmd',"wget -q -O - http://127.0.0.1:3000/api/status | grep -q '\"success\":true'",'--health-interval','10s','--health-timeout','5s','--health-retries','3',IMAGE,*OLD[0]['Config']['Cmd']]
    cid=run(*command)
    save('candidate',{'id':cid,'started':time.time(),'node_type':'slave'})
elif action in ['cutover','return']:
    health(3002)
    candidate=identity(CANDIDATE);assert env(candidate).get('NODE_TYPE')=='slave'
    original=(BACKUP/'nginx-gateway.conf').read_bytes()
    assert original.count(b'proxy_pass http://127.0.0.1:3001/;')==1
    changed=original.replace(b'proxy_pass http://127.0.0.1:3001/;',b'proxy_pass http://127.0.0.1:3002/;')
    before,after=(original,changed) if action=='cutover' else (changed,original)
    assert CONFIG.read_bytes()==before
    if action=='cutover':assert inspect('new-api-twork')['Id']==OLD[0]['Id']
    else:
        health(3001);formal=identity('new-api-twork')
        assert env(formal)==expected_env() and formal['Id']!=OLD[0]['Id']
    save(action,{'at':time.time(),'nginx_workers':workers(),'before':digest(before),'after':digest(after)})
    try:install(CONFIG,after);reload()
    except Exception:install(CONFIG,before);reload();raise
elif action=='drain':
    phase=sys.argv[2];name='new-api-twork' if phase=='old' else CANDIDATE
    since=json.loads((ROOT/('cutover.json' if phase=='old' else 'return.json')).read_text())['at']
    print(json.dumps(drain(name,since)))
elif action=='promote':
    health(3002);identity(CANDIDATE)
    assert inspect('new-api-twork')['Id']==OLD[0]['Id']
    saved=json.loads((ROOT/'cutover.json').read_text());assert digest(CONFIG.read_bytes())==saved['after']
    assert time.time()-saved['at']>30
    state=drain('new-api-twork',saved['at'])
    assert state['inflight']==0 and state['batch_errors']==0 and state['batch_started']==state['batch_finished'],state
    recent=drain('new-api-twork',time.time()-20)
    assert not recent['last_business'] and recent['batch_started']==recent['batch_finished']
    assert not set(saved['nginx_workers'])&set(workers()),'旧 Nginx worker 尚未退出'
    assert COMPOSE.read_bytes()==(BACKUP/'gateway-compose.yml').read_bytes()
    # 正式环境配置不变，先停旧 master，再由原 compose 创建新 master。
    run('docker','stop','--time','30','new-api-twork')
    assert not inspect('new-api-twork')['State']['Running']
    before=COMPOSE.read_bytes();needle=('image: '+OLD[0]['Config']['Image']).encode();assert before.count(needle)==1
    changed=before.replace(needle,('image: '+IMAGE).encode())
    install(COMPOSE,changed)
    subprocess.run(['/usr/local/bin/docker-compose','--env-file',str(COMPOSE.parent/'.env.shtlcloud'),'-f',str(COMPOSE),'up','-d','--no-deps','--no-build','new-api'],cwd=COMPOSE.parent,check=True)
    item=inspect('new-api-twork');assert env(item)==expected_env()
    for peer in OLD[1:]:assert inspect(peer['Name'])['Id']==peer['Id']
    save('formal',{'id':item['Id'],'image':item['Image'],'environment_unchanged':True,'peers_unchanged':True,'drain':state})
elif action=='stop-candidate':
    health(3001);identity('new-api-twork')
    assert CONFIG.read_bytes()==(BACKUP/'nginx-gateway.conf').read_bytes()
    saved=json.loads((ROOT/'return.json').read_text());assert time.time()-saved['at']>30
    state=drain(CANDIDATE,saved['at'])
    assert state['inflight']==0 and state['batch_errors']==0 and state['batch_started']==state['batch_finished'],state
    assert not set(saved['nginx_workers'])&set(workers())
    recent=drain(CANDIDATE,time.time()-20);assert not recent['last_business']
    run('docker','update','--restart=no',CANDIDATE)
    run('docker','stop','--time','30',CANDIDATE)
    save('candidate-stopped',{'at':time.time(),'drain':state})
else:raise RuntimeError('未知阶段')
print(json.dumps({'action':action,'result':'PASS'}))
