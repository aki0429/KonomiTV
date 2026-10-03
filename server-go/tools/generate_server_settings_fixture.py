"""サーバー設定・クライアント設定 API のパリティ検証用フィクスチャを生成する。

server/ で `uv run python ../server-go/tools/generate_server_settings_fixture.py` として実行する。
生成先: server-go/internal/config/server_settings_defaults.json
          server-go/internal/config/client_settings_defaults.json

- defaults: Python 版 ServerSettings のデフォルト値 (model_dump(mode='json')) 。
  Go 版は config.yaml に存在しない設定値をこの値で補完する。
- loaded: リポジトリの config.yaml を Python 版 LoadConfig() で読み込んだ結果 (バリデーションはスキップ) 。
  Go 版の config.yaml + デフォルト値のマージ結果がこれと一致することをテストで検証する。
- client_settings_defaults.json: Python 版 ClientSettings のデフォルト値。
  Go 版は PUT /api/settings/client で不足している項目をこの値で補完する (Pydantic と同じ挙動) 。
"""

import json
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT / 'server'))

from app.config import ClientSettings, Config, LoadConfig, ServerSettings  # noqa: E402


def main() -> None:
    defaults = ServerSettings().model_dump(mode='json')
    loaded = LoadConfig(bypass_validation=True)
    assert Config() is not None

    output_path = REPO_ROOT / 'server-go' / 'internal' / 'config' / 'server_settings_defaults.json'
    with open(output_path, mode='w', encoding='utf-8', newline='\n') as file:
        json.dump(
            {
                'defaults': defaults,
                'loaded': loaded.model_dump(mode='json'),
                # Go 版の JSON 出力 (MarshalServerSettings) がバイト単位で一致することを検証するための期待値
                'loaded_json': loaded.model_dump_json(),
            },
            file, ensure_ascii=False, indent=2,
        )
        file.write('\n')
    print(f'Wrote {output_path}')

    # クライアント設定のデフォルト値
    client_defaults = ClientSettings().model_dump(mode='json')
    output_path = REPO_ROOT / 'server-go' / 'internal' / 'config' / 'client_settings_defaults.json'
    with open(output_path, mode='w', encoding='utf-8', newline='\n') as file:
        json.dump(client_defaults, file, ensure_ascii=False, indent=2)
        file.write('\n')
    print(f'Wrote {output_path} ({len(client_defaults)} keys)')

    # クライアント設定のシリアライズの期待値 (Tortoise の JSONField と同じコンパクトな JSON)
    client_settings_sample = {
        'last_synced_at': 1700000000.0,
        'caption_font': 'テストフォント',
        'mylist': [{'id': 'test', 'title': 'テスト'}],
        'timetable_channel_width': 'Wide',
        'caption_opacity': 0.5,
        'muted_comment_keywords': [{'keyword': 'テスト'}],
        'unknown_key': True,  # Pydantic では無視される
    }
    output_path = REPO_ROOT / 'server-go' / 'internal' / 'config' / 'client_settings_sample.json'
    with open(output_path, mode='w', encoding='utf-8', newline='\n') as file:
        json.dump(
            {
                'input': client_settings_sample,
                'expected': json.dumps(
                    ClientSettings.model_validate(client_settings_sample).model_dump(mode='json'),
                    ensure_ascii=False, separators=(',', ':'),
                ),
            },
            file, ensure_ascii=False, indent=2,
        )
        file.write('\n')
    print(f'Wrote {output_path}')


if __name__ == '__main__':
    main()
