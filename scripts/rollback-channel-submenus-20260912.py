import json,os,subprocess,time,urllib.request
from pathlib import Path
os.umask(0o077)
r=Path('/www/twork/releases/channel-submenus-20260912');b=r/'backup';compose=Path('/www/twork/new-api/docker-compose.shtlcloud.yml');nginx=Path('/usr/local/nginx/conf/nginx-newapi-twork.conf')
old=json.loads((b/'containers.json').read_text())[0]
def install(p,data):
 t=p.with_name(p.name+'.pi-rollback-tmp');t.write_bytes(data);os.replace(t,p)
# 候选仍接流时先恢复旧正式容器，健康后再把入口切回。
install(compose,(b/'gateway-compose.yml').read_bytes())
subprocess.run(['/usr/local/bin/docker-compose','--env-file',str(compose.parent/'.env.shtlcloud'),'-f',str(compose),'up','-d','--no-deps','--no-build','new-api'],cwd=compose.parent,check=True)
end=time.time()+60
while True:
 item=json.loads(subprocess.check_output(['docker','inspect','new-api-twork']))[0]
 if item['Image']==old['Image'] and item['State'].get('Health',{}).get('Status')=='healthy':break
 if time.time()>end:raise RuntimeError('旧正式容器尚未健康，保留当前入口')
 time.sleep(2)
install(nginx,(b/'nginx-gateway.conf').read_bytes())
subprocess.run(['/usr/local/nginx/sbin/nginx','-t'],check=True);subprocess.run(['/usr/local/nginx/sbin/nginx','-s','reload'],check=True)
print(json.dumps({'rollback':'PASS','image':item['Image']}))
