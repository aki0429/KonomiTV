"""HWEncC (QSVEncC・NVEncC・VCEEncC・rkmppenc) のオプションの対応状況を調べるためのユーティリティ。"""

from __future__ import annotations

import logging
import subprocess

from app.constants import LIBRARY_PATH


# エンコーダーの --help の出力のキャッシュ
## プロセスが生きている間はエンコーダーの対応オプションが変わることはないため、一度だけ取得する
_help_cache: dict[str, str] = {}


def IsHWEncCOptionAvailable(encoder_type: str, option: str) -> bool:
    """
    指定された HWEncC がコマンドラインオプションに対応しているかどうかを返す。

    エンコーダーのバージョンによって利用できるオプションは異なり、未対応のオプションを指定すると
    エンコーダーが即座に終了してエンコードが失敗する
    (例: QSVEncC 8.16 には --adapt-resolution が存在しない) 。
    そのため --help の出力を調べ、対応している場合のみオプションを付与する。

    Args:
        encoder_type (str): エンコーダーの種類 (QSVEncC / NVEncC / VCEEncC / rkmppenc)
        option (str): 調べるオプション名 (例: '--adapt-resolution')

    Returns:
        bool: オプションに対応しているかどうか
    """

    # --help の出力を取得する (2回目以降はキャッシュを利用する)
    if encoder_type not in _help_cache:
        try:
            result = subprocess.run(
                [LIBRARY_PATH[encoder_type], '--help'],
                stdout = subprocess.PIPE,
                stderr = subprocess.STDOUT,
                timeout = 15,
            )
            _help_cache[encoder_type] = result.stdout.decode('utf-8', errors = 'ignore')
        except (OSError, subprocess.SubprocessError) as ex:
            # --help の取得に失敗した場合は、安全側に倒してオプションを付与しない
            logging.warning(f'Failed to check the available options of {encoder_type}: {ex}')
            _help_cache[encoder_type] = ''

    return option in _help_cache[encoder_type]
