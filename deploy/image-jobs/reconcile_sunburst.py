#!/usr/bin/env python3
"""同步生图切至 Sunburst 优先；默认只读，供应商与内部凭据不回显。"""
import argparse,json,os,time
from pathlib import Path
from urllib.parse import urlsplit,unquote

ASYNC = [('kie','flare',201,200),('apimart','flare',202,100),('kie','sunburst',203,400),('apimart','sunburst',204,300)]
LEGACY = [('apimart','sunburst',204,400),('kie','sunburst',203,300),('kie','flare',201,200)]

def reconcile(connection, base, *, apply=False, backup_dir=None):
    if urlsplit(base).scheme not in ('http','https') or urlsplit(base).path not in ('','/'):
        raise ValueError('适配器地址须为服务根地址')
    cursor=connection.cursor()
    def rows(sql,args=()):
        cursor.execute(sql,args)
        return [dict(zip([c[0] for c in cursor.description],r)) for r in cursor.fetchall()]
    try:
        old={r['id']:r for r in rows('SELECT * FROM channels WHERE id IN (198,201,202,203,204)')}
        if len(old)!=5:raise ValueError('既有图片渠道不完整')
        before={'channels':list(old.values()),'abilities':rows('SELECT * FROM abilities WHERE channel_id IN (198,201,202,203,204)'), 'pricing':rows("SELECT * FROM options WHERE `key`='ModelPrice'")}
        summary={'mode':'apply' if apply else 'dry-run','async':[],'legacy':[],'ddl_required':False}
        new_specs=[]
        for provider,family,cid,priority in ASYNC:
            settings=json.loads(old[cid].get('setting') or '{}')
            if settings.get('twork_runtime')!='image_async' or settings.get('twork_image_provider')!=provider or settings.get('twork_image_family')!=family:
                raise ValueError('异步渠道身份不匹配')
            target=f'{base.rstrip("/")}/{provider}-async-{family}'
            summary['async'].append({'id':cid,'priority':priority,'base_url':target})
        for provider,family,source,priority in LEGACY:
            name=f'{provider}-gpt-image-2.5-{family}-sync'
            existing=rows('SELECT * FROM channels WHERE name=%s',(name,))
            if len(existing)>1:raise ValueError('同步渠道重名')
            spec=dict(old[198]);spec.pop('id')
            spec.update(name=name,key=old[source]['key'],models=f'gpt-image-2.5-{family}',base_url=f'{base.rstrip("/")}/{provider}-sync-{family}',priority=priority,weight=1,status=1,tag='image',model_mapping=None,param_override=None,header_override=None,
                setting=json.dumps({'twork_image_provider':provider,'twork_image_family':family,'twork_image_resolutions':{'generation':['1k','2k','4k'],'edit':['1k','2k','4k']}}))
            for field in ('used_quota','response_time','test_time','balance','balance_updated_time'):
                if field in spec:spec[field]=0
            if 'created_time' in spec:spec['created_time']=int(time.time())
            if existing:
                for k in ('models','base_url','priority','setting','key'):
                    if existing[0].get(k)!=spec[k]:raise ValueError('既有同步渠道配置冲突')
            new_specs.append((spec,existing[0]['id'] if existing else None))
            summary['legacy'].append({'id':existing[0]['id'] if existing else None,'name':name,'model':spec['models'],'priority':priority})
        price=json.loads(before['pricing'][0]['value'])
        if price.get('gpt-image-2.5-flare')!=0.3 or price.get('gpt-image-2.5-sunburst',0.3)!=0.3:raise ValueError('旧同步价格不匹配')
        summary['legacy_price']=0.3
        if not apply:
            connection.rollback();return summary
        if not backup_dir:raise ValueError('必须指定私有备份目录')
        os.umask(0o077);backup=Path(backup_dir);backup.mkdir(parents=True,exist_ok=True)
        with (backup/'channels-before.json').open('x') as f:json.dump(before,f,default=str)
        for item in summary['async']:
            cursor.execute('UPDATE channels SET priority=%s,base_url=%s WHERE id=%s',(item['priority'],item['base_url'],item['id']))
            cursor.execute('UPDATE abilities SET priority=%s WHERE channel_id=%s',(item['priority'],item['id']))
        for index,(spec,cid) in enumerate(new_specs):
            if cid is None:
                cursor.execute('INSERT INTO channels ('+','.join('`'+c+'`' for c in spec)+') VALUES ('+','.join(['%s']*len(spec))+')',tuple(spec.values()));cid=cursor.lastrowid
            for group in spec['group'].split(','):
                cursor.execute('INSERT INTO abilities (`group`,model,channel_id,enabled,priority,weight,tag) VALUES (%s,%s,%s,1,%s,1,%s) ON DUPLICATE KEY UPDATE enabled=1,priority=VALUES(priority),weight=1,tag=VALUES(tag)',(group,spec['models'],cid,spec['priority'],spec['tag']))
            summary['legacy'][index]['id']=cid
        price['gpt-image-2.5-sunburst']=0.3
        cursor.execute("UPDATE options SET value=CONVERT(UNHEX(%s) USING utf8mb4) WHERE `key`='ModelPrice'",(json.dumps(price,separators=(',',':')).encode().hex(),))
        # 与既有 Flare 同类登记，避免模型目录将图片误作聊天能力。
        if not rows('SELECT id FROM models WHERE model_name=%s',('gpt-image-2.5-sunburst',)):
            source=rows('SELECT * FROM models WHERE model_name=%s',('gpt-image-2.5-flare',))
            if len(source)!=1:raise ValueError('缺少 Flare 模型登记')
            model=source[0];model.pop('id');model['model_name']='gpt-image-2.5-sunburst'
            for k,v in list(model.items()):
                if isinstance(v,str):model[k]=v.replace('Flare','Sunburst').replace('flare','sunburst')
            cursor.execute('INSERT INTO models ('+','.join('`'+k+'`' for k in model)+') VALUES ('+','.join(['%s']*len(model))+')',tuple(model.values()))
        connection.commit()
        summary['legacy_pool']=','.join([str(x['id']) for x in summary['legacy']]+['198'])
        (backup/'channels-after.json').write_text(json.dumps(summary,ensure_ascii=False,indent=2))
        return summary
    except Exception:
        connection.rollback();raise
    finally:cursor.close()

if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--adapter-base-url',required=True);parser.add_argument('--backup-dir');parser.add_argument('--apply',action='store_true');args=parser.parse_args()
    import pymysql
    url=urlsplit(os.environ['IMAGE_CHANNEL_DATABASE_URL'])
    db=pymysql.connect(host=url.hostname,port=url.port or 3306,user=unquote(url.username or ''),password=unquote(url.password or ''),database=url.path.strip('/'),autocommit=False)
    try:print(json.dumps(reconcile(db,args.adapter_base_url,apply=args.apply,backup_dir=args.backup_dir),ensure_ascii=False))
    finally:db.close()
