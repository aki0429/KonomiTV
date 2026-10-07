"""TwitterRouter / TwitterGraphQLAPI の Go 移植用のゴールデン期待値ジェネレーター。

Python 版サーバー (server/app) の実物のコードをスタブ越しに読み込み、
GraphQL リクエストの組み立て (variables / additional_flags / JS スニペット) と
レスポンス解析の期待値を JSON として internal/twitter/testdata/ に書き出す。

外部ネットワーク通信は一切行わない (ブラウザ/バックエンドはすべてフェイクに差し替える) 。
Cookie やトークンの実値は含めず、ダミー値のみを使う。

    python <this file>            # server-go/internal/twitter/testdata に書き出す
"""
from __future__ import annotations

import asyncio
import json
import os
import sys
import types
from datetime import datetime
from unittest.mock import MagicMock

HERE = os.path.dirname(os.path.abspath(__file__))
# internal/twitter/testdata -> internal/twitter -> internal -> server-go -> リポジトリルート -> server
REPO_ROOT = os.path.abspath(os.path.join(HERE, "..", "..", "..", ".."))
SERVER_DIR = os.path.join(REPO_ROOT, "server")
sys.path.insert(0, SERVER_DIR)

# ---------------------------------------------------------------------------
# 依存モジュールのスタブ (tortoise / zendriver / jose / passlib などは未導入環境でも動くようにする)
# ---------------------------------------------------------------------------
from pydantic import BaseModel  # noqa: E402


def _mod(name, **attrs):
    module = types.ModuleType(name)
    module.__dict__.update(attrs)
    sys.modules[name] = module
    return module


class _TortoiseModel:
    class Meta:
        pass

    def __init__(self, **kwargs):
        for key, value in kwargs.items():
            setattr(self, key, value)


class _FieldStub:
    def __class_getitem__(cls, item):
        return cls

    def __getitem__(self, item):
        return self

    def __getattr__(self, name):
        return lambda *args, **kwargs: None


_mod("tortoise", fields=_FieldStub(), __path__=[])
sys.modules["tortoise.fields"] = _mod("tortoise.fields", Field=_FieldStub())
sys.modules["tortoise.models"] = _mod("tortoise.models", Model=_TortoiseModel)
sys.modules["tortoise.contrib"] = _mod("tortoise.contrib", __path__=[])
sys.modules["tortoise.contrib.pydantic"] = _mod(
    "tortoise.contrib.pydantic", PydanticModel=BaseModel, pydantic_model_creator=lambda *a, **k: BaseModel,
)
for _name in ("zendriver", "zendriver.cdp", "zendriver.core", "jose", "passlib", "passlib.context"):
    if _name not in sys.modules:
        sys.modules[_name] = MagicMock()

# 実物が壊れている場合でも app.* は読み込ませる
import importlib.abc  # noqa: E402
import importlib.machinery  # noqa: E402
import importlib.util  # noqa: E402


class _FallbackFinder(importlib.abc.MetaPathFinder, importlib.abc.Loader):
    """実物が存在するモジュールは実物を優先し、未導入モジュールだけを MagicMock で代替する。"""

    def find_spec(self, name, path=None, target=None):
        top_level = name.split(".")[0]
        if top_level in ("app", "ruamel", "_ruamel_yaml"):
            return None
        # 実物が存在する場合はそれを優先する (PathFinder を直接使って再帰を避ける)
        try:
            if importlib.machinery.PathFinder.find_spec(name, path) is not None:
                return None
        except (ImportError, ValueError):
            pass
        # 標準ライブラリのモジュールも実物を優先する
        if top_level in sys.stdlib_module_names:
            return None
        return importlib.machinery.ModuleSpec(name, self, is_package=True)

    def create_module(self, spec):
        module = MagicMock()
        module.__name__ = spec.name
        module.__path__ = []
        module.__spec__ = spec
        module.__loader__ = self
        return module

    def exec_module(self, module):
        pass


sys.meta_path.append(_FallbackFinder())

import importlib.metadata as _metadata  # noqa: E402

_original_version = _metadata.version


def _version(name):
    try:
        return _original_version(name)
    except Exception:
        return "2.0.0"


_metadata.version = _version

from app.utils.TwitterGraphQLAPI import TwitterGraphQLAPI  # noqa: E402
from app.utils.TwitterScrapeBrowser import TwitterScrapeBrowser  # noqa: E402

# ログ出力は app.config の初期化に依存するため、テストでは無効化する
import app.logging as _app_logging  # noqa: E402

_app_logging.info = lambda *args, **kwargs: None
_app_logging.debug = lambda *args, **kwargs: None
_app_logging.warning = lambda *args, **kwargs: None
_app_logging.error = lambda *args, **kwargs: None

# ---------------------------------------------------------------------------
# フェイク (ブラウザ / アカウント)
# ---------------------------------------------------------------------------


class FakeGraphQLResult:
    def __init__(self, parsed_response=None, status_code=None, response_text=None, headers=None, request_error=None):
        self.parsed_response = parsed_response
        self.status_code = status_code
        self.response_text = response_text
        self.headers = headers
        self.request_error = request_error


class FakeComposeResult:
    def __init__(self, is_success=True, error_message=None, compose_submitted_at=None, graphql_api_result=None):
        self.is_success = is_success
        self.error_message = error_message
        self.compose_submitted_at = compose_submitted_at
        self.graphql_api_result = graphql_api_result


class FakeBrowser:
    """TwitterScrapeBrowser のフェイク。"""

    def __init__(self, invokes=None, compose=None):
        self.twitter_account = None
        self.is_setup_complete = True
        self.invokes = list(invokes or [])
        self.compose = list(compose or [])
        self.calls = []  # [endpoint, variables, additional_flags]
        self.compose_calls = []
        self.events = []
        self.setup_calls = 0
        self.shutdown_calls = 0
        self.cookies_txt = ""

    def isBrowserProcessAlive(self):
        return True

    async def setup(self):
        self.setup_calls += 1
        self.events.append("setup")
        self.is_setup_complete = True

    async def shutdown(self):
        self.shutdown_calls += 1
        self.events.append("shutdown")
        self.is_setup_complete = False

    async def invokeGraphQLAPI(self, endpoint_name, variables, additional_flags=None):
        self.calls.append([endpoint_name, variables, additional_flags])
        if not self.invokes:
            raise AssertionError(f"no fake response for {endpoint_name}")
        response = self.invokes.pop(0)
        if isinstance(response, Exception):
            raise response
        return response

    async def postTweetViaComposeUI(self, tweet_text, images, throttle_remaining_seconds, in_reply_to_status_id=None):
        self.compose_calls.append({
            "tweet_text": tweet_text,
            "images": [[getattr(image, "filename", None), getattr(image, "content_type", None)] for image in images],
            "throttle_remaining_seconds": round(throttle_remaining_seconds, 3),
            "in_reply_to_status_id": in_reply_to_status_id,
        })
        if not self.compose:
            raise AssertionError("no fake compose result")
        result = self.compose.pop(0)
        if isinstance(result, Exception):
            raise result
        return result

    async def saveTwitterCookiesToNetscapeFormat(self):
        return self.cookies_txt

    async def captureDebugScreenshot(self, reason):
        self.events.append(f"screenshot:{reason}")
        return None


class FakeAccount:
    def __init__(self, screen_name="dummy_user", account_id=None):
        self.id = account_id
        self.user_id = 1
        self.screen_name = screen_name
        self.name = "Dummy"
        self.icon_url = "https://example.com/icon.png"
        self.access_token = "NETSCAPE_COOKIE_FILE"
        self.access_token_secret = "DUMMY_COOKIE_CONTENT"
        self.save_calls = 0

    def encryptAccessTokenSecret(self, plain_text):
        return "enc:DUMMY" if plain_text else ""

    async def save(self):
        self.save_calls += 1


# ---------------------------------------------------------------------------
# Twitter Web App 風レスポンスの生成
# ---------------------------------------------------------------------------


def make_user(result_id, name, screen_name):
    return {
        "rest_id": result_id,
        "core": {"name": name, "screen_name": screen_name},
        "avatar": {"image_url": f"https://pbs.twimg.com/profile_images/{result_id}/avatar_normal.jpg"},
    }


def make_tweet(
    tweet_id,
    created_at="Wed Oct 07 12:00:00 +0000 2026",
    text="こんにちは、世界！ https://t.co/abc123",
    legacy_extra=None,
    source="Twitter Web App",
):
    legacy = {
        "id_str": tweet_id,
        "created_at": created_at,
        "full_text": text,
        "lang": "ja",
        "retweet_count": 4,
        "favorite_count": 8,
        "retweeted": False,
        "favorited": True,
        "entities": {
            "urls": [{"url": "https://t.co/abc123", "expanded_url": "https://example.com/page"}],
        },
    }
    if legacy_extra:
        legacy.update(legacy_extra)
    return {
        "__typename": "Tweet",
        "rest_id": tweet_id,
        "source": source,
        "legacy": legacy,
        "core": {"user_results": {"result": make_user("111", "テストユーザー", "test_user")}},
    }


def timeline_cursor_entry(entry_id, cursor_type, value):
    return {
        "entryId": entry_id,
        "content": {"entryType": "TimelineTimelineCursor", "cursorType": cursor_type, "value": value},
    }


def timeline_tweet_entry(entry_id, tweet):
    return {
        "entryId": entry_id,
        "content": {"entryType": "TimelineTimelineItem", "itemContent": {"itemType": "TimelineTweet", "tweet_results": {"result": tweet}}},
    }


def build_home_timeline_response():
    photo_media = {
        "type": "photo",
        "media_url_https": "https://pbs.twimg.com/media/photo1.jpg",
    }
    video_media = {
        "type": "video",
        "media_url_https": "https://pbs.twimg.com/media/video_thumb.jpg",
        "video_info": {"variants": [
            {"content_type": "video/mp4", "bitrate": 832000, "url": "https://video.twimg.com/low.mp4"},
            {"content_type": "video/mp4", "bitrate": 2176000, "url": "https://video.twimg.com/high.mp4"},
            {"content_type": "application/x-mpegURL", "url": "https://video.twimg.com/playlist.m3u8"},
        ]},
    }
    plain_tweet = make_tweet(
        "1001",
        legacy_extra={"extended_entities": {"media": [photo_media, video_media]}},
    )
    retweeted_inner = make_tweet("1002", text="リツイート元の本文")
    retweet_tweet = make_tweet("1003", text="RT @someone: リツイート元の本文")
    retweet_tweet["legacy"]["retweeted_status_result"] = {"result": retweeted_inner}
    quoted_inner = make_tweet("1004", text="引用元の本文")
    quoted_tweet = make_tweet("1005", text="引用ツイートの本文")
    quoted_tweet["quoted_status_result"] = {"result": quoted_inner}
    quoted_missing = make_tweet("1006", text="引用元が空のツイート")
    quoted_missing["quoted_status_result"] = {}
    visibility_tweet = {
        "__typename": "TweetWithVisibilityResults",
        "tweet": make_tweet("1007", text="可視性制限付きツイート"),
    }
    promoted = timeline_tweet_entry("promoted-0", make_tweet("1008", text="広告ツイート"))
    conversation_module = {
        "entryId": "conversation-0",
        "content": {
            "entryType": "TimelineTimelineModule",
            "displayType": "VerticalConversation",
            "items": [
                {"item": {"itemContent": {"itemType": "TimelineTweet", "tweet_results": {"result": make_tweet("1010", text="スレッドの古い投稿", created_at="Wed Oct 07 09:00:00 +0000 2026")}}}},
                {"item": {"itemContent": {"itemType": "TimelineTweet", "tweet_results": {"result": make_tweet("1011", text="スレッドの新しい投稿", created_at="Wed Oct 07 15:00:00 +0000 2026")}}}},
            ],
        },
    }
    return {
        "home": {
            "home_timeline_urt": {
                "instructions": [
                    {"type": "TimelineAddEntries", "entries": [
                        timeline_cursor_entry("cursor-top-0", "Top", "CURSOR_TO_TOP"),
                        promoted,
                        timeline_tweet_entry("tweet-1001", plain_tweet),
                        timeline_tweet_entry("tweet-1003", retweet_tweet),
                        timeline_tweet_entry("tweet-1005", quoted_tweet),
                        timeline_tweet_entry("tweet-1006", quoted_missing),
                        timeline_tweet_entry("tweet-1007", visibility_tweet),
                        conversation_module,
                        timeline_cursor_entry("cursor-showmore", "ShowMore", "CURSOR_SHOWMORE"),
                        timeline_cursor_entry("cursor-gap", "Gap", "CURSOR_GAP"),
                        timeline_cursor_entry("cursor-bottom-0", "Bottom", "CURSOR_TO_BOTTOM"),
                        timeline_cursor_entry("cursor-threads", "ShowMoreThreads", "CURSOR_THREADS"),
                    ]},
                    {"type": "TimelineReplaceEntry", "entry": timeline_tweet_entry("tweet-1009", make_tweet("1009", text="置換されたツイート"))},
                ],
            },
        },
    }


def build_search_timeline_response():
    return {
        "search_by_raw_query": {
            "search_timeline": {
                "timeline": {
                    "instructions": [
                        {"type": "TimelineAddEntries", "entries": [
                            timeline_tweet_entry("tweet-2001", make_tweet("2001", text="検索結果のツイート")),
                            timeline_cursor_entry("cursor-bottom-1", "Bottom", "SEARCH_CURSOR"),
                        ]},
                    ],
                },
            },
        },
    }


# ---------------------------------------------------------------------------
# シナリオ定義
# ---------------------------------------------------------------------------

VIEWER_RESPONSE = {"viewer": {"user_results": {"result": {
    "rest_id": "42",
    "core": {"name": "テスト太郎", "screen_name": "test_taro"},
    "avatar": {"image_url": "https://pbs.twimg.com/profile_images/42/avatar_normal.jpg"},
}}}}

VIEWER_MISSING_RESULT = {"viewer": {"user_results": {}}}
VIEWER_MISSING_NAME = {"viewer": {"user_results": {"result": {
    "rest_id": "42", "core": {"screen_name": "test_taro"}, "avatar": {"image_url": "https://example.com/a_normal.jpg"},
}}}}

HOME_RESPONSE = build_home_timeline_response()
SEARCH_RESPONSE = build_search_timeline_response()

QUERY_SCENARIOS = [
    ("viewer_success", VIEWER_RESPONSE, "fetchLoggedViewer", {}),
    ("viewer_missing_result", VIEWER_MISSING_RESULT, "fetchLoggedViewer", {}),
    ("viewer_missing_name", VIEWER_MISSING_NAME, "fetchLoggedViewer", {}),
    ("retweet_success", {"create_retweet": {"retweet_results": {}}}, "createRetweet", {"tweet_id": "1850000000000000001"}),
    ("delete_retweet_success", {"unretweet": {"source_tweet_results": {}}}, "deleteRetweet", {"tweet_id": "1850000000000000002"}),
    ("favorite_success", {"favorite_tweet": "Done"}, "favoriteTweet", {"tweet_id": "1850000000000000003"}),
    ("unfavorite_success", {"unfavorite_tweet": "Done"}, "unfavoriteTweet", {"tweet_id": "1850000000000000004"}),
    ("home_no_cursor", HOME_RESPONSE, "homeLatestTimeline", {}),
    ("home_top_cursor", HOME_RESPONSE, "homeLatestTimeline", {"cursor_id": "CURSOR_TO_TOP", "cursor_type": "Top", "seen_tweet_ids": ["1", "2", "3"]}),
    ("home_bottom_cursor", HOME_RESPONSE, "homeLatestTimeline", {"cursor_id": "CURSOR_TO_BOTTOM", "cursor_type": "Bottom"}),
    ("home_gap_cursor", HOME_RESPONSE, "homeLatestTimeline", {"cursor_id": "CURSOR_GAP", "cursor_type": "Gap"}),
    ("home_showmore_cursor", HOME_RESPONSE, "homeLatestTimeline", {"cursor_id": "CURSOR_SHOWMORE", "cursor_type": "ShowMore"}),
    ("search_latest", SEARCH_RESPONSE, "searchTimeline", {"search_type": "Latest", "query": "  新型車 "}),
    ("search_top", SEARCH_RESPONSE, "searchTimeline", {"search_type": "Top", "query": "新型車"}),
    ("search_bottom_cursor", SEARCH_RESPONSE, "searchTimeline", {"search_type": "Latest", "query": "新型車", "cursor_id": "SEARCH_CURSOR", "cursor_type": "Bottom"}),
]

# invokeGraphQLAPI レベルのエラー分岐
INVOKE_SCENARIOS = [
    ("non_json_response", "fetchLoggedViewer", {}, {"status_code": 200, "headers": {"content-type": "text/html; charset=utf-8"}, "response_text": "<html>ng</html>"}, None),
    ("http_error_with_json", "fetchLoggedViewer", {}, {"status_code": 500, "headers": {"content-type": "text/plain"}, "response_text": "Internal Server Error"}, None),
    ("json_parse_failure", "fetchLoggedViewer", {}, {"status_code": 200, "headers": {"content-type": "application/json"}, "response_text": "{not json"}, None),
    ("errors_only", "createRetweet", {"tweet_id": "1"}, {"status_code": 200, "headers": {"content-type": "application/json"}, "response_text": json.dumps({"errors": [{"code": 88, "message": "Rate limit exceeded"}]})}, None),
    ("errors_unknown_code", "createRetweet", {"tweet_id": "1"}, {"status_code": 200, "headers": {"content-type": "application/json"}, "response_text": json.dumps({"errors": [{"code": 9999, "message": "Unknown"}]})}, None),
    ("missing_data_key", "fetchLoggedViewer", {}, {"status_code": 200, "headers": {"content-type": "application/json"}, "response_text": json.dumps({"foo": "bar"})}, None),
    ("request_error", "fetchLoggedViewer", {}, {"status_code": None, "headers": None, "response_text": None, "request_error": "Request failed"}, None),
    ("empty_body_200", "fetchLoggedViewer", {}, {"status_code": 200, "headers": {"content-type": "application/json"}, "response_text": ""}, None),
    ("no_response_at_all", "fetchLoggedViewer", {}, {"status_code": None, "headers": None, "response_text": None}, None),
]

CREATE_TWEET_SCENARIOS = [
    ("tweet_success", "こんにちは", [], None),
    ("tweet_reply_success", "こんにちは", [["a.jpg", "image/jpeg"], ["b.png", "image/png"]], "1850000000000000009"),
    ("tweet_errors_only", "こんにちは", [], None),
    ("tweet_http_error", "こんにちは", [], None),
    ("tweet_empty_results", "こんにちは", [], None),
    ("tweet_not_dict", "こんにちは", [], None),
    ("tweet_missing_rest_id", "こんにちは", [], None),
]

CREATE_TWEET_RESPONSES = {
    "tweet_success": {"parsed_response": {"data": {"create_tweet": {"tweet_results": {"result": {"rest_id": "1850000000000000011"}}}}}, "status_code": 200},
    "tweet_reply_success": {"parsed_response": {"data": {"create_tweet": {"tweet_results": {"result": {"rest_id": "1850000000000000012"}}}}}, "status_code": 200},
    "tweet_errors_only": {"parsed_response": {"errors": [{"code": 186, "message": "Tweet needs to be a bit shorter."}]}, "status_code": 200},
    "tweet_http_error": {"parsed_response": {"errors": [{"code": 185, "message": "User is over daily status update limit."}]}, "status_code": 403},
    "tweet_empty_results": {"parsed_response": {"data": {"create_tweet": {"tweet_results": {}}}}, "status_code": 200},
    "tweet_not_dict": {"parsed_response": ["unexpected"], "status_code": 200},
    "tweet_missing_rest_id": {"parsed_response": {"data": {"create_tweet": {"tweet_results": {"result": {}}}}}, "status_code": 200},
}

# JS スニペット生成の検証用 (TwitterScrapeBrowser.invokeGraphQLAPI が CDP に渡す式)
JS_SNIPPET_SCENARIOS = [
    ("Viewer", {"withCommunitiesMemberships": True}, {"fieldToggles": {"isDelegate": False, "withAuxiliaryUserLabels": True}}),
    ("SearchTimeline", {"rawQuery": "新型車 lang:ja -filter:replies", "count": 20, "querySource": "typed_query", "product": "Latest", "withGrokTranslatedBio": False}, None),
]


# ---------------------------------------------------------------------------
# 実行
# ---------------------------------------------------------------------------


def normalize(value):
    if isinstance(value, datetime):
        return value.isoformat()
    if isinstance(value, BaseModel):
        return normalize(value.model_dump())
    if isinstance(value, dict):
        return {key: normalize(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [normalize(item) for item in value]
    return value


def to_jsonable(value):
    if isinstance(value, BaseModel):
        return normalize(value.model_dump())
    if isinstance(value, (dict, list, tuple, str, int, float, bool)) or value is None:
        return normalize(value)
    return repr(value)


async def run_scenarios():
    output = {"query_scenarios": [], "invoke_scenarios": [], "tweet_scenarios": [], "js_snippets": []}

    for name, response, method_name, kwargs in QUERY_SCENARIOS:
        account = FakeAccount()
        client = TwitterGraphQLAPI(account)
        raw = {"parsed_response": {"data": response}, "status_code": 200, "headers": {"content-type": "application/json"}}
        browser = FakeBrowser(invokes=[FakeGraphQLResult(
            parsed_response=raw["parsed_response"],
            status_code=raw["status_code"],
            headers=raw["headers"],
        )])
        client._browser = browser
        client._last_home_fetched_at = 0.0
        client._last_search_fetched_at = 0.0
        result = await getattr(client, method_name)(**kwargs)
        output["query_scenarios"].append({
            "name": name,
            "method": method_name,
            "kwargs": kwargs,
            "endpoint": browser.calls[0][0],
            "variables": browser.calls[0][1],
            "additional_flags": browser.calls[0][2],
            "raw": raw,
            "result": to_jsonable(result),
        })

    for name, method_name, kwargs, raw, _ in INVOKE_SCENARIOS:
        account = FakeAccount()
        client = TwitterGraphQLAPI(account)
        browser = FakeBrowser(invokes=[FakeGraphQLResult(
            parsed_response=raw.get("parsed_response"),
            status_code=raw.get("status_code"),
            response_text=raw.get("response_text"),
            headers=raw.get("headers"),
            request_error=raw.get("request_error"),
        )])
        client._browser = browser
        result = await getattr(client, method_name)(**kwargs)
        output["invoke_scenarios"].append({
            "name": name,
            "method": method_name,
            "kwargs": kwargs,
            "raw": raw,
            "result": to_jsonable(result),
        })

    for name, tweet, images, reply_to in CREATE_TWEET_SCENARIOS:
        account = FakeAccount()
        client = TwitterGraphQLAPI(account)
        graphql_api_result = FakeGraphQLResult(**CREATE_TWEET_RESPONSES[name])
        browser = FakeBrowser(compose=[FakeComposeResult(
            is_success=True, compose_submitted_at=1759800000.0, graphql_api_result=graphql_api_result,
        )])
        client._browser = browser
        image_objects = [types.SimpleNamespace(filename=item[0], content_type=item[1]) for item in images]
        result = await client.createTweet(tweet, image_objects, reply_to)
        output["tweet_scenarios"].append({
            "name": name,
            "tweet": tweet,
            "images": images,
            "in_reply_to_status_id": reply_to,
            "compose_call": browser.compose_calls[0],
            "raw": CREATE_TWEET_RESPONSES[name],
            "result": to_jsonable(result),
        })

    # TwitterScrapeBrowser.invokeGraphQLAPI が CDP に渡す JS 式の生成
    for endpoint_name, variables, additional_flags in JS_SNIPPET_SCENARIOS:
        browser = TwitterScrapeBrowser.__new__(TwitterScrapeBrowser)
        browser.twitter_account = FakeAccount()
        captured = {}

        class FakeResult:
            value = {"parsedResponse": None, "responseText": None, "statusCode": 200, "headers": None, "requestError": None}

        async def fake_send(request):
            return FakeResult(), None

        class FakePage:
            send = staticmethod(fake_send)

        browser._page = FakePage()
        browser._browser = object()

        import app.utils.TwitterScrapeBrowser as browser_module

        def fake_evaluate(**kwargs):
            captured["js_code"] = kwargs["expression"]
            return kwargs

        browser_module.cdp.runtime.evaluate = fake_evaluate
        await browser.invokeGraphQLAPI(endpoint_name, variables, additional_flags)
        output["js_snippets"].append({
            "endpoint": endpoint_name,
            "variables": variables,
            "additional_flags": additional_flags,
            "js_code": captured["js_code"],
        })

    # Fernet (TwitterAccount の Cookie 暗号化) の相互運用フィクスチャ
    import base64
    import hashlib
    from cryptography.fernet import Fernet

    for secret, plain_text, timestamp, iv_bytes in [
        ("dummy-jwt-secret-for-fixtures", "cookies.txt dummy content", 1759800000, bytes(range(16))),
        ("dummy-jwt-secret-for-fixtures", "", 1759800001, bytes(range(16, 32))),
        ("another-dummy-secret", "x.com	auth_token	dummy", 1759800002, bytes([7]) * 16),
    ]:
        key = base64.urlsafe_b64encode(hashlib.sha256(secret.encode("utf-8")).digest())
        instance = Fernet(key)
        token = instance._encrypt_from_parts(plain_text.encode("utf-8"), timestamp, iv_bytes)
        output.setdefault("fernet", []).append({
            "secret": secret,
            "fernet_key": key.decode("utf-8"),
            "plain_text": plain_text,
            "timestamp": timestamp,
            "iv_base64": base64.urlsafe_b64encode(iv_bytes).decode("utf-8"),
            "token": token.decode("utf-8"),
        })
        # 復号も検証する (Go 側が復号できることの期待値)
        assert instance.decrypt(token).decode("utf-8") == plain_text

    return output


def main():
    output = asyncio.run(run_scenarios())
    target = os.path.join(HERE, "twitter_expected.json")
    with open(target, "w", encoding="utf-8", newline="\n") as file:
        json.dump(output, file, ensure_ascii=False, indent=2)
        file.write("\n")
    print(f"wrote {target}")
    print(json.dumps(output["js_snippets"][0]["js_code"], ensure_ascii=False))


if __name__ == "__main__":
    main()
