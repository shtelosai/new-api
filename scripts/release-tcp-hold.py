#!/usr/bin/env python3
"""单写网关切换时暂存新 TCP 连接；就绪后透传，不记录或重发请求内容。"""
import argparse,asyncio,json,os
from pathlib import Path

async def main(args):
    state={'active':0,'released':0,'failed':0}
    root=Path(args.state_dir);root.mkdir(parents=True,exist_ok=True)
    def save():
        temp=root/'status.tmp';temp.write_text(json.dumps(state));os.replace(temp,root/'status.json')
    async def handle(reader,writer):
        state['active']+=1;save();up=None
        try:
            async with asyncio.timeout(45):
                while not (root/'ready').exists():await asyncio.sleep(.05)
                upstream,up=await asyncio.open_connection('127.0.0.1',args.target_port)
            state['released']+=1;save()
            async def pipe(source,target):
                while data:=await source.read(65536):
                    target.write(data);await target.drain()
                if target.can_write_eof():target.write_eof()
            left=asyncio.create_task(pipe(reader,up));right=asyncio.create_task(pipe(upstream,writer))
            await asyncio.gather(left,right)
        except Exception:state['failed']+=1
        finally:
            writer.close()
            if up:up.close()
            state['active']-=1;save()
    server=await asyncio.start_server(handle,'127.0.0.1',args.listen_port)
    save()
    async with server:await server.serve_forever()

if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--listen-port',type=int,required=True);p.add_argument('--target-port',type=int,required=True);p.add_argument('--state-dir',required=True);args=p.parse_args()
    asyncio.run(main(args))
