
# Type Hints を指定できるように
# ref: https://stackoverflow.com/a/33533514/17124142
from __future__ import annotations

from typing import TYPE_CHECKING

from tortoise import fields
from tortoise.models import Model as TortoiseModel


if TYPE_CHECKING:
    from app.models.CaptureBookmark import CaptureBookmark
    from app.models.User import User


class CaptureFolder(TortoiseModel):
    """
    キャプチャフォルダを管理するモデル。
    ユーザーが手動で作成する仮想フォルダで、実際のファイル移動は行わない。
    フォルダとキャプチャの紐付けは CaptureBookmark (中間テーブル) を介して管理する。
    1つのキャプチャは複数のフォルダに所属可能 (タグのような概念)。
    """

    # データベース上のテーブル名
    class Meta(TortoiseModel.Meta):
        table: str = 'capture_folders'

    # フォルダ ID (自動採番)
    id = fields.IntField(pk=True)
    # フォルダを所有するユーザー
    ## ユーザー削除時にフォルダも連動して削除する
    user: fields.ForeignKeyRelation[User] = \
        fields.ForeignKeyField('models.User', related_name='capture_folders', on_delete=fields.CASCADE)
    user_id: int
    # フォルダ名 (ユーザーが自由に命名)
    name = fields.TextField()
    # フォルダの表示順序 (小さい値が先頭)
    ## 将来的なドラッグ & ドロップ並べ替えに対応するため
    sort_order = fields.IntField(default=0)
    # このフォルダに紐付けられたブックマーク (逆参照)
    bookmarks: fields.ReverseRelation[CaptureBookmark]
    # 作成日時・更新日時
    created_at = fields.DatetimeField(auto_now_add=True)
    updated_at = fields.DatetimeField(auto_now=True)
