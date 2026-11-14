# Go版サービスの導入・更新

`konomitv-service`は、準備済みのGo版配布物をLinuxの現在ユーザーのPM2、またはWindowsサービスへ登録する。
Go本体とAkebiを子プロセスとして管理し、再起動APIでは`config.yaml`を読み直す。
実行時のPythonは不要。既存のHTTPSを別管理する構成では`-external-https`を指定する。

## 配布物の準備

配布ルートには以下を用意する。`config.yaml`は`config.example.yaml`から作成し、実際のバックエンド・保存先を設定する。

| 場所 | 内容 |
| --- | --- |
| `kgo` / `kgo.exe` | Goバックエンド |
| `config.yaml` | このインストールの設定 |
| `client/dist/` | Gitに含まれるビルド済み画面 |
| `server/static/` | Gitに含まれるロゴ・設定資料等 |
| `server/thirdparty/` | 本家の同梱FFmpeg・エンコーダー・tsreadex・Akebi等 |
| `server/go-native/FFmpeg-Thumbnail/` | Go用共有ライブラリとサムネイルワーカー |
| `server/go-native/Chrome/` | システムのChrome / Braveを使わない場合の専用Chrome |

`server/thirdparty/`は本家と同じプラットフォーム用の配布物を配置する。
Go専用のライブラリは[README.md](README.md)の`thumbnail-assets`と`browser-assets`で導入できる。
ビルドにはGo 1.26.2が必要。画面は配布済みの`client/dist/`を使える。

リポジトリの`server-go/`から、Linuxでは次を実行する。

```sh
revision=$(git rev-parse HEAD)
go run ./cmd/thumbnail-assets
CGO_ENABLED=0 go build -trimpath -o ../server/go-native/FFmpeg-Thumbnail/thumbnail-worker.elf ./cmd/thumbnail-worker
CGO_ENABLED=0 go build -trimpath -ldflags "-X main.buildRevision=$revision" -o ../kgo ./cmd/konomitv-go
CGO_ENABLED=0 go build -trimpath -ldflags "-X main.buildRevision=$revision" -o ../konomitv-service ./cmd/konomitv-service
```

WindowsのPowerShellでは次を実行する。

```powershell
$revision = (git rev-parse HEAD).Trim()
go run ./cmd/thumbnail-assets
go build -trimpath -o ../server/go-native/FFmpeg-Thumbnail/thumbnail-worker.exe ./cmd/thumbnail-worker
go build -trimpath -ldflags "-X main.buildRevision=$revision" -o ../kgo.exe ./cmd/konomitv-go
go build -trimpath -ldflags "-X main.buildRevision=$revision" -o ../konomitv-service.exe ./cmd/konomitv-service
```

## サービス登録

LinuxではPM2を使うユーザーで実行する。サービス名はインストールごとに一意にする。

```sh
../konomitv-service install -root /path/to/KonomiTV -name konomitv-go
../konomitv-service status -root /path/to/KonomiTV
```

公開HTTPSは`server.port`、内部Go APIは`127.0.0.77:(server.port+10)`で起動する。
Go本体の設定検査・起動が完了してからAkebiを起動する。
PM2は現在ユーザーの登録と`pm2 save`を利用する。OS起動時のPM2起動には、そのユーザーの既存startup設定を使う。
新しい環境ではPM2のstartup設定も準備する。ツールはsudoや別ユーザーのPM2へ切り替えない。

既存HTTPSを別管理する構成では、現在の内部API待受を明示する。

```sh
../konomitv-service install -root /path/to/KonomiTV -name konomitv-go \
    -external-https -listen 127.0.0.77:7002
```

Windowsの登録・操作にはサービス管理権限が必要。
実行アカウントを`-account`で明示する。そのアカウントにはサービスログオン権限と保存先へのアクセス権が必要。
パスワードはコマンド引数へ直接渡さず、`-password-env`で指定した環境変数から登録時だけ読む。

```powershell
$serviceCredential = Get-Credential
$env:KONOMITV_SERVICE_PASSWORD = $serviceCredential.GetNetworkCredential().Password
try {
    ../konomitv-service.exe install -root C:\KonomiTV -name KonomiTV-Go `
        -account $serviceCredential.UserName -password-env KONOMITV_SERVICE_PASSWORD
} finally {
    Remove-Item Env:KONOMITV_SERVICE_PASSWORD
}
```

WindowsではSCMへ遅延自動起動で登録する。更新時には既存の実行アカウント・パスワード・起動設定を維持する。
LANから利用する場合のAkebiへの受信許可は、利用環境のファイアウォール設定に合わせる。

所有情報は`server/data/native-service.json`へ保存する。
登録先の実行ファイル・引数・作業フォルダが一致したサービスだけを操作する。
同名の別サービスや、所有情報が異なるフォルダは上書きしない。

## Go本体の更新

更新元のソースから新しい実行ファイルを別名で作り、`-replacement`で指定する。
画面・静的ファイルも更新するリリースでは、そのリリースの`client/dist/`・`server/static/`を合わせて配置する。

```sh
../konomitv-service update -root /path/to/KonomiTV -replacement /path/to/kgo-new
```

更新前に`server/backups/native-update-*`へGo本体・設定・認証鍵・サービス所有情報とSQLiteのオンラインバックアップを保存する。
WALのコミット済みデータも含める。サービスを停止して実行ファイルを入れ替え、SHA-256とAPIの起動を確認する。
起動に失敗した場合は旧実行ファイルとサービス構成へ戻す。
DBや設定のバックアップは復旧用に保持し、更新中に追加されたデータを古いDBで自動的に上書きしない。

サービス管理コマンド自身は内容ハッシュ別の`server/go-native/Service/`へ配置する。
新しい管理コマンドから更新すると、その管理コマンドへ登録先も切り替わる。
同じインストールへの同時操作はOSのファイルロックで排他し、異常終了時はロックも解放される。

## 停止・再開・登録削除

```sh
../konomitv-service stop -root /path/to/KonomiTV
../konomitv-service start -root /path/to/KonomiTV
../konomitv-service restart -root /path/to/KonomiTV
../konomitv-service uninstall -root /path/to/KonomiTV
```

`uninstall`は所有するサービス登録を削除する。設定・DB・録画・キャプチャ・アプリのファイルは保持する。
外部管理のHTTPSや別サービスは操作しない。
PM2環境でのアプリ内shutdown APIは、本家同様にPM2の自動再起動が働く。常駐を止めるときは`stop`を使う。

実機で確認した範囲と未確認の環境は[MIGRATION.md](MIGRATION.md)を参照。
