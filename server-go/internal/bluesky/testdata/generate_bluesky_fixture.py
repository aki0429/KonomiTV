"""BlueskyRouter / BlueskyAPI (Python 版) のゴールデンフィクスチャを生成する。

app パッケージ (Tortoise/FastAPI 依存) を極力ロードせずに、本物の
server/app/utils/BlueskyAPI.py を最小スタブで読み込み、純粋関数と
実際の atproto SDK を使ったクライアント挙動の期待値を書き出す。

実行方法 (server-go 側から):
    cd <repo>/server-go
    uv run --no-project --python 3.12 \
      --with atproto==0.0.68 --with cryptography --with pillow --with python-jose \
      python internal/bluesky/testdata/generate_bluesky_fixture.py

注意: 外部ネットワークへは一切アクセスしない (atproto の HTTP 通信は
AsyncRequest をフェイクに差し替えて遮断している) 。
"""

from __future__ import annotations

import ast
import asyncio
import base64
import hashlib
import importlib.util as importlib_util
import io
import json
import os
import sys
import types
from pathlib import Path

# ---------------------------------------------------------------------------

HERE = Path(__file__).resolve().parent                                       # .../server-go/internal/bluesky/testdata
REPO_ROOT = HERE.parents[3]                                                  # .../KonomiTV
PY_APP_DIR = REPO_ROOT / "server" / "app"
OUTPUT_PATH = HERE / "bluesky_fixture.json"

# テスト用のダミー秘密鍵 (本物の jwt_secret.dat は使わない)
TEST_JWT_SECRET = "test-secret-for-server-go-compatibility-0123456789abcdef"

# 固定時刻 (アカウント連携と投稿の createdAt を決定的にする)
FIXED_NOW_ISO = "2026-10-07T12:34:56.789012+00:00"

# ---------------------------------------------------------------------------
# app パッケージのスタブ (本物の __init__.py は読み込まない)
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

    # app.logging モジュールは logging オブジェクトをエクスポートしている
    # (from app import logging; logging.error(...) の形で使われる)
    _logging = _Logging()
    logging_module.logging = _logging
    logging_module.error = _logging.error
    logging_module.warning = _logging.warning
    logging_module.info = _logging.info
    logging_module.logger = None

    constants_module = types.ModuleType("app.constants")
    from zoneinfo import ZoneInfo

    constants_module.JST = ZoneInfo("Asia/Tokyo")
    constants_module.BLUESKY_ACCOUNT_SESSION_ENCRYPTION_PREFIX = "enc:"
    import hashlib as _hashlib

    constants_module.BLUESKY_ACCOUNT_SESSION_FERNET_KEY = base64.urlsafe_b64encode(
        _hashlib.sha256(f"bluesky:{TEST_JWT_SECRET}".encode()).digest()
    )

    schemas_module = types.ModuleType("app.schemas")
    schemas_source = (PY_APP_DIR / "schemas.py").read_text(encoding="utf-8")
    schemas_tree = ast.parse(schemas_source)
    wanted_classes = {
        "Tweet",
        "TweetUser",
        "TwitterAPIResult",
        "PostTweetResult",
        "TimelineLoadMoreCursor",
        "TimelineTweetsResult",
    }
    collected = [
        node
        for node in schemas_tree.body
        if isinstance(node, ast.ClassDef) and node.name in wanted_classes
    ]
    assert {node.name for node in collected} == wanted_classes, "schemas.py から必要なクラスを抽出できなかった"
    schemas_code = "\n".join(
        [
            "from __future__ import annotations",
            "from datetime import date, datetime",
            "from typing import Annotated, Literal, TypedDict",
            "from pydantic import BaseModel, Field, RootModel, computed_field",
        ]
        + [ast.unparse(node) for node in collected]
    )
    exec(compile(schemas_code, "app/schemas-stub.py", "exec"), schemas_module.__dict__)

    models_module = types.ModuleType("app.models")
    bluesky_account_module = types.ModuleType("app.models.BlueskyAccount")
    from cryptography.fernet import Fernet

    class BlueskyAccount:
        """server/app/models/BlueskyAccount.py の encrypt/decrypt 部分だけのスタブ"""

        def __init__(self, **kwargs):
            self.id = kwargs.get("id", 1)
            self.user_id = kwargs.get("user_id", 42)
            self.did = kwargs.get("did", "")
            self.handle = kwargs.get("handle", "")
            self.name = kwargs.get("name", "")
            self.icon_url = kwargs.get("icon_url", "")
            self.session_string = kwargs.get("session_string", "")

        def encryptSessionString(self, plain_text: str) -> str:
            if plain_text == "":
                return ""
            fernet = Fernet(constants_module.BLUESKY_ACCOUNT_SESSION_FERNET_KEY)
            encrypted = fernet.encrypt(plain_text.encode("utf-8"), time=1700000000)
            # 決定論的にするため IV / タイムスタンプを固定する
            return f"{constants_module.BLUESKY_ACCOUNT_SESSION_ENCRYPTION_PREFIX}{encrypted.decode('utf-8')}"

        def decryptSessionString(self) -> str:
            return self.session_string

        async def save(self) -> None:
            # セッション更新通知の検証用に、保存されたセッション文字列を記録する
            sink = getattr(BlueskyAccount, "_save_sink", None)
            if sink is not None:
                sink(self.session_string)
            return None

        _save_sink = None

    bluesky_account_module.BlueskyAccount = BlueskyAccount
    models_module.BlueskyAccount = bluesky_account_module

    sys.modules.update(
        {
            "app": app_module,
            "app.logging": logging_module,
            "app.constants": constants_module,
            "app.schemas": schemas_module,
            "app.models": models_module,
            "app.models.BlueskyAccount": bluesky_account_module,
        }
    )


def load_bluesky_api_module():
    install_app_stubs()
    spec = importlib_util.spec_from_file_location("konomitv_bluesky_api", PY_APP_DIR / "utils" / "BlueskyAPI.py")
    assert spec is not None and spec.loader is not None
    module = importlib_util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


# ---------------------------------------------------------------------------
# 純粋関数のフィクスチャ
# ---------------------------------------------------------------------------


def build_handle_cases(module) -> list[dict]:
    cases = [
        "  KonomiTV.bsky.social  ",
        "@KonomiTV.bsky.social",
        "https://bsky.app/profile/KonomiTV.bsky.social",
        "https://bsky.app/profile/KonomiTV.bsky.social/post/3kabc",
        "http://bsky.app/profile/KonomiTV.bsky.social?lang=ja",
        "did:plc:abcdefg",
        "こんみつ.bsky.social",
        "@did:plc:ABC",
        "\t@Example.Bsky.Social\n",
        "",
    ]
    return [{"input": value, "output": module.BlueskyAPI.normalizeBlueskyHandle(value)} for value in cases]


def build_datetime_cases() -> list[dict]:
    values = [
        "2026-10-07T12:34:56.789Z",
        "2026-10-07T12:34:56+00:00",
        "2026-10-07T12:34:56.789012+09:00",
        "2026-10-07T03:34:56.100Z",
    ]
    return [{"input": value} for value in values]


def build_tag_cases(module) -> list[dict]:
    texts = [
        "サイト https://example.com,いいね！",
        "これはテストです #KonomiTV #テスト",
        "末尾 https://example.com。",
        "#＃全角ハッシュタグ",
        "https://example.com/path?a=1&b=2)」",
        "記号のみ #。 タグなし",
        "URL なしタグ #tag と # と #あ",
        "複数 https://a.example.com と https://b.example.com/x,y #c #d",
        "https://example.com",
        "emptyなハッシュ ##",
        "テキストのみ",
        "全角スペース\u3000#タグ\u3000おわり",
        "括弧 (https://example.com/test) を含む",
    ]
    results = []
    for text in texts:
        builder = module.BlueskyAPI._buildTextBuilder(text)
        results.append(
            {
                "text": text,
                "built_text": builder.build_text(),
                "facets": [json.loads(f.model_dump_json(exclude_none=True, by_alias=True)) for f in builder.build_facets()],
            }
        )
    return results


def build_facet_expand_cases(module) -> list[dict]:
    from atproto import models

    def make(text: str, byte_start: int, byte_end: int, uri: str):
        return models.AppBskyRichtextFacet.Main(
            features=[models.AppBskyRichtextFacet.Link(uri=uri)],
            index=models.AppBskyRichtextFacet.ByteSlice(byte_start=byte_start, byte_end=byte_end),
        )

    text = "リンクは こちら https://example.com/test です"
    encoded = text.encode("utf-8")
    start = encoded.index(b"https")
    cases = []

    facet_good = make(text, start, start + len("https://example.com/test"), "https://example.com/test")
    cases.append(
        {"text": text, "facets": [json.loads(facet_good.model_dump_json(exclude_none=True, by_alias=True))], "output": None}
    )

    # 不正な byte range は無視される (警告ログのみ)
    facet_bad = make(text, 3, 2, "https://example.com/test")
    cases.append(
        {"text": text, "facets": [json.loads(facet_bad.model_dump_json(exclude_none=True, by_alias=True))], "output": None}
    )

    # 複数 facet (後方から置換される)
    text2 = "A https://a.example B https://b.example C"
    enc2 = text2.encode("utf-8")
    s1 = enc2.index(b"https://a")
    s2 = enc2.index(b"https://b")
    facets = [
        make(text2, s1, s1 + len("https://a.example"), "https://a.example/expanded?a=1"),
        make(text2, s2, s2 + len("https://b.example"), "https://b.example"),
    ]
    cases.append(
        {"text": text2, "facets": [json.loads(f.model_dump_json(exclude_none=True, by_alias=True)) for f in facets], "output": None}
    )

    cases.append({"text": text, "facets": [], "output": None})
    cases.append({"text": text, "facets": None, "output": None})
    return cases


# ---------------------------------------------------------------------------
# PostView 変換のフィクスチャ
# ---------------------------------------------------------------------------


def build_post_views() -> list[dict]:
    def author(did, handle, name=None, avatar=None):
        value = {"did": did, "handle": handle}
        if name is not None:
            value["displayName"] = name
        if avatar is not None:
            value["avatar"] = avatar
        return value

    views = []

    # 1. 本文 + link facet + images embed + viewer 状態あり
    views.append(
        {
            "name": "plain_with_images_and_viewer",
            "post": {
                "$type": "app.bsky.feed.defs#postView",
                "uri": "at://did:plc:test/app.bsky.feed.post/3kplain",
                "cid": "bafyplain",
                "author": author("did:plc:test", "test.bsky.social", "テスト", "https://cdn.example/avatar.png"),
                "record": {
                    "$type": "app.bsky.feed.post",
                    "text": "リンクは こちら https://example.com/short です #タグ",
                    "createdAt": "2026-10-07T03:34:56.789Z",
                    "facets": [
                        {
                            "index": {"byteStart": 22, "byteEnd": 46},
                            "features": [{"$type": "app.bsky.richtext.facet#link", "uri": "https://example.com/very/long/url"}],
                        }
                    ],
                },
                "embed": {
                    "$type": "app.bsky.embed.images#view",
                    "images": [
                        {"thumb": "https://cdn.example/1.jpg", "fullsize": "https://cdn.example/1.png", "alt": "1", "aspectRatio": {"width": 1, "height": 1}},
                        {"thumb": "https://cdn.example/2.jpg", "fullsize": "https://cdn.example/2.png", "alt": "2", "aspectRatio": {"width": 1, "height": 1}},
                    ],
                },
                "replyCount": 0,
                "repostCount": 3,
                "likeCount": 5,
                "quoteCount": 0,
                "indexedAt": "2026-10-07T03:34:57.000Z",
                "viewer": {"repost": "at://did:plc:test/app.bsky.feed.repost/1", "like": "at://did:plc:test/app.bsky.feed.like/1"},
            },
        }
    )

    # 2. reason=repost 付き (リポストのネスト表示)
    views.append(
        {
            "name": "repost_reason",
            "reason": {
                "$type": "app.bsky.feed.defs#reasonRepost",
                "by": author("did:plc:reposter", "reposter.bsky.social", "", None),
                "indexedAt": "2026-10-07T04:00:00.000Z",
            },
            "post": {
                "$type": "app.bsky.feed.defs#postView",
                "uri": "at://did:plc:original/app.bsky.feed.post/3korig",
                "cid": "bafyorig",
                "author": author("did:plc:original", "original.bsky.social"),
                "record": {
                    "$type": "app.bsky.feed.post",
                    "text": "オリジナル投稿",
                    "createdAt": "2026-10-06T01:02:03.000+09:00",
                },
                "replyCount": 1,
                "repostCount": 0,
                "likeCount": 2,
                "quoteCount": 0,
                "indexedAt": "2026-10-06T01:02:04.000+09:00",
                "viewer": {},
            },
        }
    )

    # 3. 未知の embed ($type) と未知の reason
    views.append(
        {
            "name": "unknown_embed_and_reason",
            "reason": {"$type": "app.bsky.feed.defs#reasonPin", "indexedAt": "2026-10-07T04:00:00.000Z"},
            "post": {
                "$type": "app.bsky.feed.defs#postView",
                "uri": "at://did:plc:test/app.bsky.feed.post/3kunknown",
                "cid": "bafyunknown",
                "author": author("did:plc:test", "test.bsky.social"),
                "record": {"$type": "app.bsky.feed.post", "text": "未知 embed", "createdAt": "2026-10-07T03:34:56.000Z"},
                "embed": {
                    "$type": "app.bsky.embed.recordWithMedia#view",
                    "record": {
                        "$type": "app.bsky.embed.record#view",
                        "record": {"$type": "app.bsky.embed.record#viewNotFound", "uri": "at://x", "notFound": True},
                    },
                    "media": {"$type": "app.bsky.embed.futureMedia#view", "novel": True},
                },
                "replyCount": 0,
                "repostCount": 0,
                "likeCount": 0,
                "quoteCount": 0,
                "indexedAt": "2026-10-07T03:34:57.000Z",
            },
        }
    )

    # 4. record が app.bsky.feed.post でない (本文が取れない) ケース
    views.append(
        {
            "name": "non_post_record",
            "post": {
                "$type": "app.bsky.feed.defs#postView",
                "uri": "at://did:plc:test/app.bsky.feed.post/3kweird",
                "cid": "bafyweird",
                "author": author("did:plc:test", "test.bsky.social"),
                "record": {"$type": "app.bsky.feed.repost", "createdAt": "2020-01-01T00:00:00.000Z", "subject": {"cid": "x", "uri": "at://y"}},
                "replyCount": None,
                "repostCount": None,
                "likeCount": None,
                "quoteCount": None,
                "indexedAt": "2026-10-07T03:34:57.000Z",
            },
        }
    )

    # 5. video / external embed (画像 URL は載せない)
    views.append(
        {
            "name": "video_embed",
            "post": {
                "$type": "app.bsky.feed.defs#postView",
                "uri": "at://did:plc:test/app.bsky.feed.post/3kvideo",
                "cid": "bafyvideo",
                "author": author("did:plc:test", "test.bsky.social"),
                "record": {"$type": "app.bsky.feed.post", "text": "動画", "createdAt": "2026-10-07T03:34:56.000Z"},
                "embed": {"$type": "app.bsky.embed.video#view", "cid": "b", "playlist": "https://video.example/playlist.m3u8", "thumbnail": "https://video.example/thumb.jpg"},
                "replyCount": 0,
                "repostCount": 0,
                "likeCount": 0,
                "quoteCount": 0,
                "indexedAt": "2026-10-07T03:34:57.000Z",
            },
        }
    )

    return views


# ---------------------------------------------------------------------------
# クライアント挙動のフィクスチャ (atproto SDK をフェイク HTTP で駆動)
# ---------------------------------------------------------------------------


def b64url(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


def make_jwt(exp_offset_seconds: int = 3600) -> str:
    import time as _time

    header = b64url(json.dumps({"typ": "JWT", "alg": "none"}).encode())
    payload = b64url(
        json.dumps(
            {
                "scope": "com.atproto.access",
                "sub": "did:plc:test",
                "iat": int(_time.time()) - 60,
                "exp": int(_time.time()) + exp_offset_seconds,
            }
        ).encode()
    )
    signature = b64url(bytes(range(32)))
    return f"{header}.{payload}.{signature}"


SESSION_HANDLE = "test.bsky.social"
SESSION_DID = "did:plc:test"
PDS_ENDPOINT = "https://pds.example.com"
PROFILE = {
    "did": SESSION_DID,
    "handle": SESSION_HANDLE,
    "displayName": "テストアカウント",
    "avatar": "https://cdn.example/avatar.png",
}


def build_client_scenarios() -> list[dict]:
    access_jwt = make_jwt(3600)
    refresh_jwt = make_jwt(7200)
    session_string = ":::".join([SESSION_HANDLE, SESSION_DID, access_jwt, refresh_jwt, PDS_ENDPOINT])

    timeline_feed = [
        {
            "post": {
                "$type": "app.bsky.feed.defs#postView",
                "uri": "at://did:plc:test/app.bsky.feed.post/3ktl1",
                "cid": "bafytl1",
                "author": {"did": "did:plc:test", "handle": SESSION_HANDLE, "displayName": "テストアカウント"},
                "record": {"$type": "app.bsky.feed.post", "text": "タイムライン1 https://example.com", "createdAt": "2026-10-07T03:34:56.789Z"},
                "replyCount": 0,
                "repostCount": 1,
                "likeCount": 2,
                "quoteCount": 0,
                "indexedAt": "2026-10-07T03:34:57.000Z",
                "viewer": {"like": "at://did:plc:test/app.bsky.feed.like/9"},
            }
        }
    ]
    search_posts = [timeline_feed[0]["post"]]

    tiny_image = make_test_png_bytes()

    return [
        {
            "name": "home_latest_timeline",
            "session_string": session_string,
            "operation": {"type": "home_latest_timeline", "cursor_id": None},
            "responses": [
                {"method": "GET", "path": "/xrpc/app.bsky.actor.getProfile", "body": PROFILE},
                {"method": "GET", "path": "/xrpc/app.bsky.feed.getTimeline", "body": {"cursor": "next-cursor-1", "feed": timeline_feed}},
            ],
        },
        {
            "name": "home_latest_timeline_with_cursor",
            "session_string": session_string,
            "operation": {"type": "home_latest_timeline", "cursor_id": "cursor-in"},
            "responses": [
                {"method": "GET", "path": "/xrpc/app.bsky.actor.getProfile", "body": PROFILE},
                {"method": "GET", "path": "/xrpc/app.bsky.feed.getTimeline", "body": {"feed": timeline_feed}},
            ],
        },
        {
            "name": "search_timeline",
            "session_string": session_string,
            "operation": {"type": "search_timeline", "query": "KonomiTV テスト", "cursor_id": None},
            "responses": [
                {"method": "GET", "path": "/xrpc/app.bsky.actor.getProfile", "body": PROFILE},
                {"method": "GET", "path": "/xrpc/app.bsky.feed.searchPosts", "body": {"cursor": "search-cursor", "posts": search_posts}},
            ],
        },
        {
            "name": "create_post_text_and_image",
            "session_string": session_string,
            "image": {"name": "test.png", "content_type": "image/png", "data": base64.b64encode(tiny_image).decode("ascii")},
            "operation": {"type": "create_post", "text": "投稿テスト https://example.com #KonomiTV", "with_image": True, "reply_to": None},
            "responses": [
                {"method": "GET", "path": "/xrpc/app.bsky.actor.getProfile", "body": PROFILE},
                {"method": "POST", "path": "/xrpc/com.atproto.repo.uploadBlob", "body": {"blob": {"$type": "blob", "ref": {"$link": "bafyblob"}, "mimeType": "image/png", "size": len(tiny_image)}}},
                {"method": "POST", "path": "/xrpc/com.atproto.repo.createRecord", "body": {"uri": "at://did:plc:test/app.bsky.feed.post/3knew", "cid": "bafynew"}},
            ],
        },
        {
            "name": "create_post_reply",
            "session_string": session_string,
            "operation": {
                "type": "create_post",
                "text": "返信です",
                "with_image": False,
                "reply_to": {
                    "root_uri": "at://did:plc:test/app.bsky.feed.post/3kroot",
                    "root_cid": "bafyroot",
                    "parent_uri": "at://did:plc:test/app.bsky.feed.post/3kparent",
                    "parent_cid": "bafyparent",
                },
            },
            "responses": [
                {"method": "GET", "path": "/xrpc/app.bsky.actor.getProfile", "body": PROFILE},
                {"method": "POST", "path": "/xrpc/com.atproto.repo.createRecord", "body": {"uri": "at://did:plc:test/app.bsky.feed.post/3kreply", "cid": "bafyreply"}},
            ],
        },
        {
            "name": "create_post_too_many_images",
            "session_string": session_string,
            "operation": {"type": "create_post", "text": "5枚", "with_image_count": 5},
            "responses": [
                {"method": "GET", "path": "/xrpc/app.bsky.actor.getProfile", "body": PROFILE},
            ],
        },
        {
            "name": "create_repost",
            "session_string": session_string,
            "operation": {"type": "create_repost", "post_id": "at://did:plc:test/app.bsky.feed.post/3kother"},
            "responses": [
                {"method": "GET", "path": "/xrpc/app.bsky.actor.getProfile", "body": PROFILE},
                {"method": "GET", "path": "/xrpc/app.bsky.feed.getPosts", "body": {"posts": [{"$type": "app.bsky.feed.defs#postView", "uri": "at://did:plc:test/app.bsky.feed.post/3kother", "cid": "bafyother", "author": {"did": "did:plc:other", "handle": "other.bsky.social"}, "record": {"$type": "app.bsky.feed.post", "text": "対象", "createdAt": "2026-10-07T00:00:00.000Z"}, "indexedAt": "2026-10-07T00:00:01.000Z"}]}},
                {"method": "POST", "path": "/xrpc/com.atproto.repo.createRecord", "body": {"uri": "at://did:plc:test/app.bsky.feed.repost/3krepost", "cid": "bafyrepost"}},
            ],
        },
        {
            "name": "delete_repost",
            "session_string": session_string,
            "operation": {"type": "delete_repost", "post_id": "at://did:plc:test/app.bsky.feed.post/3kother"},
            "responses": [
                {"method": "GET", "path": "/xrpc/app.bsky.actor.getProfile", "body": PROFILE},
                {"method": "GET", "path": "/xrpc/app.bsky.feed.getPosts", "body": {"posts": [{"$type": "app.bsky.feed.defs#postView", "uri": "at://did:plc:test/app.bsky.feed.post/3kother", "cid": "bafyother", "author": {"did": "did:plc:other", "handle": "other.bsky.social"}, "record": {"$type": "app.bsky.feed.post", "text": "対象", "createdAt": "2026-10-07T00:00:00.000Z"}, "indexedAt": "2026-10-07T00:00:01.000Z", "viewer": {"repost": "at://did:plc:test/app.bsky.feed.repost/3krepost"}}]}},
                {"method": "POST", "path": "/xrpc/com.atproto.repo.deleteRecord", "body": {}},
            ],
        },
        {
            "name": "delete_repost_missing_viewer",
            "session_string": session_string,
            "operation": {"type": "delete_repost", "post_id": "at://did:plc:test/app.bsky.feed.post/3kother"},
            "responses": [
                {"method": "GET", "path": "/xrpc/app.bsky.actor.getProfile", "body": PROFILE},
                {"method": "GET", "path": "/xrpc/app.bsky.feed.getPosts", "body": {"posts": [{"$type": "app.bsky.feed.defs#postView", "uri": "at://did:plc:test/app.bsky.feed.post/3kother", "cid": "bafyother", "author": {"did": "did:plc:other", "handle": "other.bsky.social"}, "record": {"$type": "app.bsky.feed.post", "text": "対象", "createdAt": "2026-10-07T00:00:00.000Z"}, "indexedAt": "2026-10-07T00:00:01.000Z", "viewer": {}}]}},
            ],
        },
        {
            "name": "favorite_and_unfavorite",
            "session_string": session_string,
            "operation": {"type": "favorite_post", "post_id": "at://did:plc:test/app.bsky.feed.post/3kother"},
            "responses": [
                {"method": "GET", "path": "/xrpc/app.bsky.actor.getProfile", "body": PROFILE},
                {"method": "GET", "path": "/xrpc/app.bsky.feed.getPosts", "body": {"posts": [{"$type": "app.bsky.feed.defs#postView", "uri": "at://did:plc:test/app.bsky.feed.post/3kother", "cid": "bafyother", "author": {"did": "did:plc:other", "handle": "other.bsky.social"}, "record": {"$type": "app.bsky.feed.post", "text": "対象", "createdAt": "2026-10-07T00:00:00.000Z"}, "indexedAt": "2026-10-07T00:00:01.000Z", "viewer": {"like": "at://did:plc:test/app.bsky.feed.like/3klike"}}]}},
                {"method": "POST", "path": "/xrpc/com.atproto.repo.createRecord", "body": {"uri": "at://did:plc:test/app.bsky.feed.like/3klike", "cid": "bafylike"}},
            ],
        },
        {
            "name": "repost_invalid_uri",
            "session_string": session_string,
            "operation": {"type": "create_repost", "post_id": "https://example.com/not-at-uri"},
            "responses": [
                {"method": "GET", "path": "/xrpc/app.bsky.actor.getProfile", "body": PROFILE},
            ],
        },
    ]


def make_test_png_bytes() -> bytes:
    """2MB 未満の小さな PNG 画像を生成する (Pillow 使用) 。"""
    from PIL import Image

    buffer = io.BytesIO()
    Image.new("RGB", (64, 48), (200, 30, 30)).save(buffer, format="PNG")
    return buffer.getvalue()


# ---------------------------------------------------------------------------
# 実際の atproto SDK をフェイク HTTP で駆動する
# ---------------------------------------------------------------------------


class FakeResponseRecorder:
    def __init__(self, spec):
        self.spec = spec
        self.requests = []
        self.responses = list(spec["responses"])
        self.saved_sessions = []

    def handle(self, method: str, path: str, query: str, content_type: str, authorization: str | None, body: bytes):
        import httpx

        self.requests.append(
            {
                "method": method,
                "path": path,
                "query": query,
                "content_type": content_type,
                "authorization": authorization,
                "body_sha256": hashlib.sha256(body).hexdigest(),
                "body": _decode_body(content_type, body),
            }
        )
        for index, response in enumerate(self.responses):
            if response["method"] == method and response["path"] == path:
                del self.responses[index]
                return httpx.Response(
                    status_code=response.get("status_code", 200),
                    content=json.dumps(response.get("body", {}), ensure_ascii=False).encode("utf-8"),
                    headers={"content-type": "application/json"},
                )
        return httpx.Response(
            status_code=500,
            content=json.dumps({"error": "MockResponseMissing", "message": f"no mock for {method} {path}"}).encode("utf-8"),
            headers={"content-type": "application/json"},
        )


def _decode_body(content_type: str, body: bytes):
    if not body:
        return None
    if "application/json" in content_type:
        try:
            return json.loads(body.decode("utf-8"))
        except ValueError:
            return body.decode("utf-8", "replace")
    if content_type == "*/*":
        return {"content_type": content_type, "sha256": hashlib.sha256(body).hexdigest(), "length": len(body)}
    if "x-www-form-urlencoded" in content_type:
        from urllib.parse import parse_qsl

        return dict(parse_qsl(body.decode("utf-8")))
    return {"content_type": content_type, "length": len(body)}


async def run_client_scenarios(module, scenarios: list[dict]) -> None:
    import httpx
    from fastapi import UploadFile

    for spec in scenarios:
        recorder = FakeResponseRecorder(spec)

        def handler(request: httpx.Request, _recorder=recorder) -> httpx.Response:
            return _recorder.handle(
                request.method,
                request.url.path,
                request.url.query.decode("utf-8"),
                request.headers.get("content-type", ""),
                request.headers.get("authorization"),
                request.content,
            )

        real_async_client = httpx.AsyncClient

        def make_async_client(*args, _handler=handler, _real=real_async_client, **kwargs):
            kwargs["transport"] = httpx.MockTransport(_handler)
            return _real(*args, **kwargs)

        httpx.AsyncClient = make_async_client

        module.BlueskyAPI._BlueskyAPI__instances.clear()
        module.BlueskyAccount._save_sink = recorder.saved_sessions.append

        account = module.BlueskyAccount(id=1, user_id=42, session_string=spec["session_string"])
        api = module.BlueskyAPI(account)

        operation = spec["operation"]
        if operation["type"] == "home_latest_timeline":
            result = await api.homeLatestTimeline(cursor_id=operation.get("cursor_id"))
        elif operation["type"] == "search_timeline":
            result = await api.searchTimeline(operation["query"], cursor_id=operation.get("cursor_id"))
        elif operation["type"] == "create_post":
            images = []
            if operation.get("with_image") is True:
                image_spec = spec["image"]
                upload = UploadFile(
                    file=io.BytesIO(base64.b64decode(image_spec["data"])),
                    filename=image_spec["name"],
                )
                upload.headers = {"content-type": image_spec["content_type"]}
                images = [upload]
            elif operation.get("with_image_count") is not None:
                images = [
                    UploadFile(file=io.BytesIO(make_test_png_bytes()), filename="i.png")
                    for _ in range(operation["with_image_count"])
                ]
            reply_to = operation.get("reply_to")
            result = await api.createPost(operation["text"], images, reply_to)
        elif operation["type"] == "create_repost":
            result = await api.createRepost(operation["post_id"])
        elif operation["type"] == "delete_repost":
            result = await api.deleteRepost(operation["post_id"])
        elif operation["type"] == "favorite_post":
            result = await api.favoritePost(operation["post_id"])
        else:
            raise AssertionError(f"unknown operation: {operation['type']}")

        spec["result"] = _serialize_result(result)
        spec["requests"] = recorder.requests
        spec["saved_session_strings"] = recorder.saved_sessions

        await module.BlueskyAPI.removeInstance(1)
        httpx.AsyncClient = real_async_client


def _serialize_result(result) -> dict:
    if hasattr(result, "model_dump_json"):
        return _normalize_json(json.loads(result.model_dump_json(exclude_none=False, by_alias=True)))
    raise AssertionError(f"unexpected result type: {type(result)!r}")


def _normalize_json(value):
    if isinstance(value, dict):
        return {key: _normalize_json(child) for key, child in value.items() if child is not None or key in {"tweet_id", "post_uri", "post_cid", "movie_url", "retweeted_tweet", "quoted_tweet", "newer_cursor_id", "image_urls"}}
    if isinstance(value, list):
        return [_normalize_json(child) for child in value]
    return value


# ---------------------------------------------------------------------------


def main() -> None:
    os.environ.setdefault("PYTHONUTF8", "1")
    module = load_bluesky_api_module()

    fixture = {
        "note": "server/app/utils/BlueskyAPI.py (Python 版) から生成した期待値",
        "handle_cases": build_handle_cases(module),
        "datetime_cases": [
            {"input": case["input"], "output": module.BlueskyAPI._parseDateTime(case["input"]).isoformat(" ")}
            for case in build_datetime_cases()
        ],
        "record_key_cases": [
            {"input": uri, "output": module.BlueskyAPI._extractRecordKey(uri)}
            for uri in [
                "at://did:plc:test/app.bsky.feed.post/3kabc",
                "at://did:plc:test/app.bsky.feed.post/3k/a/b",
                "no-slash",
            ]
        ],
        "tag_cases": build_tag_cases(module),
        "facet_expand_cases": build_facet_expand_cases(module),
        "post_views": build_post_views(),
        "client_scenarios": build_client_scenarios(),
    }

    asyncio.run(run_client_scenarios(module, fixture["client_scenarios"]))

    fixture["post_view_results"] = []
    from atproto_client.models.utils import get_or_create
    from atproto import models

    # PostView を SDK でパースしてから _formatPostView() に通す
    post_view_api = module.BlueskyAPI(module.BlueskyAccount(id=999, user_id=42, handle="test.bsky.social"))
    for view in fixture["post_views"]:
        # Python 版は SDK のレスポンスモデル化直前に未知 Union を正規化する
        # (atproto SDK は未知の embed / reason で ValidationError を送出するため、
        #  BlueSky サーバーが将来追加した embed を落とさないよう前処理している)
        # 実際の SDK 経路 (get_response_model) と同じく、レスポンス全体を正規化してから
        # FeedViewPost として model_validate する (reason も SDK の Union として解決される)
        raw_feed_view = {"post": view["post"]}
        if "reason" in view:
            raw_feed_view["reason"] = view["reason"]
        normalized_feed_view = json.loads(json.dumps(raw_feed_view, ensure_ascii=False))
        module._NormalizeUnknownSchemaForSDK(normalized_feed_view)
        parsed_feed_view = models.AppBskyFeedDefs.FeedViewPost.model_validate(normalized_feed_view)
        parsed_post = parsed_feed_view.post
        reason = parsed_feed_view.reason
        tweet = post_view_api._formatPostView(parsed_post, reason)
        fixture["post_view_results"].append(
            {
                "name": view["name"],
                "tweet": _normalize_json(json.loads(tweet.model_dump_json(by_alias=True))),
            }
        )

    # facet 展開の期待値
    for case in fixture["facet_expand_cases"]:
        facets = None
        if case["facets"]:
            facets = [models.AppBskyRichtextFacet.Main.model_validate(f) for f in case["facets"]]
        case["output"] = module.BlueskyAPI._expandFacetLinksInText(case["text"], facets)

    OUTPUT_PATH.write_text(json.dumps(fixture, ensure_ascii=False, indent=1, sort_keys=False), encoding="utf-8")
    print(f"wrote {OUTPUT_PATH}")
    print(f"client scenarios: {len(fixture['client_scenarios'])}, post views: {len(fixture['post_view_results'])}")


if __name__ == "__main__":
    main()
