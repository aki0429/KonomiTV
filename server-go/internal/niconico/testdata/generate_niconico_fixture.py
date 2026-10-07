"""NiconicoRouter / OAuthCallbackResponse (Python 版) のゴールデンフィクスチャを生成する。

app パッケージ (Tortoise/FastAPI 依存) を最小スタブで置き換え、本物の
server/app/routers/NiconicoRouter.py と server/app/utils/OAuthCallbackResponse.py を
読み込んで、実際のハンドラー呼び出し結果を期待値として書き出す。

実行方法 (server-go 側から):
    cd <repo>/server-go
    uv run --no-project --python 3.12 --with fastapi --with starlette --with httpx \
      --with python-jose python internal/niconico/testdata/generate_niconico_fixture.py

注意: 外部ネットワークへは一切アクセスしない (ニコニコ OAuth API への通信は
httpx.MockTransport を差し込んで遮断している) 。
クライアントシークレットの実値はスタブのダミー値に置き換えており、出力にも含めない。
"""

from __future__ import annotations

import ast
import asyncio
import base64
import hashlib
import importlib.util as importlib_util
import json
import sys
import types
from pathlib import Path

# テスト用のダミー値 (本物の値は使わない)
TEST_INTERLACED_SECRET = "test-client-secret-for-server-go"
TEST_USER_ACCESS_TOKEN = "test-user-access-token"

HERE = Path(__file__).resolve().parent
REPO_ROOT = HERE.parents[3]
PY_APP_DIR = REPO_ROOT / "server" / "app"
OUTPUT_PATH = HERE / "niconico_fixture.json"


# ---------------------------------------------------------------------------
# app パッケージのスタブ
# ---------------------------------------------------------------------------


def install_app_stubs() -> None:
    app_module = types.ModuleType("app")
    app_module.__path__ = [str(PY_APP_DIR)]

    logging_module = types.ModuleType("app.logging")

    class _Logging:
        def info(self, *args, **kwargs):
            return None

        def warning(self, *args, **kwargs):
            return None

        def error(self, *args, **kwargs):
            return None

    _logging = _Logging()
    logging_module.logging = _logging
    logging_module.error = _logging.error
    logging_module.warning = _logging.warning
    logging_module.info = _logging.info
    logging_module.logger = None

    # app.constants: NiconicoRouter が必要とするものだけ
    constants_module = types.ModuleType("app.constants")
    constants_module.API_REQUEST_HEADERS = {"User-Agent": "KonomiTV/0.14.1"}
    constants_module.NICONICO_OAUTH_CLIENT_ID = "4JTJdyBZLwMJwaI7"
    constants_module.HTTPX_CLIENT = lambda: None  # テストごとに差し替える

    # app.schemas: ThirdpartyAuthURL のみ
    schemas_module = types.ModuleType("app.schemas")
    from pydantic import BaseModel

    class ThirdpartyAuthURL(BaseModel):
        authorization_url: str

    schemas_module.ThirdpartyAuthURL = ThirdpartyAuthURL

    # app.models.User
    models_module = types.ModuleType("app.models")
    models_module.__path__ = [str(PY_APP_DIR / "models")]
    user_module = types.ModuleType("app.models.User")

    class User:
        def __init__(self):
            self.id = 1
            self.niconico_user_id = None
            self.niconico_user_name = None
            self.niconico_user_premium = None
            self.niconico_access_token = None
            self.niconico_refresh_token = None
            self.save_calls = 0

        async def save(self):
            self.save_calls += 1

    user_module.User = User

    # app.routers.UsersRouter: GetCurrentUser
    routers_module = types.ModuleType("app.routers")
    routers_module.__path__ = [str(PY_APP_DIR / "routers")]
    users_router_module = types.ModuleType("app.routers.UsersRouter")

    from fastapi import HTTPException, status

    users_router_module.current_user_holder = {"user": None}

    async def GetCurrentUser(token):  # noqa: N802
        if token == TEST_USER_ACCESS_TOKEN:
            if users_router_module.current_user_holder["user"] is None:
                users_router_module.current_user_holder["user"] = User()
            return users_router_module.current_user_holder["user"]
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Access token is invalid",
            headers={"WWW-Authenticate": "Bearer"},
        )

    users_router_module.GetCurrentUser = GetCurrentUser

    # app.utils: Interlaced
    utils_module = types.ModuleType("app.utils")
    utils_module.__path__ = [str(PY_APP_DIR / "utils")]
    utils_module.Interlaced = lambda n: TEST_INTERLACED_SECRET

    # 本物の OAuthCallbackResponse を読み込んで app.utils.OAuthCallbackResponse とする
    oauth_spec = importlib_util.spec_from_file_location(
        "app.utils.OAuthCallbackResponse", PY_APP_DIR / "utils" / "OAuthCallbackResponse.py"
    )
    oauth_module = importlib_util.module_from_spec(oauth_spec)
    sys.modules["app.utils.OAuthCallbackResponse"] = oauth_module
    oauth_spec.loader.exec_module(oauth_module)
    utils_module.OAuthCallbackResponse = oauth_module

    sys.modules.update(
        {
            "app": app_module,
            "app.logging": logging_module,
            "app.constants": constants_module,
            "app.schemas": schemas_module,
            "app.models": models_module,
            "app.models.User": user_module,
            "app.routers": routers_module,
            "app.routers.UsersRouter": users_router_module,
            "app.utils": utils_module,
        }
    )
    app_module.logging = logging_module
    app_module.schemas = schemas_module
    app_module.models = models_module
    app_module.routers = routers_module
    app_module.utils = utils_module
    models_module.User = user_module


def load_niconico_router():
    spec = importlib_util.spec_from_file_location(
        "app.routers.NiconicoRouter", PY_APP_DIR / "routers" / "NiconicoRouter.py"
    )
    module = importlib_util.module_from_spec(spec)
    sys.modules["app.routers.NiconicoRouter"] = module
    spec.loader.exec_module(module)
    return module


# ---------------------------------------------------------------------------
# フィクスチャ生成
# ---------------------------------------------------------------------------


def make_request(host: str, origin: str | None, authorization: str | None, scheme: str = "https", port: int = 443):
    from starlette.requests import Request

    headers = [(b"host", host.encode())]
    if origin is not None:
        headers.append((b"origin", origin.encode()))
    if authorization is not None:
        headers.append((b"authorization", authorization.encode()))

    scope = {
        "type": "http",
        "asgi": {"version": "3.0"},
        "http_version": "1.1",
        "method": "GET",
        "scheme": scheme,
        "path": "/api/niconico/auth",
        "raw_path": b"/api/niconico/auth",
        "query_string": b"",
        "headers": headers,
        "server": (host.split(":")[0], port),
        "client": ("127.0.0.1", 12345),
    }
    return Request(scope)


def build_auth_url_cases(module) -> list[dict]:
    cases = [
        {"name": "with_origin", "host": "konomitv.example.com", "origin": "https://client.example.com", "authorization": f"Bearer {TEST_USER_ACCESS_TOKEN}"},
        {"name": "without_origin", "host": "konomitv.example.com", "origin": None, "authorization": f"Bearer {TEST_USER_ACCESS_TOKEN}"},
        {"name": "origin_with_trailing_slash", "host": "konomitv.example.com", "origin": "https://client.example.com/", "authorization": f"Bearer {TEST_USER_ACCESS_TOKEN}"},
        {"name": "host_with_port", "host": "konomitv.example.com:7010", "origin": None, "authorization": None},
    ]

    results = []
    for case in cases:
        request = make_request(case["host"], case["origin"], case["authorization"])
        response = asyncio.run(module.NiconicoAuthURLAPI(request=request, current_user=None))
        results.append(
            {
                "name": case["name"],
                "host": case["host"],
                "origin": case["origin"],
                "authorization": case["authorization"],
                "expected_netloc": request.url.netloc,
                "expected": response,
            }
        )
    return results


def make_id_token(sub: int) -> str:
    from jose import jwt

    return jwt.encode(
        {"sub": str(sub), "aud": "4JTJdyBZLwMJwaI7", "iss": "https://oauth.nicovideo.jp"},
        "dummy-key-for-fixture",
        algorithm="HS256",
    )


def build_callback_cases(module) -> list[dict]:
    """NiconicoAuthCallbackAPI の期待値を生成する (HTTP は MockTransport で遮断) 。"""
    import httpx
    import urllib.parse

    users_router_module = sys.modules["app.routers.UsersRouter"]

    cases = [
        {"name": "error_denied", "error": "access_denied", "code": None},
        {"name": "missing_code", "error": None, "code": None},
        {"name": "token_api_error", "error": None, "code": "code", "token_status": 400,
         "token_body": {"error": "invalid_grant"}},
        {"name": "token_api_network_error", "error": None, "code": "code", "token_network_error": True},
        {"name": "user_api_error", "error": None, "code": "code", "token_status": 200,
         "token_body": {"access_token": "at", "refresh_token": "rt", "id_token": make_id_token(12345)},
         "user_status": 500, "user_body": {}},
        {"name": "user_api_network_error", "error": None, "code": "code", "token_status": 200,
         "token_body": {"access_token": "at", "refresh_token": "rt", "id_token": make_id_token(12345)},
         "user_network_error": True},
        {"name": "success", "error": None, "code": "code", "token_status": 200,
         "token_body": {"access_token": "at-1", "refresh_token": "rt-1", "id_token": make_id_token(67890)},
         "user_status": 200, "user_body": {"data": {"user": {"nickname": "テストユーザー", "isPremium": True}}},
         "expect_user_id": 67890},
    ]

    results = []
    for case in cases:
        requests = []

        def handler(request: httpx.Request, _case=case, _requests=requests) -> object:
            _requests.append(
                {
                    "method": request.method,
                    "path": request.url.path,
                    "user_agent": request.headers.get("user-agent"),
                    "x_frontend_id": request.headers.get("x-frontend-id"),
                    "form": dict(urllib.parse.parse_qsl(request.content.decode("utf-8")))
                    if "x-www-form-urlencoded" in request.headers.get("content-type", "")
                    else None,
                }
            )
            path = request.url.path
            if path == "/oauth2/token":
                if _case.get("token_network_error"):
                    raise httpx.ConnectError("connection failed", request=request)
                return httpx.Response(
                    status_code=_case.get("token_status", 200),
                    json=_case.get("token_body", {}),
                    headers={"content-type": "application/json"},
                )
            if path.startswith("/v1/users/"):
                if _case.get("user_network_error"):
                    raise httpx.ConnectError("connection failed", request=request)
                return httpx.Response(
                    status_code=_case.get("user_status", 200),
                    json=_case.get("user_body", {}),
                    headers={"content-type": "application/json"},
                )
            return httpx.Response(status_code=404, json={})

        def factory(_handler=handler):
            return httpx.AsyncClient(transport=httpx.MockTransport(_handler), timeout=3.0)

        # NiconicoRouter は import 時に HTTPX_CLIENT を名前空間へ取り込むため、モジュール側を差し替える
        module.HTTPX_CLIENT = factory
        users_router_module.current_user_holder["user"] = None

        response = asyncio.run(
            module.NiconicoAuthCallbackAPI(
                client="https://client.example.com/",
                user_access_token=TEST_USER_ACCESS_TOKEN,
                code=case.get("code"),
                error=case.get("error"),
            )
        )

        body_html = response.body.decode("utf-8")
        current_user = users_router_module.current_user_holder["user"]
        results.append(
            {
                "name": case["name"],
                "client": "https://client.example.com/",
                "user_access_token": TEST_USER_ACCESS_TOKEN,
                "code": case.get("code"),
                "error": case.get("error"),
                "token_status": case.get("token_status"),
                "token_body": case.get("token_body"),
                "user_status": case.get("user_status"),
                "user_body": case.get("user_body"),
                "token_network_error": case.get("token_network_error", False),
                "user_network_error": case.get("user_network_error", False),
                "expected": {
                    "status_code": response.status_code,
                    "content_type": str(response.headers.get("content-type")),
                    "body_html": body_html,
                    "body_sha256": hashlib.sha256(response.body).hexdigest(),
                },
                "expected_requests": requests,
                "expected_user": None
                if current_user is None
                else {
                    "niconico_user_id": current_user.niconico_user_id,
                    "niconico_user_name": current_user.niconico_user_name,
                    "niconico_user_premium": current_user.niconico_user_premium,
                    "niconico_access_token": current_user.niconico_access_token,
                    "niconico_refresh_token": current_user.niconico_refresh_token,
                    "save_calls": current_user.save_calls,
                },
                # クライアントシークレットはダミー値 (form 内は置換して出力しない)
                "test_interlaced_secret": TEST_INTERLACED_SECRET,
            }
        )

    # フォーム内の client_secret は期待値比較用にダミー値へ置換する
    for case in results:
        for request in case["expected_requests"]:
            if request["form"] and "client_secret" in request["form"]:
                assert request["form"]["client_secret"] == TEST_INTERLACED_SECRET, request["form"]
                request["form"]["client_secret"] = "<client_secret>"
    return results


def build_callback_invalid_token_case(module) -> dict:
    """無効な user_access_token を渡したときの Python 版の挙動を記録する。

    Python 版は except HTTPException 内で cast(Any, ex).message を参照しているが、
    FastAPI の HTTPException に .message は存在しないため AttributeError が送出され、
    Starlette の ServerErrorMiddleware が 500 Internal Server Error を返す。
    """
    exception_type = None
    exception_message = None
    try:
        asyncio.run(
            module.NiconicoAuthCallbackAPI(
                client="https://client.example.com/",
                user_access_token="invalid-user-access-token",
                code="code",
                error=None,
            )
        )
    except BaseException as ex:  # noqa: BLE001
        exception_type = type(ex).__name__
        exception_message = str(ex)
    assert exception_type is not None, "Python 版が例外を送出しなかった"
    return {
        "name": "invalid_user_access_token",
        "client": "https://client.example.com/",
        "user_access_token": "invalid-user-access-token",
        "code": "code",
        "error": None,
        "expected_exception_type": exception_type,
        "expected_exception_message": exception_message,
        "expected_status_code": 500,
    }


def build_missing_param_cases(module) -> list[dict]:
    """必須クエリパラメータが欠落したときの FastAPI の 422 レスポンスを記録する。"""
    from fastapi import FastAPI
    from starlette.testclient import TestClient

    app = FastAPI()
    app.include_router(module.router)
    client = TestClient(app, raise_server_exceptions=False)

    cases = [
        {"name": "missing_all", "query": {}},
        {"name": "missing_user_access_token", "query": {"client": "https://client.example.com/"}},
        {"name": "missing_client", "query": {"user_access_token": TEST_USER_ACCESS_TOKEN}},
    ]
    results = []
    for case in cases:
        response = client.get("/api/niconico/callback", params=case["query"])
        results.append(
            {
                "name": case["name"],
                "query": case["query"],
                "expected_status_code": response.status_code,
                "expected_content_type": str(response.headers.get("content-type")),
                "expected_body": response.json(),
            }
        )
    return results


def build_oauth_callback_response_cases(module) -> list[dict]:
    oauth_module = sys.modules["app.utils.OAuthCallbackResponse"]
    cases = [
        {"detail": "Success", "redirect_to": "https://client.example.com/settings/jikkyo", "status_code": 200},
        {"detail": "Authorization was denied (access_denied)", "redirect_to": "https://client.example.com/settings/jikkyo", "status_code": 401},
        {"detail": "Failed to get access token. (HTTP Error 400)", "redirect_to": "https://client.example.com/settings/jikkyo", "status_code": 500},
        {"detail": "詳細メッセージ & <tag>", "redirect_to": "https://client.example.com/settings/jikkyo", "status_code": 500},
    ]
    results = []
    for case in cases:
        response = oauth_module.OAuthCallbackResponse(**case)
        results.append(
            {
                **case,
                "content_type": str(response.headers.get("content-type")),
                "body_html": response.body.decode("utf-8"),
            }
        )
    return results


def build_logout_cases(module) -> list[dict]:
    user_module = sys.modules["app.models.User"]
    user = user_module.User()
    user.niconico_user_id = 12345
    user.niconico_user_name = "テストユーザー"
    user.niconico_user_premium = True
    user.niconico_access_token = "at"
    user.niconico_refresh_token = "rt"
    asyncio.run(module.NiconicoAccountLogoutAPI(current_user=user))
    return [
        {
            "expected_user": {
                "niconico_user_id": user.niconico_user_id,
                "niconico_user_name": user.niconico_user_name,
                "niconico_user_premium": user.niconico_user_premium,
                "niconico_access_token": user.niconico_access_token,
                "niconico_refresh_token": user.niconico_refresh_token,
                "save_calls": user.save_calls,
            }
        }
    ]


def main() -> None:
    install_app_stubs()
    module = load_niconico_router()

    fixture = {
        "note": "server/app/routers/NiconicoRouter.py (Python 版) から生成した期待値",
        "auth_url_cases": build_auth_url_cases(module),
        "callback_cases": build_callback_cases(module),
        "callback_invalid_token_case": build_callback_invalid_token_case(module),
        "missing_param_cases": build_missing_param_cases(module),
        "oauth_callback_response_cases": build_oauth_callback_response_cases(module),
        "logout_cases": build_logout_cases(module),
    }
    OUTPUT_PATH.write_text(json.dumps(fixture, ensure_ascii=False, indent=1), encoding="utf-8")
    print(f"wrote {OUTPUT_PATH}")
    for case in fixture["auth_url_cases"]:
        print("AUTH", case["name"], case["expected_netloc"])


if __name__ == "__main__":
    main()
