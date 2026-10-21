# API ルート実装台帳 (第2チェックポイント)

Python Router の115 routeとGoの登録先を照合。ルート登録は完全な動作互換性の証明ではない。
個々の正常系・実機の受入れは残作業。Twitter backend未完成を含め、全機能完成とはしない。

| Route | Go 登録先 | 状態・主依存 |
| --- | --- | --- |
| `DELETE /api/bluesky/accounts/{handle}` | `bluesky.go:handleBlueskyAccountDelete` | Go native。外部サービス・必要時の認証。モック検証、実アカウント未操作 |
| `DELETE /api/bluesky/accounts/{handle}/posts/{post_id:path}/like` | `bluesky.go:handleBlueskyPostAction (rest...)` | Go native。外部サービス・必要時の認証。モック検証、実アカウント未操作 |
| `DELETE /api/bluesky/accounts/{handle}/posts/{post_id:path}/repost` | `bluesky.go:handleBlueskyPostAction (rest...)` | Go native。外部サービス・必要時の認証。モック検証、実アカウント未操作 |
| `DELETE /api/captures/{filename}` | `server.go:handleCaptureDelete` | Go native。SQLite・認証・静的asset |
| `DELETE /api/captures/folders/{folder_id}` | `server.go:handleCaptureFolderDelete` | Go native。SQLite・認証・静的asset |
| `DELETE /api/captures/folders/{folder_id}/captures` | `server.go:handleCaptureFolderCaptureRemove` | Go native。SQLite・認証・静的asset |
| `DELETE /api/iptv/sources` | `server.go:handleIPTVSourceDelete` | Go native。M3U/配信URL。Pythonアプリ依存なし |
| `DELETE /api/iptv/tvui` | `server.go:handleIPTVTVUIUnregister` | Go native。M3U/配信URL。Pythonアプリ依存なし |
| `DELETE /api/niconico/logout` | `niconico.go:handleNiconicoLogout` | Go native。外部サービス・必要時の認証。モック検証、実アカウント未操作 |
| `DELETE /api/recording/conditions/{reservation_condition_id}` | `reservation_conditions.go:handleReservationConditionDelete` | Go native。EDCB CtrlCmd。実機予約/検索の受入れ未実施 |
| `DELETE /api/recording/reservations/{reservation_id}` | `reservations.go:handleReservationDelete` | Go native。EDCB CtrlCmd。実機予約/検索の受入れ未実施 |
| `DELETE /api/twitter/accounts/{screen_name}` | `twitter.go:handleTwitterAccountDelete` | Go native。SQLite・認証・静的asset |
| `DELETE /api/twitter/accounts/{screen_name}/tweets/{tweet_id}/favorite` | `twitter.go:handleTwitterFavoriteCancel` | Go native。SQLite・認証・静的asset |
| `DELETE /api/twitter/accounts/{screen_name}/tweets/{tweet_id}/retweet` | `twitter.go:handleTwitterRetweetCancel` | Go native。SQLite・認証・静的asset |
| `DELETE /api/users/{username}` | `server.go:handleSpecifiedUserDelete` | Go native。SQLite・認証・静的asset |
| `DELETE /api/users/me` | `server.go:handleUserDelete` | Go native。SQLite・認証・静的asset |
| `DELETE /api/users/me/account-links/{link_id}` | `server.go:handleAccountLinkDelete` | Go native。SQLite・認証・静的asset |
| `DELETE /api/videos/{video_id}` | `server.go:handleVideoDelete` | Go native。SQLite・認証・静的asset |
| `GET /api/bluesky/accounts/{handle}/search` | `bluesky.go:handleBlueskySearch` | Go native。外部サービス・必要時の認証。モック検証、実アカウント未操作 |
| `GET /api/bluesky/accounts/{handle}/timeline` | `bluesky.go:handleBlueskyTimeline` | Go native。外部サービス・必要時の認証。モック検証、実アカウント未操作 |
| `GET /api/captures` | `server.go:handleCaptures` | Go native。SQLite・認証・静的asset |
| `GET /api/captures/{filename}` | `server.go:handleCaptureImage` | Go native。SQLite・認証・静的asset |
| `GET /api/captures/folders` | `server.go:handleCaptureFolderList` | Go native。SQLite・認証・静的asset |
| `GET /api/captures/folders/{folder_id}/captures` | `server.go:handleCaptureFolderCaptureList` | Go native。SQLite・認証・静的asset |
| `GET /api/channels` | `server.go:handleChannels` | Go native。SQLite・認証・静的asset |
| `GET /api/channels/{channel_id}` | `server.go:handleChannel` | Go native。SQLite・認証・静的asset |
| `GET /api/channels/{channel_id}/jikkyo` | `server.go:handleChannelJikkyo` | Go native。外部サービス・必要時の認証。モック検証、実アカウント未操作 |
| `GET /api/channels/{channel_id}/logo` | `server.go:handleChannelLogo` | Go native。SQLite・認証・静的asset |
| `GET /api/data-broadcasting/internet-status` | `server.go:handleDataBroadcastingInternetStatus` | Go native。SQLite・認証・静的asset |
| `GET /api/data-broadcasting/request/{request_url:path}` | `server.go:dispatcher -> databroadcasting.go` | Go native外部Web中継。Pythonアプリproxyではない |
| `GET /api/iptv/channels` | `server.go:handleIPTVChannels` | Go native。M3U/配信URL。Pythonアプリ依存なし |
| `GET /api/iptv/countries` | `server.go:handleIPTVCountries` | Go native。M3U/配信URL。Pythonアプリ依存なし |
| `GET /api/iptv/groups` | `server.go:handleIPTVGroups` | Go native。M3U/配信URL。Pythonアプリ依存なし |
| `GET /api/iptv/logo` | `server.go:handleIPTVLogo` | Go native。M3U/配信URL。Pythonアプリ依存なし |
| `GET /api/iptv/playlist.m3u` | `server.go:handleIPTVPlaylist` | Go native。M3U/配信URL。Pythonアプリ依存なし |
| `GET /api/iptv/proxy` | `server.go:handleIPTVProxy` | Go native。M3U/配信URL。Pythonアプリ依存なし |
| `GET /api/iptv/sources` | `server.go:handleIPTVSources` | Go native。M3U/配信URL。Pythonアプリ依存なし |
| `GET /api/iptv/tvui` | `server.go:handleIPTVTVUIChannels` | Go native。M3U/配信URL。Pythonアプリ依存なし |
| `GET /api/maintenance/logs/{log_type}` | `server.go:handleMaintenanceLogs` | Go native。SQLite・認証・静的asset |
| `GET /api/niconico/auth` | `niconico.go:handleNiconicoAuthURL` | Go native。外部サービス・必要時の認証。モック検証、実アカウント未操作 |
| `GET /api/niconico/callback` | `niconico.go:handleNiconicoAuthCallback` | Go native。外部サービス・必要時の認証。モック検証、実アカウント未操作 |
| `GET /api/programs/timetable` | `server.go:handleProgramTimeTable` | Go native。SQLite・認証・静的asset |
| `GET /api/recording/conditions` | `reservation_conditions.go:handleReservationConditions` | Go native。EDCB CtrlCmd。実機予約/検索の受入れ未実施 |
| `GET /api/recording/conditions/{reservation_condition_id}` | `reservation_conditions.go:handleReservationCondition` | Go native。EDCB CtrlCmd。実機予約/検索の受入れ未実施 |
| `GET /api/recording/presets` | `recording_presets.go:handleRecordingPresets` | Go native。EDCB CtrlCmd。実機予約/検索の受入れ未実施 |
| `GET /api/recording/reservations` | `reservations.go:handleReservations` | Go native。EDCB CtrlCmd。実機予約/検索の受入れ未実施 |
| `GET /api/recording/reservations/{reservation_id}` | `reservations.go:handleReservation` | Go native。EDCB CtrlCmd。実機予約/検索の受入れ未実施 |
| `GET /api/series` | `server.go:handleSeriesList` | Go native。SQLite・認証・静的asset |
| `GET /api/series/{series_id}` | `server.go:handleSeries` | Go native。SQLite・認証・静的asset |
| `GET /api/series/search` | `server.go:handleSeriesSearch` | Go native。SQLite・認証・静的asset |
| `GET /api/settings/client` | `server.go:handleClientSettings` | Go native。SQLite・認証・静的asset |
| `GET /api/settings/server` | `server.go:handleServerSettings` | Go native。SQLite・認証・静的asset |
| `GET /api/streams/live` | `server.go:handleLiveStreams` | Go native。EDCB/Mirakurun/IPTVと外部encoder。ローカルパイプを今回追加、実機未実施 |
| `GET /api/streams/live/{display_channel_id}/{quality}` | `server.go:handleLiveStream` | Go native。EDCB/Mirakurun/IPTVと外部encoder。ローカルパイプを今回追加、実機未実施 |
| `GET /api/streams/live/{display_channel_id}/{quality}/events` | `server.go:handleLiveStreamEvents` | Go native。EDCB/Mirakurun/IPTVと外部encoder。ローカルパイプを今回追加、実機未実施 |
| `GET /api/streams/live/{display_channel_id}/{quality}/mpegts` | `server.go:handleLiveStreamMPEGTS` | Go native。EDCB/Mirakurun/IPTVと外部encoder。ローカルパイプを今回追加、実機未実施 |
| `GET /api/streams/live/{display_channel_id}/{quality}/psi-archived-data` | `server.go:handleLiveStreamPSIArchivedData` | Go native。EDCB/Mirakurun/IPTVと外部encoder。ローカルパイプを今回追加、実機未実施 |
| `GET /api/streams/video/{video_id}/{quality}/buffer` | `server.go:handleVideoStreamBuffer` | Go native。FFmpeg/HWEncC・tsreadex・psisimux。HLS実録画/保存合成を検証 |
| `GET /api/streams/video/{video_id}/{quality}/offline-stream` | `server.go:handleVideoStreamOffline` | Go native。FFmpeg/HWEncC・tsreadex・psisimux。HLS実録画/保存合成を検証 |
| `GET /api/streams/video/{video_id}/{quality}/playlist` | `server.go:handleVideoStreamPlaylist` | Go native。FFmpeg/HWEncC・tsreadex・psisimux。HLS実録画/保存合成を検証 |
| `GET /api/streams/video/{video_id}/{quality}/segment` | `server.go:handleVideoStreamSegment` | Go native。FFmpeg/HWEncC・tsreadex・psisimux。HLS実録画/保存合成を検証 |
| `GET /api/twitter/accounts/{screen_name}/search` | `twitter.go:handleTwitterSearch` | 未完成: 既定 browser backend は UnavailableBackend。CDP作業を別保存 |
| `GET /api/twitter/accounts/{screen_name}/timeline` | `twitter.go:handleTwitterTimeline` | 未完成: 既定 browser backend は UnavailableBackend。CDP作業を別保存 |
| `GET /api/twitter/video-proxy` | `twitter.go:handleTwitterVideoProxy` | Go native。SQLite・認証・静的asset |
| `GET /api/users` | `server.go:handleUsers` | Go native。SQLite・認証・静的asset |
| `GET /api/users/{username}` | `server.go:handleSpecifiedUser` | Go native。SQLite・認証・静的asset |
| `GET /api/users/{username}/icon` | `server.go:handleSpecifiedUserIcon` | Go native。SQLite・認証・静的asset |
| `GET /api/users/me` | `server.go:handleUserMe` | Go native。SQLite・認証・静的asset |
| `GET /api/users/me/icon` | `server.go:handleUserMeIcon` | Go native。SQLite・認証・静的asset |
| `GET /api/version` | `server.go:handleVersion` | Go native。SQLite・認証・静的asset |
| `GET /api/videos` | `server.go:handleVideos` | Go native。SQLite・認証・静的asset |
| `GET /api/videos/{video_id}` | `server.go:handleVideo` | Go native。SQLite・認証・静的asset |
| `GET /api/videos/{video_id}/download` | `server.go:handleVideoDownload` | Go native。SQLite・認証・静的asset |
| `GET /api/videos/{video_id}/jikkyo` | `server.go:handleVideoJikkyo` | Go native。外部サービス・必要時の認証。モック検証、実アカウント未操作 |
| `GET /api/videos/{video_id}/thumbnail` | `server.go:handleVideoThumbnail` | Go native。SQLite・認証・静的asset |
| `GET /api/videos/{video_id}/thumbnail/tiled` | `server.go:handleVideoThumbnailTile` | Go native。SQLite・認証・静的asset |
| `GET /api/videos/search` | `server.go:handleVideosSearch` | Go native。SQLite・認証・静的asset |
| `POST /api/bluesky/accounts/{handle}/posts` | `bluesky.go:handleBlueskyPost` | Go native。外部サービス・必要時の認証。モック検証、実アカウント未操作 |
| `POST /api/bluesky/auth` | `bluesky.go:handleBlueskyAuth` | Go native。外部サービス・必要時の認証。モック検証、実アカウント未操作 |
| `POST /api/captures` | `server.go:handleCaptureUpload` | Go native。SQLite・認証・静的asset |
| `POST /api/captures/folders` | `server.go:handleCaptureFolderCreate` | Go native。SQLite・認証・静的asset |
| `POST /api/captures/folders/{folder_id}/captures` | `server.go:handleCaptureFolderCaptureAdd` | Go native。SQLite・認証・静的asset |
| `POST /api/data-broadcasting/request/{request_url:path}` | `server.go:dispatcher -> databroadcasting.go` | Go native外部Web中継。Pythonアプリproxyではない |
| `POST /api/iptv/sources` | `server.go:handleIPTVSourceAdd` | Go native。M3U/配信URL。Pythonアプリ依存なし |
| `POST /api/iptv/sources/enabled` | `server.go:handleIPTVSourceEnabled` | Go native。M3U/配信URL。Pythonアプリ依存なし |
| `POST /api/iptv/tvui` | `server.go:handleIPTVTVUIRegister` | Go native。M3U/配信URL。Pythonアプリ依存なし |
| `POST /api/maintenance/restart` | `server.go:handleMaintenanceRestart` | Go native。SQLite・認証・静的asset |
| `POST /api/maintenance/run-background-analysis` | `metadata.go:handleMetadataBackgroundAnalysis` | Go native metadata。FFprobe/FFmpeg。解析細部互換性とI/O制限は継続監査 |
| `POST /api/maintenance/run-batch-scan` | `metadata.go:handleMetadataBatchScan` | Go native metadata。FFprobe/FFmpeg。解析細部互換性とI/O制限は継続監査 |
| `POST /api/maintenance/shutdown` | `server.go:handleMaintenanceShutdown` | Go native。SQLite・認証・静的asset |
| `POST /api/maintenance/update-database` | `server.go:handleMaintenanceUpdateDatabase` | Go native EDCB/Mirakurun/IPTV更新。既定startup/periodic接続済み |
| `POST /api/programs/search` | `server.go:handleProgramSearch` | Go native。EDCB CtrlCmd。実機予約/検索の受入れ未実施 |
| `POST /api/recording/conditions` | `reservation_conditions.go:handleReservationConditionAdd` | Go native。EDCB CtrlCmd。実機予約/検索の受入れ未実施 |
| `POST /api/recording/reservations` | `reservations.go:handleReservationAdd` | Go native。EDCB CtrlCmd。実機予約/検索の受入れ未実施 |
| `POST /api/twitter/accounts/{screen_name}/keep-alive` | `twitter.go:handleTwitterKeepAlive` | 未完成: 既定 browser backend は UnavailableBackend。CDP作業を別保存 |
| `POST /api/twitter/accounts/{screen_name}/tweets` | `twitter.go:handleTwitterTweet` | 未完成: 既定 browser backend は UnavailableBackend。CDP作業を別保存 |
| `POST /api/twitter/auth` | `twitter.go:handleTwitterCookieAuth` | 未完成: 既定 browser backend は UnavailableBackend。CDP作業を別保存 |
| `POST /api/users` | `server.go:handleUserCreate` | Go native。SQLite・認証・静的asset |
| `POST /api/users/me/account-links` | `server.go:handleAccountLinkCreate` | Go native。SQLite・認証・静的asset |
| `POST /api/users/token` | `server.go:handleUserAccessToken` | Go native。SQLite・認証・静的asset |
| `POST /api/videos/{video_id}/reanalyze` | `metadata.go:handleMetadataVideoReanalyze` | Go native metadata。FFprobe/FFmpeg。解析細部互換性とI/O制限は継続監査 |
| `POST /api/videos/{video_id}/thumbnail/regenerate` | `metadata.go:handleMetadataVideoThumbnailRegenerate` | Go native metadata。FFprobe/FFmpeg。解析細部互換性とI/O制限は継続監査 |
| `PUT /api/bluesky/accounts/{handle}/posts/{post_id:path}/like` | `bluesky.go:handleBlueskyPostAction (rest...)` | Go native。外部サービス・必要時の認証。モック検証、実アカウント未操作 |
| `PUT /api/bluesky/accounts/{handle}/posts/{post_id:path}/repost` | `bluesky.go:handleBlueskyPostAction (rest...)` | Go native。外部サービス・必要時の認証。モック検証、実アカウント未操作 |
| `PUT /api/captures/folders/{folder_id}` | `server.go:handleCaptureFolderUpdate` | Go native。SQLite・認証・静的asset |
| `PUT /api/recording/conditions/{reservation_condition_id}` | `reservation_conditions.go:handleReservationConditionUpdate` | Go native。EDCB CtrlCmd。実機予約/検索の受入れ未実施 |
| `PUT /api/recording/reservations/{reservation_id}` | `reservations.go:handleReservationUpdate` | Go native。EDCB CtrlCmd。実機予約/検索の受入れ未実施 |
| `PUT /api/settings/client` | `server.go:handleClientSettingsUpdate` | Go native。SQLite・認証・静的asset |
| `PUT /api/settings/server` | `server.go:handleServerSettingsUpdate` | Go native。SQLite・認証・静的asset |
| `PUT /api/streams/video/{video_id}/{quality}/keep-alive` | `server.go:handleVideoStreamKeepAlive` | Go native。FFmpeg/HWEncC・tsreadex・psisimux。HLS実録画/保存合成を検証 |
| `PUT /api/twitter/accounts/{screen_name}/tweets/{tweet_id}/favorite` | `twitter.go:handleTwitterFavorite` | 未完成: 既定 browser backend は UnavailableBackend。CDP作業を別保存 |
| `PUT /api/twitter/accounts/{screen_name}/tweets/{tweet_id}/retweet` | `twitter.go:handleTwitterRetweet` | 未完成: 既定 browser backend は UnavailableBackend。CDP作業を別保存 |
| `PUT /api/users/{username}` | `server.go:handleSpecifiedUserUpdate` | Go native。SQLite・認証・静的asset |
| `PUT /api/users/me` | `server.go:handleUserUpdate` | Go native。SQLite・認証・静的asset |
| `PUT /api/users/me/icon` | `server.go:handleUserUpdateIcon` | Go native。SQLite・認証・静的asset |
