"""验证发布期间的连接等待、完整透传与半关闭，不连接生产。"""
import asyncio,json,socket,subprocess,sys,tempfile,unittest
from pathlib import Path

class ReleaseHoldTest(unittest.IsolatedAsyncioTestCase):
    async def test_waits_for_ready_then_transfers_exact_bytes_once(self):
        calls=[];payload=b'hello'*30000;response=b'response'*50000
        async def upstream(reader,writer):
            calls.append(await reader.read());writer.write(response);await writer.drain();writer.close()
        target=await asyncio.start_server(upstream,'127.0.0.1',0)
        with tempfile.TemporaryDirectory() as directory:
            with socket.socket() as sock:sock.bind(('127.0.0.1',0));port=sock.getsockname()[1]
            process=subprocess.Popen([sys.executable,str(Path(__file__).with_name('release-tcp-hold.py')),'--listen-port',str(port),'--target-port',str(target.sockets[0].getsockname()[1]),'--state-dir',directory])
            try:
                async with asyncio.timeout(10):
                    while not (Path(directory)/'status.json').exists():await asyncio.sleep(.01)
                    reader,writer=await asyncio.open_connection('127.0.0.1',port)
                    writer.write(payload);await writer.drain();writer.write_eof()
                    await asyncio.sleep(.1)
                    self.assertEqual(calls,[])
                    (Path(directory)/'ready').touch()
                    self.assertEqual(await reader.read(),response)
                    writer.close();await writer.wait_closed()
                    self.assertEqual(calls,[payload])
                    while json.loads((Path(directory)/'status.json').read_text())['active']:await asyncio.sleep(.01)
                    state=json.loads((Path(directory)/'status.json').read_text())
                    self.assertEqual(state,{'active':0,'released':1,'failed':0})
            finally:
                process.terminate();process.wait(timeout=5)
        target.close();await target.wait_closed()

if __name__=='__main__':unittest.main()
