"""Ordinary application HTTPS code, with no sandbox-specific trust handling."""
import asyncio
import json
import os
import sys

import certifi
import httpx


def sync_get(url):
    with httpx.Client(verify=True, trust_env=False) as client:
        response = client.get(url)
        response.raise_for_status()
        return response.text


async def async_get(url):
    async with httpx.AsyncClient(verify=True, trust_env=False) as client:
        response = await client.get(url)
        response.raise_for_status()
        return response.text


if __name__ == '__main__':
    url = sys.argv[1]
    assert sync_get(url) == 'sandbox web page'
    assert asyncio.run(async_get(url)) == 'sandbox web page'
    # Interceptor must retain its closed network even with an extended trust store.
    try:
        sync_get('https://undeclared.example.invalid/')
    except httpx.ConnectError:
        pass
    else:
        raise AssertionError('undeclared destination was reachable')
    assert os.geteuid() != 0
    print(json.dumps({'python': sys.executable, 'httpx': httpx.__version__,
                      'certifi': certifi.__version__, 'uid': os.geteuid(),
                      'sync_https': True, 'async_https': True, 'undeclared_blocked': True}))
