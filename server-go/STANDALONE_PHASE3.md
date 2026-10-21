# Go 単独化・第3チェックポイント

ベース: e412b23afcbd5997eb583bd9688f22ce525affef、作業コピー task-4/KonomiTV-app、branch work/go-standalone-memory。
第2チェックポイント tree `423bc04a169b664aaaf35e24ad2f56e9495a9318` からの追加変更を保存する。
公開・本番適用・実アカウント認証/投稿・本番 EDCB の操作は実施していない。

## 今回の実装

- `internal/twitter/cdp.go`: Go WebSocket CDP 接続、command ID の応答配信、期限/切断、購読解除。pending は64、購読 queue は256、受信は64MiBまで。必要通知があふれたら成功扱いで捨てず接続失敗にする。
- 専用 headless Chrome/Brave を一時 profile と loopback debug port で起動。既存ユーザープロファイルは使わない。Windows Job / Unix process group で所有するプロセスを回収する。`KONOMITV_BROWSER_PATH` によるバイナリ指定可。
- `BrowserBackend`: 採取元 OS/言語/UA-CH、保存 Cookie 読み込み、main.js breakpoint、既存 Python と同じ埋め込み webpack hook、GraphQL 呼び出し、Cookie Netscape 保存。Python ランタイム/アプリ/ソースファイルの読み込みは不要。
- ツイート UI: 通常/返信、paste、画像 file input、FINALIZE 応答、ボタン有効待機、Ctrl+Enter、CreateTweet 応答取得。Preflight を無視し、base64 body を復号。送信結果不明時は自動再送せず送信時刻を保持する。
- API 既定 factory を Go CDP に変更。DB の `enc:` Cookie を API 層で既存 Fernet 互換復号。平文旧形式にも対応。破損時は認証失敗として処理。
- レジストリ破棄/サーバー終了で browser を Shutdown。スクリーンショットは保存先明示時のみ、private ファイルで保存。

## 検証と限界

- 既存 Twitter fixture/GraphQL/API テストに加え、セットアップ、無関係な breakpoint、失敗時回収、通常/返信/画像、結果喪失時の再送なし、購読解除、Cookie filter/復号をモックで検証。
- 実 WebSocket は localhost synthetic peer だけ。command/event/cancel/disconnect/error sanitization を確認。
- 実 Chrome の about:blank CDP と、localhost synthetic main.js の breakpoint→hook→resume→GraphQL 呼び出しを確認。実 Cookie の投入や Twitter 接続は一切行っていない。
- **実 Twitter Web App の動的 module / DOM / 本人認証との互換性は未検証**。埋め込み hook は既存 `server/static/zendriver_setup.js` のコピー。ローカル Chrome テストの hook は合成であり、実 Twitter の互換性を証明しない。
- Python の動的 uBlock filter 取得は追加していない。埋め込み hook の既存固定 analytics patterns を使用し、動的追加リストは空。受入れ判断時にこの差を確認する。
- Windows Chrome 起動/終了は確認。Linux 側の Chrome 実起動は未検証。OS プロセス回収と同時 account 更新/アイドル終了の追加負荷監査は残る。

保存ログ (task-4直下):

- `phase3-all-tests.txt`: `go test ./... -p 1 -count=1 -timeout=120s` 全パッケージ PASS。API 114.372秒。`phase3-vet.txt`: `go vet ./...` 成功、出力なし。
- `phase3-twitter-isolated-tests.txt`: Twitter 全テスト + opt-in about:blank Chrome PASS (7.205秒)。
- `phase3-chromium-tests.txt`: about:blank と localhost main.js 実 CDP 2テスト PASS (5.948秒)。
- `phase3-final-targeted-tests.txt` / `phase3-final-vet.txt`: ShutdownAll と main 最終追加後の該当パッケージ再確認 PASS、vet 成功。
- `phase3-cookie-test.txt`: 保存 Cookie 平文/暗号化/破損の単独テスト PASS。
- Linux amd64 `go test -c ./internal/twitter` 成功。実 Linux Chromium テストは未実施。
- 初回 sandbox 内の CDP localhost テストは socket permission で失敗 (`phase3-twitter-tests.txt`)。許可された隔離実行へ切り替えた上記テストは成功。アプリ不具合として数えていない。

## 115 route の到達点

`API_IMPLEMENTATION_AUDIT.md` に Python 115 route と Go handler の対応を保持。Go literal111登録に加え BML dispatcher と Bluesky action dispatcher が Python 複数 route を扱う。
Twitter browser backend の「未実装」は今回 Go 実装に置換したが、**115全ルートの個別正常系完了・実用互換性完了とはしていない**。

未検証: 実 EDCB/Mirakurun ライブ、チューナー handoff、実予約/検索、GPUエンコード、実 SNS/認証、全ルート同一fixtureの Python 差分監査、大規模 NAS/SMB。
残る実装/監査: 録画常駐監視の drive I/O 上限、外部 symlink alias と mount 再接続時の照合、監視/手動解析の競合、解析互換性。今回は監視の追加変更を入れていない。
通常 API を偽の成功応答に差し替えたり、未対応機能を削除したりしていない。

## メモリ

第2チェックポイントの測定結果を維持。今回、新たな PES/segment 最適化と実再生測定は行っていない。
同一録画/HLS各2回で app平均 PSS は before85.63/85.74MiB、after88.28/85.82MiB。**実再生 PSS の改善は未確認**。
全5segmentの length/SHA は4走行で一致。CPU/遅延の優位も主張しない。
profile 上の主な allocation は PES組立/append/packetize。安全な所有権・同一出力を維持した改善と各2回の再測定は次工程。

## 統合境界

installer との既知衝突は `cmd/konomitv-go/main.go` と `internal/api/server.go`。
今回の追加は main.go に twitter import と ShutdownAll 呼び出し。server.go は第2チェックポイントから追加変更なし。
新規 DB 初期化・インストーラー・IPTV 有無の installer 作業は変更していない。
Git 作者未設定のため commit は作っていない。第2チェックポイントからの incremental patch と累積 patch を別々に保存し、二重適用しない。
