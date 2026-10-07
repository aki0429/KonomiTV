"""TwitterRouter の入力バリデーション (FastAPI/Pydantic) の期待値ジェネレーター。

TwitterRouter を FastAPI にマウントし、httpx の ASGI トランスポート経由で
実際の 422 エラーボディ (FastAPI 形式) を採取して JSON に書き出す。
外部ネットワーク通信は行わない (依存はすべてフェイクに差し替える) 。

    python <this file>
"""
from __future__ import annotations

import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

HERE = os.path.dirname(os.path.abspath(__file__))
with open(os.path.join(HERE, "generate_expected.py"), encoding="utf-8") as file:
    _source = file.read()
exec(compile(_source.split("VIEWER_RESPONSE =")[0], "bootstrap", "exec"))

import httpx  # noqa: E402
from fastapi import Depends, FastAPI  # noqa: E402

import app.routers.TwitterRouter as TwitterRouter  # noqa: E402
from app.routers import UsersRouter  # noqa: E402


class FakeUser:
    id = 1
    name = "tester"


class FakeAccount:
    id = 5
    screen_name = "dummy"


app = FastAPI()
app.include_router(TwitterRouter.router)


async def fake_user() -> FakeUser:
    return FakeUser()


async def fake_account(screen_name: str, current_user: FakeUser = Depends(UsersRouter.GetCurrentUser)) -> FakeAccount:
    return FakeAccount()


app.dependency_overrides[UsersRouter.GetCurrentUser] = fake_user
app.dependency_overrides[TwitterRouter.GetCurrentTwitterAccount] = fake_account


CASES = [
    ("timeline_bad_cursor_type", "GET", "/api/twitter/accounts/dummy/timeline?cursor_type=Bad", None),
    ("search_missing_query", "GET", "/api/twitter/accounts/dummy/search", None),
    ("search_bad_search_type", "GET", "/api/twitter/accounts/dummy/search?query=test&search_type=Bad", None),
    ("video_proxy_missing_url", "GET", "/api/twitter/video-proxy", None),
    ("auth_empty_body", "POST", "/api/twitter/auth", {}),
    ("auth_cookies_txt_null", "POST", "/api/twitter/auth", {"cookies_txt": None}),
    ("auth_cookies_txt_number", "POST", "/api/twitter/auth", {"cookies_txt": 123}),
    ("auth_invalid_browser_info", "POST", "/api/twitter/auth", {"cookies_txt": "# Netscape HTTP Cookie File", "browser_info": {"locale": "ja"}}),
]


def main() -> None:
    import asyncio

    outputs = []

    async def run() -> None:
        transport = httpx.ASGITransport(app=app)
        async with httpx.AsyncClient(transport=transport, base_url="http://testserver") as client:
            for name, method, path, body in CASES:
                response = await client.request(method, path, json=body)
                outputs.append({
                    "name": name,
                    "method": method,
                    "path": path,
                    "body": body,
                    "status_code": response.status_code,
                    "response_json": response.json(),
                })

    asyncio.run(run())
    target = os.path.join(HERE, "twitter_validation_expected.json")
    with open(target, "w", encoding="utf-8", newline="\n") as file:
        json.dump(outputs, file, ensure_ascii=False, indent=2)
        file.write("\n")
    print(f"wrote {target}")
    for item in outputs:
        print(item["name"], item["status_code"], json.dumps(item["response_json"], ensure_ascii=False)[:200])


if __name__ == "__main__":
    main()
