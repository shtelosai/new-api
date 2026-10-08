#!/usr/bin/env python3
"""配置四条专用异步图片渠道；默认只读预览，不修改旧渠道或用户授权。

运维环境复用 tbackend 的 PyMySQL；供应商密钥不进入此脚本。
"""

import argparse
import json
import os
from urllib.parse import unquote, urlsplit


def channel_specs(kie_base, apimart_base, internal_key, group):
    if len(internal_key) < 32 or not group or ',' in group:
        raise ValueError('内部认证须至少 32 字符，且必须指定单一分组')
    for base in (kie_base, apimart_base):
        url = urlsplit(base)
        if (url.scheme not in ('http', 'https') or not url.hostname
                or url.username or url.password or url.query or url.fragment):
            raise ValueError('内部适配器地址无效')
    result = []
    for family in ('flare', 'sunburst'):
        for provider, base, priority in (('kie', kie_base, 200), ('apimart', apimart_base, 100)):
            result.append({
                'name': f'twork-{provider}-{family}-async', 'type': 1,
                'key': internal_key, 'status': 1, 'base_url': base.rstrip('/') + '-' + family,
                'models': f'twork-image-{family}-async', 'group': group,
                'priority': priority + (200 if family == 'sunburst' else 0), 'weight': 1, 'tag': 'image-async',
                'setting': json.dumps({'twork_runtime': 'image_async',
                    'twork_image_provider': provider, 'twork_image_family': family}),
            })
    return result


def configure(connection, specs, *, apply=False, placeholder='%s'):
    """先完整检查再写入；同名异配必须人工处理，重复执行不增加渠道。"""
    cursor = connection.cursor()
    prepared = []
    try:
        for spec in specs:
            cursor.execute('SELECT * FROM channels WHERE name=' + placeholder, (spec['name'],))
            rows = [dict(zip([col[0] for col in cursor.description], row)) for row in cursor.fetchall()]
            if len(rows) > 1:
                raise ValueError('存在重复的异步渠道名称')
            existing = rows[0] if rows else None
            if existing:
                for key, value in spec.items():
                    actual = existing.get(key)
                    if key == 'setting':
                        if json.loads(actual or '{}') != json.loads(value):
                            raise ValueError('同名渠道配置不一致，拒绝覆盖')
                    elif actual != value:
                        raise ValueError('同名渠道配置不一致，拒绝覆盖')
                cursor.execute('SELECT enabled,priority,weight,tag FROM abilities WHERE `group`='
                    + placeholder + ' AND model=' + placeholder + ' AND channel_id=' + placeholder,
                    (spec['group'], spec['models'], existing['id']))
                ability = cursor.fetchone()
                if ability and tuple(ability) != (1, spec['priority'], 1, spec['tag']):
                    raise ValueError('既有渠道能力不一致，拒绝覆盖')
            else:
                ability = None
            prepared.append((spec, existing, ability))
        summary = []
        for spec, existing, ability in prepared:
            channel_id = existing['id'] if existing else None
            if apply and existing is None:
                columns = ','.join('`' + key + '`' for key in spec)
                cursor.execute('INSERT INTO channels (' + columns + ') VALUES ('
                    + ','.join([placeholder] * len(spec)) + ')', tuple(spec.values()))
                channel_id = cursor.lastrowid
            if apply and ability is None:
                cursor.execute('INSERT INTO abilities (`group`,model,channel_id,enabled,priority,weight,tag) VALUES ('
                    + ','.join([placeholder] * 7) + ')',
                    (spec['group'], spec['models'], channel_id, 1, spec['priority'], 1, spec['tag']))
            summary.append({'id': channel_id, 'name': spec['name'], 'model': spec['models'],
                'priority': spec['priority'], 'action': 'existing' if existing else 'create'})
        if apply:
            connection.commit()
        else:
            connection.rollback()
        return {'mode': 'apply' if apply else 'dry-run', 'channels': summary,
            'authorization': '将四条渠道加入已有生图分发规则，再执行正式权限同步；本脚本不授予用户权限'}
    except Exception:
        connection.rollback()
        raise
    finally:
        cursor.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--kie-base-url', required=True)
    parser.add_argument('--apimart-base-url', required=True)
    parser.add_argument('--group', default='default')
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    specs = channel_specs(args.kie_base_url, args.apimart_base_url,
        os.environ.get('IMAGE_ADAPTER_INTERNAL_TOKEN', ''), args.group)
    url = urlsplit(os.environ.get('IMAGE_CHANNEL_DATABASE_URL', ''))
    if url.scheme != 'mysql' or not url.hostname or not url.path.strip('/'):
        raise ValueError('请在私有环境配置 IMAGE_CHANNEL_DATABASE_URL=mysql://...')
    import pymysql
    connection = pymysql.connect(host=url.hostname, port=url.port or 3306,
        user=unquote(url.username or ''), password=unquote(url.password or ''),
        database=url.path.strip('/'), charset='utf8mb4', autocommit=False)
    try:
        print(json.dumps(configure(connection, specs, apply=args.apply), ensure_ascii=False, indent=2))
    finally:
        connection.close()


if __name__ == '__main__':
    try:
        main()
    except Exception:
        # 不输出连接串、SQL 参数、内部 key 或数据库驱动错误正文。
        raise SystemExit('配置未完成，请核对私有连接配置、渠道预览和数据库状态')
