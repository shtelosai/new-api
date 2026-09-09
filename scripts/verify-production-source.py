#!/usr/bin/env python3
"""发布前验证自维护 main；不修改代码、分支或远端。"""
import subprocess
import sys


def git(*args):
    return subprocess.check_output(['git', *args], text=True).strip()


def main():
    if git('branch', '--show-current') != 'main':
        raise ValueError('生产只能从自维护 main 分支构建')
    if git('status', '--porcelain'):
        raise ValueError('工作区不干净，必须先提交或移出非发布文件')
    upstream = git('rev-parse', '--abbrev-ref', '@{upstream}')
    remote = git('config', 'branch.main.remote')
    if git('config', 'branch.main.merge') != 'refs/heads/main':
        raise ValueError('main 必须跟踪公司仓库的 main')
    allowed = {'git@github.com:shtelosai/new-api.git', 'https://github.com/shtelosai/new-api.git'}
    if git('remote', 'get-url', remote) not in allowed:
        raise ValueError('发布源必须是 shtelosai/new-api，不能使用官方上游')
    head = git('rev-parse', 'HEAD')
    remote_head = git('ls-remote', remote, 'refs/heads/main').split()
    if not remote_head or remote_head[0] != head:
        raise ValueError('本地 main 与公司远端 main 不一致')
    print(f'PASS branch=main upstream={upstream} commit={head}')


if __name__ == '__main__':
    try:
        main()
    except (ValueError, subprocess.CalledProcessError) as error:
        print(f'发布源检查失败：{error}', file=sys.stderr)
        sys.exit(1)
