package iptv

// BuildTSConversionArguments は IPTV のストリームを MPEG-2 TS に変換する FFmpeg の引数を組み立てる。
//
// KonomiTV の既存のライブエンコード処理 (LiveEncodingTask) は放送波の MPEG-2 TS を
// 入力として受け取るため、IPTV のストリーム (主に HLS) を MPEG-2 TS に変換して渡す。
// ここではコンテナの変換のみを行い、映像・音声は再エンコードしない。
// 移植元: server/app/utils/IPTVUtil.py の BuildTSConversionArguments()
func BuildTSConversionArguments(channel *Channel, ffmpegPath string, userAgent string) []string {
	args := []string{
		ffmpegPath,
		"-hide_banner",
		"-loglevel", "error",
		// 配信サーバーによっては User-Agent / Referer を要求するため、プレイリストの指定を反映する
		"-user_agent", userAgent,
	}
	if channel.Referrer != nil {
		args = append(args, "-referer", *channel.Referrer)
	}
	args = append(args,
		// ライブストリームではタイムスタンプが欠落することがあるため、必要に応じて生成させる
		"-fflags", "+genpts",
		"-i", channel.URL,
		// 映像・音声は再エンコードせず、MPEG-2 TS に詰め替えるだけにする
		"-c", "copy",
		"-f", "mpegts",
		"pipe:1",
	)
	return args
}
