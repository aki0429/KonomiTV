
# このフォーク (aki0429/KonomiTV) 向けのインストーラーの設定値
## 本家 (tsukumijima/KonomiTV) のインストーラーをベースに、ソースコードとサードパーティーライブラリの取得元を
## このフォーク向けに差し替えている
## インストールするブランチやサードパーティーライブラリの配布元を変更したい場合は、このファイルだけを編集すればよい

from typing import NamedTuple


# ***** ソースコードの取得元 *****

# インストーラーがソースコードを取得するリポジトリ (このフォーク)
SOURCE_REPOSITORY = 'aki0429/KonomiTV'

# 安定版 (メニューの 1 / 2) としてインストールするブランチ
## このフォークは本家 master をベースに IPTV 対応とキャプチャギャラリーを取り込んだ iptv-master で運用しているため、
## 現状は安定版と開発版が同じブランチを指している
## 特定のコミットを安定版として固定したくなったら、この定数を別ブランチ (例: iptv-0.14.1) に書き換える
SOURCE_STABLE_BRANCH = 'iptv-master'

# 開発版 (メニューの 4 / 5) としてインストールするブランチ
## このブランチの最新コミットがそのままインストールされる
SOURCE_DEVELOPMENT_BRANCH = 'iptv-master'

# インストーラーのヘッダーやコンソールのタイトルに表示するエディション名
INSTALLER_EDITION = 'IPTV Edition'


# ***** サードパーティーライブラリの取得元 *****
# このフォークのソースコード (iptv-master) は本家 master をベースにしており、Python 3.13 と uv を必要とする
# 一方、本家のリリース版 (v0.14.1) 向けにビルドされたサードパーティーライブラリは Python 3.11 + Poetry 構成で、
# 現在のインストーラー (サードパーティーライブラリ内の Python で uv sync する) では使えない
# そのため、本家の最新版インストーラーの「開発版をインストール」と同じく、本家 master 向けにビルドされた
# GitHub Actions のアーティファクト (nightly.link が配布している zip アーカイブ) を取得する
#
# このフォークの Actions で build_thirdparty.yaml を実行できるようにした場合は、THIRDPARTY_REPOSITORY を
# 'aki0429/KonomiTV' に、THIRDPARTY_BRANCH を 'iptv-master' に書き換えることで自前のビルドを使えるようになる

# サードパーティーライブラリをビルドしているリポジトリ (本家)
THIRDPARTY_REPOSITORY = 'tsukumijima/KonomiTV'

# サードパーティーライブラリをビルドする GitHub Actions のワークフローファイル名
THIRDPARTY_WORKFLOW_FILE = 'build_thirdparty.yaml'

# サードパーティーライブラリをビルドしているブランチ (本家 master)
THIRDPARTY_BRANCH = 'master'


class ThirdpartyArchive(NamedTuple):
    """
    サードパーティーライブラリのアーカイブの配布情報を表す。

    Attributes:
        url (str): アーカイブをダウンロードする URL
        file_name (str): アーカイブ自体のファイル名 (例: thirdparty-windows.7z)
        is_zip_wrapped (bool): ダウンロードしたファイルがさらに zip で包まれているかどうか
    """

    url: str
    file_name: str
    is_zip_wrapped: bool


def GetSourceBranch(version: str) -> str:
    """
    インストール/アップデートの種類から、ソースコードを取得するブランチを返す。

    Args:
        version (str): インストール/アップデートの種類 ('latest' なら開発版、それ以外は安定版)

    Returns:
        str: ソースコードを取得するブランチ名
    """

    return SOURCE_DEVELOPMENT_BRANCH if version == 'latest' else SOURCE_STABLE_BRANCH


def BuildGitRepositoryURL() -> str:
    """
    ソースコードを git clone するためのリポジトリの URL を返す。

    Returns:
        str: リポジトリの URL
    """

    return f'https://github.com/{SOURCE_REPOSITORY}.git'


def BuildSourceCodeZipURL(version: str) -> str:
    """
    Git がインストールされていない環境で使う、ソースコードの zip アーカイブの URL を返す。

    Args:
        version (str): インストール/アップデートの種類 ('latest' なら開発版、それ以外は安定版)

    Returns:
        str: ソースコードの zip アーカイブの URL
    """

    return f'https://codeload.github.com/{SOURCE_REPOSITORY}/zip/refs/heads/{GetSourceBranch(version)}'


def GetSourceCodeArchiveDirectoryName(version: str) -> str:
    """
    ソースコードの zip アーカイブを展開したときに作られる、ルートフォルダの名前を返す。

    GitHub が生成する zip アーカイブは、リポジトリ名とブランチ名をハイフンで連結したフォルダ
    (例: KonomiTV-iptv-master) にすべてのファイルを格納しているため、そのフォルダ名を
    インストール先のフォルダへ移動させる必要がある。

    Args:
        version (str): インストール/アップデートの種類 ('latest' なら開発版、それ以外は安定版)

    Returns:
        str: 展開後に作られるルートフォルダの名前
    """

    return f'{SOURCE_REPOSITORY.split("/")[1]}-{GetSourceBranch(version)}'


def GetThirdpartyArchive(platform_type: str, is_arm_device: bool) -> ThirdpartyArchive:
    """
    サードパーティーライブラリのアーカイブの配布情報を返す。

    Args:
        platform_type (str): プラットフォームの種類 ('Windows' または 'Linux')
        is_arm_device (bool): ARM デバイス (aarch64) かどうか

    Returns:
        ThirdpartyArchive: サードパーティーライブラリのアーカイブの配布情報
    """

    # Windows 向けと Linux 向け (x86_64 / ARM) でアーカイブの名前と形式が異なる
    thirdparty_compressed_file_name = 'thirdparty-windows.7z'
    if platform_type == 'Linux' and is_arm_device is False:
        thirdparty_compressed_file_name = 'thirdparty-linux.tar.xz'
    elif platform_type == 'Linux' and is_arm_device is True:
        thirdparty_compressed_file_name = 'thirdparty-linux-arm.tar.xz'

    # 本家 master 向けにビルドされた Actions のアーティファクトは、nightly.link によって zip で包まれた状態で配布されている
    return ThirdpartyArchive(
        url = (
            f'https://nightly.link/{THIRDPARTY_REPOSITORY}/workflows/{THIRDPARTY_WORKFLOW_FILE}/'
            f'{THIRDPARTY_BRANCH}/{thirdparty_compressed_file_name}.zip'
        ),
        file_name = thirdparty_compressed_file_name,
        is_zip_wrapped = True,
    )
