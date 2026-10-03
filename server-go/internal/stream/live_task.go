package stream

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/config"
	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/iptv"
)

// エンコードタスクの定数 (server/app/streams/LiveEncodingTask.py と一致させること) 。
const (
	// maxRetryCount はエンコードタスクの最大リトライ回数 (この数を超えた場合はエンコードタスクを再起動しない) 。
	maxRetryCount = 10
	// tunerTSReadTimeout はチューナーから放送波 TS を読み取る際のタイムアウト (秒) 。
	tunerTSReadTimeout = 15
	// encoderTSReadTimeoutStandby はエンコーダーの出力を読み取る際のタイムアウト (Standby 時) (秒) 。
	encoderTSReadTimeoutStandby = 20
	// encoderTSReadTimeoutONAir はエンコーダーの出力を読み取る際のタイムアウト (ONAir 時) (秒) 。
	encoderTSReadTimeoutONAir = 5
	// encoderTSReadTimeoutONAirVCEEncC は VCEEncC 利用時の ONAir のタイムアウト (秒) 。
	encoderTSReadTimeoutONAirVCEEncC = 10
	// originalQualityONAirBufferBytes はオリジナル画質で ONAir に移行するために必要な累積バイト数。
	originalQualityONAirBufferBytes = 1536 * 1024
	// tsPacketSize は MPEG-TS のパケットサイズ (バイト) 。
	tsPacketSize = 188
	// writerChunkSize はライブストリームに書き込むチャンクのサイズ (バイト) 。
	writerChunkSize = 65536
	// subWriterInterval は SubWriter がチャンクバッファをチェックする間隔。
	subWriterInterval = 25 * time.Millisecond
)

// TaskOptions はエンコードタスクの実行に必要な依存関係。
type TaskOptions struct {
	// Config はサーバー設定。
	Config *config.Config
	// Paths はサーバーのパス情報。
	Paths constants.Paths
	// Logger はログ出力に使うロガー。
	Logger *slog.Logger
	// IPTV は IPTV チャンネルのマネージャー。
	IPTV *iptv.Manager
	// DB は読み取り専用のデータベース接続。
	DB *sql.DB
}

// EnableEncodingTasks はライブストリームのエンコードタスクを有効化する。
// これを呼び出さない場合、ライブストリームは Standby のまま停止する (テスト用) 。
func (m *Manager) EnableEncodingTasks(options TaskOptions) {
	m.taskOptions = &options
	m.SetTaskStarter(func(liveStream *LiveStream) { m.startEncodingTask(liveStream) })
	m.SetIPTVChannelChecker(func(displayChannelID string) bool {
		return options.IPTV != nil && options.IPTV.GetChannelByDisplayChannelID(displayChannelID) != nil
	})
}

// startEncodingTask はエンコードタスクを非同期で実行する。
func (m *Manager) startEncodingTask(liveStream *LiveStream) {
	if m.taskOptions == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	liveStream.SetTask(cancel, done)
	go func() {
		defer close(done)
		defer cancel()
		defer liveStream.ClearTask()
		task := &liveEncodingTask{
			manager:    m,
			liveStream: liveStream,
			options:    *m.taskOptions,
			logger:     m.logger,
		}
		task.run(ctx)
	}()
}

// liveEncodingTask は1つのライブストリームに対するエンコードタスク。
// 移植元: server/app/streams/LiveEncodingTask.py の LiveEncodingTask
type liveEncodingTask struct {
	// manager はライブストリームのマネージャー。
	manager *Manager
	// liveStream は対象のライブストリーム。
	liveStream *LiveStream
	// options はエンコードタスクの依存関係。
	options TaskOptions
	// logger はログ出力に使うロガー。
	logger *slog.Logger

	// retryCount はエンコードタスクのリトライ回数。
	retryCount int

	// stateMu は programPresent と lines を保護する。
	stateMu sync.Mutex
	// programPresent は現在放送中の番組 (IPTV の場合は常に nil) 。
	programPresent *database.Program
	// lines はエンコーダーの出力ログ (直近のもの) 。
	lines []string
}

// getProgramPresent は現在放送中の番組を返す。
func (t *liveEncodingTask) getProgramPresent() *database.Program {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	return t.programPresent
}

// setProgramPresent は現在放送中の番組を設定する。
func (t *liveEncodingTask) setProgramPresent(program *database.Program) {
	t.stateMu.Lock()
	t.programPresent = program
	t.stateMu.Unlock()
}

// addLine はエンコーダーの出力ログを追加する (直近 500 行のみ保持する) 。
func (t *liveEncodingTask) addLine(line string) {
	t.stateMu.Lock()
	t.lines = append(t.lines, line)
	if len(t.lines) > 500 {
		t.lines = t.lines[len(t.lines)-500:]
	}
	t.stateMu.Unlock()
}

// getLines はエンコーダーの出力ログのコピーを返す。
func (t *liveEncodingTask) getLines() []string {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	return append([]string{}, t.lines...)
}

// isOffTheAir は現在放送中の番組が停波中 (または番組情報なし) かどうかを返す。
func (t *liveEncodingTask) isOffTheAir() bool {
	program := t.getProgramPresent()
	return program == nil || isOffTheAirProgram(program.Title)
}

// logPrefix はログのプレフィックスを返す。
func (t *liveEncodingTask) logPrefix() string {
	return t.liveStream.LogPrefix()
}

// run はエンコードタスクを実行する (再起動を含む) 。
// 移植元: LiveEncodingTask.run() の終了処理 (Restart 時の再起動)
func (t *liveEncodingTask) run(ctx context.Context) {
	for {
		t.runOnce(ctx)

		// エンコードタスクを再起動する必要がない場合は終了する。
		if t.liveStream.GetStatus().Status != "Restart" {
			return
		}

		// 再起動回数が最大再起動回数に達していなければ、再起動する。
		if t.retryCount >= maxRetryCount {
			// 最大再起動回数を使い果たしたので、Offline にする。
			if program := t.getProgramPresent(); program == nil || program.IsFree {
				t.liveStream.SetStatus("Offline", "ライブストリームの再起動に失敗しました。(E-17)", false)
			} else {
				t.liveStream.SetStatus("Offline", "ライブストリームの再起動に失敗しました。契約されていないため視聴できません。(E-17)", false)
			}
			return
		}
		t.retryCount++
		time.Sleep(100 * time.Millisecond)
	}
}

// backendType は実際に使用するバックエンドを返す。
// always_receive_tv_from_mirakurun が true なら、バックエンドに関わらず常に Mirakurun / mirakc から受信する。
func (t *liveEncodingTask) backendType() string {
	if t.options.Config.General.AlwaysReceiveTVFromMirakurun {
		return "Mirakurun"
	}
	return t.options.Config.General.Backend
}

// runOnce はエンコードタスクを1回実行する。
// 移植元: LiveEncodingTask.run()
func (t *liveEncodingTask) runOnce(ctx context.Context) {
	// まだ Standby になっていなければ、ステータスを Standby に設定する。
	status := t.liveStream.GetStatus()
	if !(status.Status == "Standby" && status.Detail == "エンコードタスクを起動しています…") {
		t.liveStream.SetStatus("Standby", "エンコードタスクを起動しています…", false)
	}

	// チャンネル情報を取得する。
	// IPTV の疑似チャンネルの場合は DB にチャンネルが存在しないため、IPTV の情報からチャンネル相当のオブジェクトを生成する。
	var iptvChannel *iptv.Channel
	var channel *database.Channel
	if t.options.IPTV != nil {
		iptvChannel = t.options.IPTV.GetChannelByDisplayChannelID(t.liveStream.DisplayChannelID)
	}
	encodingChannel := &iptv.EncodingChannel{}
	if iptvChannel != nil {
		encodingChannel = iptvChannel.ToEncodingChannel()
	} else {
		var err error
		channel, err = database.GetChannelByIDOrDisplayChannelID(ctx, t.options.DB, t.liveStream.DisplayChannelID)
		if err != nil {
			t.logger.Error(t.logPrefix()+" Failed to get the channel. "+err.Error(), "live_stream_id", t.liveStream.ID)
			t.liveStream.SetStatus("Offline", "チャンネル情報の取得に失敗しました。(E-18)", false)
			return
		}
		encodingChannel = &iptv.EncodingChannel{
			ID:                channel.ID,
			DisplayChannelID:  channel.DisplayChannelID,
			NetworkID:         channel.NetworkID,
			ServiceID:         channel.ServiceID,
			TransportStreamID: channel.TransportStreamID,
			RemoconID:         channel.RemoconID,
			ChannelNumber:     channel.ChannelNumber,
			Type:              channel.Type,
			Name:              channel.Name,
			IsSubchannel:      channel.IsSubchannel,
			IsRadiochannel:    channel.IsRadiochannel,
			IsWatchable:       channel.IsWatchable,
		}
	}

	// 現在の番組情報を取得する。
	t.setProgramPresent(nil)
	if iptvChannel == nil {
		present, _, err := database.GetCurrentAndNextProgram(ctx, t.options.DB, encodingChannel.ID)
		if err == nil {
			t.setProgramPresent(present)
		}
	}
	if program := t.getProgramPresent(); program != nil {
		t.logger.Info(t.logPrefix()+" Title: "+program.Title, "live_stream_id", t.liveStream.ID)
	} else {
		t.logger.Info(t.logPrefix()+" Title: 番組情報がありません", "live_stream_id", t.liveStream.ID)
	}

	// PSI/SI データアーカイバーを初期化する。
	// IPTV のストリームには日本のデータ放送が存在しないため、IPTV では初期化しない。
	if iptvChannel == nil {
		archiver := NewLivePSIDataArchiver(encodingChannel.ServiceID, t.options.Paths.LibraryPath("psisiarc"), t.logger)
		t.liveStream.SetPSIDataArchiver(archiver)
	} else {
		t.liveStream.SetPSIDataArchiver(nil)
	}

	isOriginalQuality := t.liveStream.Quality == OriginalQuality

	// ***** tsreadex プロセスの作成と実行 *****

	tsreadexArgs := []string{
		// 取り除く TS パケットの10進数の PID (EIT の PID を指定)
		"-x", "18/38/39",
		// 特定サービスのみを選択して出力するフィルタを有効にする
		// IPTV のストリームは日本の放送波ではないため、サービス単位のフィルタは行わない (-1)
		"-n", "-1",
	}
	if t.options.Config.TV.DebugModeTSPath == nil && iptvChannel == nil {
		tsreadexArgs[3] = fmt.Sprintf("%d", encodingChannel.ServiceID)
	}

	if isOriginalQuality {
		// オリジナル画質 (mpeg2toh264 で再生) の場合はエンコーダーを通す必要自体がないので、
		// PMT 上に常に AAC 音声・字幕・文字スーパーが存在する状態にはしつつ、変換せずにそのまま出力する。
		tsreadexArgs = append(tsreadexArgs,
			"-a", "1",
			"-b", "1",
			"-c", "1",
			"-u", "1",
		)
	} else {
		// エンコード済み画質はエンコーダーが安定して入力できる音声と Timed Metadata を生成する。
		tsreadexArgs = append(tsreadexArgs,
			"-a", "13",
			"-b", "5",
			"-c", "5",
			"-u", "1",
			"-d", "9",
		)
	}

	if t.options.Config.TV.DebugModeTSPath == nil {
		// 通常は標準入力を指定する。
		tsreadexArgs = append(tsreadexArgs, "-")
	} else {
		// デバッグモード: 指定された TS ファイルを読み込む (読み込み速度を 2350KB/s に制限) 。
		tsreadexArgs = append(tsreadexArgs, "-l", "2350", *t.options.Config.TV.DebugModeTSPath)
	}

	tsreadex, tsreadexStdout, err := startProcess(t.options.Paths.LibraryPath("tsreadex"), tsreadexArgs, true, true, true)
	if err != nil {
		t.logger.Error(t.logPrefix()+" Failed to start tsreadex. "+err.Error(), "live_stream_id", t.liveStream.ID)
		t.liveStream.SetStatus("Offline", "エンコーダーの起動に失敗しました。(E-19)", false)
		t.liveStream.DisconnectAll()
		return
	}
	defer tsreadex.kill()

	// ***** エンコーダープロセスの作成と実行 *****

	// フル HD 放送が行われているチャンネルかを取得する。
	// IPTV のストリームは 1920x1080 のプログレッシブが一般的なので、フル HD として扱う。
	isFullHDChannel := true
	if iptvChannel == nil {
		isFullHDChannel = isFullHDChannelID(encodingChannel.NetworkID, encodingChannel.ServiceID)
	}

	// ラジオチャンネルでは HW エンコードの意味がないため、FFmpeg に固定する。
	encoderType := t.options.Config.General.Encoder
	if encodingChannel.IsRadiochannel {
		encoderType = "FFmpeg"
	}

	var encoder *runningProcess
	if isOriginalQuality {
		// オリジナル画質ではエンコーダーを通さず、tsreadex の出力をそのままライブストリームに流す。
		t.logger.Info(t.logPrefix()+" Original MPEG-TS stream passthrough is starting.", "live_stream_id", t.liveStream.ID)
		encoder = newPassthroughProcess(tsreadexStdout)
	} else if encoderType == "FFmpeg" {
		var encoderArgs []string
		if encodingChannel.IsRadiochannel {
			encoderArgs = BuildFFmpegOptionsForRadio(t.retryCount)
		} else {
			encoderArgs = BuildFFmpegOptions(t.liveStream.Quality, encodingChannel.Type, isFullHDChannel, t.liveStream.EncodingOptions, t.retryCount)
		}
		t.logger.Info(t.logPrefix()+" FFmpeg Commands:\n"+t.options.Paths.LibraryPath("FFmpeg")+" "+strings.Join(encoderArgs, " "), "live_stream_id", t.liveStream.ID)
		encoder, err = startEncoderProcess(t.options.Paths.LibraryPath("FFmpeg"), encoderArgs, tsreadexStdout)
	} else {
		encoderArgs := BuildHWEncCOptions(t.liveStream.Quality, encoderType, encodingChannel.Type, isFullHDChannel,
			t.liveStream.EncodingOptions, t.retryCount,
			func(option string) bool {
				return HWEncCOptionAvailable(t.options.Paths.LibraryPath(encoderType), option)
			})
		t.logger.Info(t.logPrefix()+" "+encoderType+" Commands:\n"+t.options.Paths.LibraryPath(encoderType)+" "+strings.Join(encoderArgs, " "), "live_stream_id", t.liveStream.ID)
		encoder, err = startEncoderProcess(t.options.Paths.LibraryPath(encoderType), encoderArgs, tsreadexStdout)
	}
	if err != nil {
		t.logger.Error(t.logPrefix()+" Failed to start the encoder. "+err.Error(), "live_stream_id", t.liveStream.ID)
		t.liveStream.SetStatus("Offline", "エンコーダーの起動に失敗しました。(E-19)", false)
		t.liveStream.DisconnectAll()
		return
	}
	defer encoder.kill()
	if !isOriginalQuality {
		// tsreadex の出力はエンコーダーに渡したので、親プロセス側ではクローズする。
		// オリジナル画質の場合は tsreadex の出力を Go 側で直接読み取るため、クローズしない。
		tsreadexStdout.Close()
	}

	// ***** チューナーの起動と接続 *****

	// エンコードタスクが稼働中かどうか。
	var isRunning atomic.Bool
	isRunning.Store(true)

	// チューナー (または IPTV の変換プロセス) からの入力。
	var source io.ReadCloser
	var iptvProcess *runningProcess
	var mirakurunResponse *http.Response
	// チューナーからの放送波 TS の最終読み取り時刻 (単調増加時間) 。
	tunerTSReadAt := time.Now()
	var tunerTSReadAtLock sync.Mutex
	// Mirakurun の Service Stream API のレスポンスステータス。
	mirakurunStatusCode := 0

	switch {
	case iptvChannel != nil:
		// IPTV のストリームを MPEG-2 TS に変換し、その標準出力を放送波の TS と同じように扱う。
		t.liveStream.SetStatus("Standby", "IPTV のストリームに接続しています…", false)
		args := iptv.BuildTSConversionArguments(iptvChannel, t.options.Paths.LibraryPath("FFmpeg"), t.options.Config.IPTV.UserAgent)
		iptvProcess, _, err = startProcess(args[0], args[1:], false, true, true)
		if err != nil {
			t.logger.Error(t.logPrefix()+" Failed to start the IPTV conversion process. "+err.Error(), "live_stream_id", t.liveStream.ID)
			t.liveStream.SetStatus("Offline", "IPTV のストリームに接続できませんでした。(E-20)", false)
			t.liveStream.DisconnectAll()
			return
		}
		defer iptvProcess.kill()
		source = iptvProcess.stdout

	case t.backendType() == "Mirakurun":
		// Mirakurun 形式のサービス ID (NID と SID を 5 桁でゼロ埋めした上で int に変換する) 。
		mirakurunServiceID := fmt.Sprintf("%05d%05d", encodingChannel.NetworkID, encodingChannel.ServiceID)
		t.liveStream.SetStatus("Standby", "チューナーを起動しています…", false)
		mirakurunResponse, err = t.openMirakurunStream(ctx, mirakurunServiceID)
		if err != nil {
			// 番組名に「放送休止」などが入っていれば停波によるものとみなし、そうでないならチューナーへの接続に失敗したものとする。
			if t.isOffTheAir() {
				t.liveStream.SetStatus("Offline", "この時間は放送を休止しています。(E-01M)", false)
			} else {
				t.liveStream.SetStatus("Offline", "チューナーへの接続に失敗しました。チューナー側に何らかの問題があるかもしれません。(E-01M)", false)
			}
			t.liveStream.DisconnectAll()
			return
		}
		defer mirakurunResponse.Body.Close()
		mirakurunStatusCode = mirakurunResponse.StatusCode
		source = mirakurunResponse.Body

	default:
		// EDCB バックエンドは Go 版では未対応。
		t.logger.Warn(t.logPrefix()+" The EDCB backend is not supported in the Go server.", "live_stream_id", t.liveStream.ID)
		t.liveStream.SetStatus("Offline", "EDCB バックエンドは Go 版サーバーでは未対応です。(E-02E)", false)
		t.liveStream.DisconnectAll()
		return
	}

	// ***** チューナーからの出力の読み込み → tsreadex・エンコーダーへの書き込み *****

	tsreadexStdin := tsreadex.stdin
	var backgroundTasks sync.WaitGroup
	readerDone := make(chan struct{})

	backgroundTasks.Add(1)
	go func() {
		defer backgroundTasks.Done()
		defer close(readerDone)
		defer tsreadexStdin.Close()
		buffer := make([]byte, tsPacketSize*256)
		for {
			read, err := io.ReadFull(source, buffer)
			if read > 0 {
				tunerTSReadAtLock.Lock()
				tunerTSReadAt = time.Now()
				tunerTSReadAtLock.Unlock()

				// ストリームデータを tsreadex の標準入力に書き込む。
				if _, writeErr := tsreadexStdin.Write(buffer[:read]); writeErr != nil {
					break
				}

				// 生の放送波の TS パケットを PSI/SI データアーカイバーに送信する。
				if archiver := t.liveStream.PSIDataArchiver(); archiver != nil {
					archiver.PushTSPacketData(buffer[:read])
				}
			}
			if err != nil {
				break
			}
			// エンコードタスクが終了しているか既にエンコーダープロセスが終了していたら、タスクを終了する。
			if !isRunning.Load() || tsreadex.exited() || encoder.exited() {
				break
			}
		}
		if mirakurunResponse != nil {
			mirakurunResponse.Body.Close()
		}
	}()

	// ***** tsreadex・エンコーダーからの出力の読み込み → ライブストリームへの書き込み *****

	writer := newStreamWriter(t.liveStream, isOriginalQuality, &t.retryCount)
	backgroundTasks.Add(1)
	go func() {
		defer backgroundTasks.Done()
		writer.writerLoop(encoder)
	}()
	backgroundTasks.Add(1)
	go func() {
		defer backgroundTasks.Done()
		writer.subWriterLoop(tsreadex, encoder, &isRunning)
	}()

	// ***** エンコーダーの状態監視 *****

	if !isOriginalQuality {
		backgroundTasks.Add(1)
		go func() {
			defer backgroundTasks.Done()
			t.observeEncoder(encoder, encoderType, isOriginalQuality, tsreadex, &isRunning)
		}()
	}

	// ***** エンコードタスク全体の制御 *****

	t.controlLoop(ctx, encoderType, encoder, tsreadex, &isRunning, &tunerTSReadAt, &tunerTSReadAtLock, mirakurunResponse, &mirakurunStatusCode, encodingChannel, readerDone)

	// ***** エンコードタスクの終了処理 *****

	// 稼働中フラグをオフにし、すべての非同期処理を終了させる。
	isRunning.Store(false)
	tsreadex.kill()
	encoder.kill()
	source.Close()

	// すべての視聴中クライアントのライブストリームへの接続を切断する。
	t.liveStream.DisconnectAll()

	// PSI/SI データアーカイバーを終了・破棄する。
	if archiver := t.liveStream.PSIDataArchiver(); archiver != nil {
		archiver.Destroy()
		t.liveStream.SetPSIDataArchiver(nil)
	}

	// 非同期処理の終了を待つ (最大 5 秒) 。
	waitWithTimeout(&backgroundTasks, 5*time.Second)
}

// unixNow は現在時刻を Unix 時間 (秒、浮動小数点数) で返す。
func unixNow() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

// openMirakurunStream は Mirakurun / mirakc の Service Stream API へ HTTP リクエストを開始する。
func (t *liveEncodingTask) openMirakurunStream(ctx context.Context, mirakurunServiceID string) (*http.Response, error) {
	baseURL := strings.TrimSuffix(t.options.Config.General.MirakurunURL, "/")
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, baseURL+"/api/services/"+mirakurunServiceID+"/stream", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-Mirakurun-Priority", "0")
	client := &http.Client{}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return response, nil
	}
	return response, nil
}

// controlLoop はエンコードタスク全体を制御する。
// 移植元: LiveEncodingTask.run() の Controller()
func (t *liveEncodingTask) controlLoop(
	ctx context.Context,
	encoderType string,
	encoder *runningProcess,
	tsreadex *runningProcess,
	isRunning *atomic.Bool,
	tunerTSReadAt *time.Time,
	tunerTSReadAtLock *sync.Mutex,
	mirakurunResponse *http.Response,
	mirakurunStatusCode *int,
	channel *iptv.EncodingChannel,
	readerDone <-chan struct{},
) {
	for {
		status := t.liveStream.GetStatus()

		// 現在放送中の番組が終了した際に、現在の番組情報を新しいものに更新する。
		if program := t.getProgramPresent(); program != nil && time.Now().After(program.EndTime) {
			present, _, err := database.GetCurrentAndNextProgram(ctx, t.options.DB, channel.ID)
			if err == nil && present != nil {
				t.logger.Info(t.logPrefix()+" Title: "+present.Title, "live_stream_id", t.liveStream.ID)
			}
			t.setProgramPresent(present)
		}

		// 現在 ONAir でかつクライアント数が 0 なら Idling (アイドリング状態) に移行する。
		if status.Status == "ONAir" && status.ClientCount == 0 {
			t.liveStream.SetStatus("Idling", "ライブストリームは Idling です。", false)
		}

		// 現在 Idling でかつ最終更新から max_alive_time 秒以上経っていたらエンコーダーを終了し、Offline 状態に移行する。
		if status.Status == "Idling" && unixNow()-status.UpdatedAt > t.options.Config.TV.MaxAliveTime {
			t.liveStream.SetStatus("Offline", "ライブストリームは Offline です。", false)
		}

		// ***** 異常処理 (エンコードタスク再起動による回復が不可能) *****

		// 前回チューナーからの放送波 TS を読み取ってから TUNER_TS_READ_TIMEOUT 秒以上経過していたら、
		// 停波中もしくはチューナーからの放送波 TS の送信が停止したと判断して Offline に移行する。
		tunerTSReadAtLock.Lock()
		elapsed := time.Since(*tunerTSReadAt).Seconds()
		tunerTSReadAtLock.Unlock()
		if elapsed > tunerTSReadTimeout {
			if t.isOffTheAir() {
				t.liveStream.SetStatus("Offline", "この時間は放送を休止しています。(E-11)", false)
			} else {
				t.liveStream.SetStatus("Offline", "チューナーからの放送波の受信がタイムアウトしました。チューナー側に何らかの問題があるかもしれません。(E-11)", false)
			}
		}

		// Mirakurun の Service Stream API からエラーが返された場合。
		if t.backendType() == "Mirakurun" && mirakurunResponse != nil && *mirakurunStatusCode != http.StatusOK {
			mirakurunOrMirakc := "Mirakurun"
			if server := mirakurunResponse.Header.Get("server"); strings.Contains(server, "mirakc") {
				mirakurunOrMirakc = "mirakc"
			}
			// Offline にしてエンコードタスクを停止する (mirakc はチューナー不足時に 503 ではなく 404 を返すことがある) 。
			if *mirakurunStatusCode == http.StatusServiceUnavailable ||
				(*mirakurunStatusCode == http.StatusNotFound && mirakurunOrMirakc == "mirakc") {
				t.liveStream.SetStatus("Offline", "チューナーの起動に失敗しました。空きチューナーが不足している可能性があります。(E-12M)", false)
			} else if *mirakurunStatusCode == http.StatusNotFound {
				t.liveStream.SetStatus("Offline", fmt.Sprintf("現在このチャンネルは受信できません。%s 側に問題があるかもしれません。(HTTP Error %d) (E-12M)", mirakurunOrMirakc, *mirakurunStatusCode), false)
			} else {
				t.liveStream.SetStatus("Offline", fmt.Sprintf("チューナーで不明なエラーが発生しました。%s 側に問題があるかもしれません。(HTTP Error %d) (E-12M)", mirakurunOrMirakc, *mirakurunStatusCode), false)
			}
			return
		}

		// ***** 異常処理 (エンコードタスク再起動による回復が可能) *****

		// ストリームデータの最終書き込み時刻から一定時間が経過しているなら、エンコーダーがフリーズしたものとみなす。
		encoderTSReadTimeoutONAir := encoderTSReadTimeoutONAir
		if encoderType == "VCEEncC" {
			encoderTSReadTimeoutONAir = encoderTSReadTimeoutONAirVCEEncC
		}
		streamDataLastWriteTime := unixNow() - t.liveStream.GetStreamDataWrittenAt()
		if (status.Status == "Standby" && streamDataLastWriteTime > encoderTSReadTimeoutStandby) ||
			(status.Status == "ONAir" && streamDataLastWriteTime > float64(encoderTSReadTimeoutONAir)) {

			if t.isOffTheAir() {
				t.liveStream.SetStatus("Offline", "この時間は放送を休止しています。(E-13)", false)
			} else {
				// できるだけエンコーダーのエラーメッセージを拾ってログを出力してから終了したいので、1秒間実行を待機する。
				time.Sleep(1 * time.Second)
				if t.liveStream.SetStatus("Restart", "エンコードが途中で停止しました。エンコードタスクを再起動しています… (ER-04)", false) {
					t.logEncoderLines(encoderType, 50, 150)
				}
			}
		}

		// チューナーとの接続が切断された場合。
		if t.backendType() == "Mirakurun" && mirakurunResponse != nil {
			select {
			case <-readerDone:
				if status.Status == "Standby" || status.Status == "ONAir" {
					t.liveStream.SetStatus("Restart", "チューナーとの接続が切断されました。エンコードタスクを再起動しています… (ER-05)", false)
				}
			default:
			}
		}

		// エンコーダーが意図せず終了した場合。
		if encoder.exited() {
			// H.265/HEVC でのエンコードに非対応かどうかを確認する (初回のみ) 。
			if t.retryCount == 0 {
				if t.checkHEVCUnsupported(encoderType) {
					return
				}
			}
			// エンコーダーの再起動で復帰できる可能性があるので、エンコードタスクを再起動する。
			if t.liveStream.GetStatus().Status != "Offline" {
				if t.liveStream.SetStatus("Restart", "エンコーダーが強制終了されました。エンコードタスクを再起動しています… (ER-06)", false) {
					t.logEncoderLines(encoderType, 50, 150)
				}
			}
			// エンコーダーが既に終了しているため、後続の異常検出処理を実行する意味がない。
			return
		}

		// この時点で最新のライブストリームのステータスが Offline か Restart に変更されていたら、エンコードタスクの終了処理に移る。
		status = t.liveStream.GetStatus()
		if status.Status == "Offline" || status.Status == "Restart" {
			return
		}

		// ビジーにならないように 0.1 秒待機する。
		time.Sleep(100 * time.Millisecond)
	}
}

// checkHEVCUnsupported はエンコーダーのログから H.265/HEVC 非対応の環境かどうかを判定し、Offline に移行する。
// 移植元: LiveEncodingTask.run() の Controller() の H.265/HEVC 非対応チェック
func (t *liveEncodingTask) checkHEVCUnsupported(encoderType string) bool {
	for _, line := range t.getLines() {
		switch {
		case encoderType == "QSVEncC" && strings.Contains(line, "HEVC encoding is not supported on current platform."):
			t.liveStream.SetStatus("Offline", "お使いの Intel GPU は H.265/HEVC でのエンコードに対応していません。(E-14HQ)", false)
			return true
		case encoderType == "NVEncC" && strings.Contains(line, "does not support H.265/HEVC encoding."):
			// 他の行に available for encode. という文字列が含まれている場合は除外する。
			availableForEncode := false
			for _, line2 := range t.getLines() {
				if strings.Contains(line2, "available for encode.") {
					availableForEncode = true
					break
				}
			}
			if !availableForEncode {
				t.liveStream.SetStatus("Offline", "お使いの NVIDIA GPU は H.265/HEVC でのエンコードに対応していません。(E-15HN)", false)
				return true
			}
		case encoderType == "VCEEncC" && strings.Contains(line, "HW Acceleration of H.265/HEVC is not supported on this platform."):
			t.liveStream.SetStatus("Offline", "お使いの AMD GPU は H.265/HEVC でのエンコードに対応していません。(E-16HV)", false)
			return true
		}
	}
	return false
}

// logEncoderLines はエンコーダーのログの直近の行を出力する。
func (t *liveEncodingTask) logEncoderLines(encoderType string, ffmpegLines int, hwencLines int) {
	lines := t.getLines()
	count := hwencLines
	if encoderType == "FFmpeg" {
		count = ffmpegLines
	}
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	for _, line := range lines {
		t.logger.Warn(t.logPrefix()+" "+line, "live_stream_id", t.liveStream.ID)
	}
}

// observeEncoder はエンコーダーの出力ログを監視し、進捗に応じてステータスを更新する。
// 移植元: LiveEncodingTask.run() の EncoderObServer()
func (t *liveEncodingTask) observeEncoder(encoder *runningProcess, encoderType string, isOriginalQuality bool, tsreadex *runningProcess, isRunning *atomic.Bool) {
	// エンコーダーのログファイルを開く (エンコーダーログ有効時のみ) 。
	var encoderLog *os.File
	if t.options.Config.General.DebugEncoder {
		count := 1
		logPath := fmt.Sprintf("%s%cKonomiTV-Encoder-%s.log", t.options.Paths.LogsDir, os.PathSeparator, t.liveStream.ID)
		for {
			if _, err := os.Stat(logPath); errors.Is(err, os.ErrNotExist) {
				break
			}
			logPath = fmt.Sprintf("%s%cKonomiTV-Encoder-%s-%d.log", t.options.Paths.LogsDir, os.PathSeparator, t.liveStream.ID, count)
			count++
		}
		if file, err := os.Create(logPath); err == nil {
			encoderLog = file
		}
	}
	if encoderLog != nil {
		defer encoderLog.Close()
	}

	reader := bufio.NewReader(encoder.stderr)
	for {
		// 行ごとに随時読み込む (\r か \n が来たら行としてデコードする) 。
		// FFmpeg はコンソールの行を上書きするために \r しか出力しないため、行単位ではなく文字単位で読み取る。
		line, err := readEncoderLine(reader)
		if err != nil {
			break
		}
		if line == "" {
			continue
		}

		// エンコード進捗のログだったら、正規表現で余計なゴミを取り除く。
		if match := encoderProgressPattern.FindStringSubmatch(line); match != nil {
			line = match[1]
		}

		// 山ほど出力されるメッセージと空行をログから除外する。
		if isEncoderNoise(line) {
			continue
		}

		// ログリストに行単位で追加する。
		t.addLine(line)

		// ストリーム関連のログを表示する。
		if strings.Contains(line, "Stream #0:") || t.options.Config.General.DebugEncoder {
			t.logger.Debug(t.logPrefix()+" ["+encoderType+"] "+line, "live_stream_id", t.liveStream.ID)
		}

		// エンコーダーのログ出力が有効なら、エンコーダーのログファイルに書き込む。
		if encoderLog != nil {
			_, _ = encoderLog.WriteString(line + "\n")
		}

		// エンコードの進捗を判定し、ステータスを更新する (Standby の間のみ) 。
		if t.liveStream.GetStatus().Status == "Standby" {
			t.updateStatusFromEncoderLine(encoderType, line)
		}

		// 特定のエラーログが出力されている場合は回復が見込めないため、エンコーダーを終了する。
		t.handleEncoderErrorLine(encoderType, line)

		// エンコードタスクが終了しているか既にエンコーダープロセスが終了していたら、タスクを終了する。
		if !isRunning.Load() || tsreadex.exited() || encoder.exited() {
			break
		}
	}
}

// updateStatusFromEncoderLine はエンコーダーの進捗ログからステータスを更新する。
// 移植元: LiveEncodingTask.run() の EncoderObServer() の進捗判定
func (t *liveEncodingTask) updateStatusFromEncoderLine(encoderType string, line string) {
	if encoderType == "FFmpeg" {
		if strings.Contains(line, "arib parser was created") || strings.Contains(line, "Invalid frame dimensions 0x0.") {
			t.liveStream.SetStatus("Standby", "エンコードを開始しています…", false)
		} else if strings.Contains(line, "frame=    1 fps=0.0 q=0.0") || strings.Contains(line, "size=       0kB time=00:00") {
			t.liveStream.SetStatus("Standby", "バッファリングしています…", false)
		} else if strings.Contains(line, "frame=") || strings.Contains(line, "bitrate=") {
			t.liveStream.SetStatus("ONAir", "ライブストリームは ONAir です。", false)
			// エラーから回復した場合は、エンコードタスクの再起動回数のカウントをリセットする。
			if t.retryCount > 0 {
				t.retryCount = 0
			}
		}
	} else {
		if strings.Contains(line, "opened file \"pipe:0\"") {
			t.liveStream.SetStatus("Standby", "エンコードを開始しています…", false)
		} else if strings.Contains(line, "starting output thread...") {
			t.liveStream.SetStatus("Standby", "バッファリングしています…", false)
		} else if strings.Contains(line, "Encode Thread:") {
			t.liveStream.SetStatus("Standby", "バッファリングしています…", false)
		} else if strings.Contains(line, " frames: ") {
			t.liveStream.SetStatus("ONAir", "ライブストリームは ONAir です。", false)
			if t.retryCount > 0 {
				t.retryCount = 0
			}
		}
	}
}

// handleEncoderErrorLine はエンコーダーのエラーログからステータスを更新する。
// 移植元: LiveEncodingTask.run() の EncoderObServer() のエラー判定
func (t *liveEncodingTask) handleEncoderErrorLine(encoderType string, line string) {
	if encoderType == "FFmpeg" {
		if strings.Contains(line, "Stream map '0:v:0' matches no streams.") {
			if t.isOffTheAir() {
				t.liveStream.SetStatus("Offline", "この時間は放送を休止しています。(E-04F)", false)
			} else {
				t.liveStream.SetStatus("Offline", "チューナーからの放送波の受信に失敗したため、エンコードを開始できません。(E-04F)", false)
			}
		} else if strings.Contains(line, "Conversion failed!") {
			if t.liveStream.SetStatus("Restart", "エンコード中に予期しないエラーが発生しました。エンコードタスクを再起動しています… (ER-01F)", false) {
				t.logEncoderLines(encoderType, 50, 150)
			}
		}
		return
	}
	switch {
	case strings.Contains(line, "error finding stream information."):
		if t.isOffTheAir() {
			t.liveStream.SetStatus("Offline", "この時間は放送を休止しています。(E-05H)", false)
		} else {
			t.liveStream.SetStatus("Offline", "チューナーからの放送波の受信に失敗したため、エンコードを開始できません。(E-05H)", false)
		}
	case encoderType == "NVEncC" && strings.Contains(line, "due to the NVIDIA's driver limitation."):
		t.liveStream.SetStatus("Offline", "NVENC のエンコードセッションが不足しているため、エンコードを開始できません。(E-06HN)", false)
	case encoderType == "QSVEncC" && (strings.Contains(line, "unable to decode by qsv.") || strings.Contains(line, "No device found for QSV encoding!")):
		t.liveStream.SetStatus("Offline", "お使いの PC 環境は QSVEncC エンコーダーに対応していません。(E-07HQ)", false)
	case encoderType == "QSVEncC" && strings.Contains(line, "iHD_drv_video.so init failed"):
		t.liveStream.SetStatus("Offline", "お使いの PC 環境は Linux 版 QSVEncC エンコーダーに対応していません。第5世代以前の古い CPU をお使いの可能性があります。(E-08HQ)", false)
	case encoderType == "NVEncC" && strings.Contains(line, "CUDA not available."):
		t.liveStream.SetStatus("Offline", "お使いの PC 環境は NVEncC エンコーダーに対応していません。(E-09HN)", false)
	case encoderType == "VCEEncC" && (strings.Contains(line, "Failed to initalize VCE factory:") || strings.Contains(line, "Assertion failed:Init() failed to vkCreateInstance")):
		t.liveStream.SetStatus("Offline", "お使いの PC 環境は VCEEncC エンコーダーに対応していません。(E-10HV)", false)
	case strings.Contains(line, "Consider increasing the value for the --input-analyze and/or --input-probesize!"):
		t.liveStream.SetStatus("Restart", "入力ストリームの解析に失敗しました。エンコードタスクを再起動しています… (ER-02H)", false)
	case strings.Contains(line, "finished with error!"):
		// Controller 側で完全にエンコーダープロセスが落ちたタイミングで HEVC 非対応かなどを判断しているため、0.5 秒待機してから実行する。
		time.Sleep(500 * time.Millisecond)
		if t.liveStream.SetStatus("Restart", "エンコード中に予期しないエラーが発生しました。エンコードタスクを再起動しています… (ER-03H)", false) {
			t.logEncoderLines(encoderType, 50, 150)
		}
	}
}

// isFullHDChannelID はネットワーク ID とサービス ID から、そのチャンネルでフル HD 放送が行われているかを返す。
// 移植元: LiveEncodingTask.isFullHDChannel()
func isFullHDChannelID(networkID int, serviceID int) bool {
	// 地デジでフル HD 放送を行っているチャンネルのネットワーク ID。
	switch networkID {
	case 31811, 31940, 32038, 32162, 32311, 32466:
		return true
	}
	// BS でフル HD 放送を行っているチャンネルのサービス ID。
	if networkID == 0x0004 {
		switch serviceID {
		case 103, 191, 192, 193, 211:
			return true
		}
	}
	// BS4K・CS4K (放送終了) は 4K 放送なのでフル HD 扱いとする。
	if networkID == 0x000B || networkID == 0x000C {
		return true
	}
	return false
}

// isOffTheAirProgram は番組名から停波中の番組かを判定する。
// 移植元: app/models/Program.py の Program.isOffTheAirProgram()
func isOffTheAirProgram(title string) bool {
	return title == "番組情報がありません" ||
		strings.Contains(title, "放送休止") ||
		strings.Contains(title, "放送終了") ||
		strings.Contains(title, "休止") ||
		strings.Contains(title, "停波")
}

// encoderProgressPattern はエンコード進捗のログから余計なゴミを取り除くための正規表現。
var encoderProgressPattern = regexp.MustCompile(
	`^.*?([1-9][0-9]+ frames: [0-9\.]+ fps, [0-9]+ kb/s(?:, GPU [0-9]+%, VE [0-9]+%, VD [0-9]+%|, GPU [0-9]+%, VD [0-9]+%)?)$`)

// readEncoderLine はエンコーダーの出力から \r または \n までの1行を読み取る。
func readEncoderLine(reader *bufio.Reader) (string, error) {
	var builder strings.Builder
	for {
		value, err := reader.ReadByte()
		if err != nil {
			if builder.Len() > 0 {
				return strings.TrimSpace(builder.String()), nil
			}
			return "", err
		}
		if value == '\r' || value == '\n' {
			break
		}
		builder.WriteByte(value)
	}
	return strings.TrimSpace(builder.String()), nil
}

// isEncoderNoise はエンコーダーのログから除外する行かどうかを返す。
// 移植元: LiveEncodingTask.run() の EncoderObServer() の除外判定
func isEncoderNoise(line string) bool {
	if line == "" {
		return true
	}
	if strings.Contains(line, "removing 2 bytes from input bitstream not read by decoder.") ||
		strings.Contains(line, "Delay between the") ||
		strings.Contains(line, "[h264_metadata") ||
		strings.Contains(line, "[hevc_metadata") ||
		strings.Contains(line, "packet in the muxing queue") ||
		strings.Contains(line, "ing output") {
		return true
	}
	// FFmpeg と HWEncC のログが衝突して行の先頭が欠けることがあるので、できるだけ多く弾く。
	for _, suffix := range []string{"ng output", "g output", " output", "output", "utput", "tput", "put", "ut", "t"} {
		if line == suffix {
			return true
		}
	}
	return false
}

// streamWriter はエンコーダーの出力をライブストリームのクライアントに書き込む。
type streamWriter struct {
	liveStream *LiveStream
	// isOriginalQuality はオリジナル画質のストリームかどうか。
	isOriginalQuality bool
	// retryCount はエンコードタスクのリトライ回数 (ONAir 移行時にリセットする) 。
	retryCount *int
	// mu は buffer と chunkWrittenAt を保護する。
	mu sync.Mutex
	// buffer はエンコーダーの出力のチャンクが積み増されていくバッファ。
	buffer []byte
	// chunkWrittenAt はチャンクの最終書き込み時刻 (単調増加時間) 。
	chunkWrittenAt time.Time
	// originalQualityBytesWritten はオリジナル画質向けの累積書き込みバイト数。
	originalQualityBytesWritten int
}

// newStreamWriter はストリームライターを生成する。
func newStreamWriter(liveStream *LiveStream, isOriginalQuality bool, retryCount *int) *streamWriter {
	return &streamWriter{
		liveStream:        liveStream,
		isOriginalQuality: isOriginalQuality,
		retryCount:        retryCount,
	}
}

// writerLoop はエンコーダーの出力を読み取り、64KiB 以上になったらライブストリームに書き込む。
// 移植元: LiveEncodingTask.run() の Writer()
func (w *streamWriter) writerLoop(encoder *runningProcess) {
	chunk := make([]byte, tsPacketSize)
	for {
		if _, err := io.ReadFull(encoder.stdout, chunk); err != nil {
			// エンコーダーの出力が終了したら、エンコーダーが終了したものとして扱う。
			encoder.finished.Store(true)
			return
		}
		w.mu.Lock()
		w.buffer = append(w.buffer, chunk...)
		if len(w.buffer) >= writerChunkSize {
			w.flushLocked()
		}
		w.mu.Unlock()
	}
}

// subWriterLoop は 25ms 間隔でバッファをチェックし、書き込みが滞っていればフラッシュする。
// 移植元: LiveEncodingTask.run() の SubWriter()
func (w *streamWriter) subWriterLoop(tsreadex *runningProcess, encoder *runningProcess, isRunning *atomic.Bool) {
	for {
		time.Sleep(subWriterInterval)
		w.mu.Lock()
		if time.Since(w.chunkWrittenAt) > subWriterInterval && len(w.buffer) > 0 {
			w.flushLocked()
		}
		w.mu.Unlock()
		if !isRunning.Load() || tsreadex.exited() || encoder.exited() {
			return
		}
	}
}

// flushLocked はバッファの内容をライブストリームに書き込む (呼び出し元でロックを取得すること) 。
func (w *streamWriter) flushLocked() {
	chunkSize := len(w.buffer)
	w.liveStream.WriteStreamData(w.buffer)
	w.buffer = nil
	w.chunkWrittenAt = time.Now()

	// オリジナル画質 ("original") 指定時のみ、ライブストリームへ書き込んだ TS データ量からバッファリング完了を判定する。
	if !w.isOriginalQuality {
		return
	}
	status := w.liveStream.GetStatus()
	if status.Status != "Standby" {
		return
	}
	w.originalQualityBytesWritten += chunkSize
	if w.originalQualityBytesWritten < originalQualityONAirBufferBytes {
		if status.Detail != "ストリーミングを開始しています…" {
			w.liveStream.SetStatus("Standby", "ストリーミングを開始しています…", false)
		}
	} else {
		w.liveStream.SetStatus("ONAir", "ライブストリームは ONAir です。", false)
		if *w.retryCount > 0 {
			*w.retryCount = 0
		}
	}
}

// runningProcess は実行中の子プロセスを表す。
type runningProcess struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  io.ReadCloser
	// finished はプロセスが終了したかどうか。
	finished atomic.Bool
	// waitOnce は Wait() を1回だけ実行するためのもの。
	waitOnce sync.Once
}

// startProcess は子プロセスを起動する。
// withStdin が true の場合は標準入力のパイプを作成し、withStdout が true の場合は標準出力のパイプを作成する。
// discardStderr が true の場合は標準エラー出力を破棄する (読み取らないパイプを作らないため) 。
func startProcess(path string, args []string, withStdin bool, withStdout bool, discardStderr bool) (*runningProcess, io.ReadCloser, error) {
	command := exec.Command(path, args...)
	process := &runningProcess{command: command}
	if discardStderr {
		command.Stderr = io.Discard
	}
	if withStdin {
		stdin, err := command.StdinPipe()
		if err != nil {
			return nil, nil, err
		}
		process.stdin = stdin
	}
	var stdout io.ReadCloser
	if withStdout {
		pipe, err := command.StdoutPipe()
		if err != nil {
			return nil, nil, err
		}
		stdout = pipe
		process.stdout = pipe
	}
	if !discardStderr {
		stderr, err := command.StderrPipe()
		if err != nil {
			return nil, nil, err
		}
		process.stderr = stderr
	}
	if err := command.Start(); err != nil {
		return nil, nil, err
	}
	go func() {
		_ = command.Wait()
		process.finished.Store(true)
	}()
	return process, stdout, nil
}

// startEncoderProcess はエンコーダープロセスを起動する (標準入力に tsreadex の出力を接続する) 。
func startEncoderProcess(path string, args []string, stdin io.Reader) (*runningProcess, error) {
	command := exec.Command(path, args...)
	command.Stdin = stdin
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return nil, err
	}
	process := &runningProcess{command: command, stdout: stdout, stderr: stderr}
	if err := command.Start(); err != nil {
		return nil, err
	}
	go func() {
		_ = command.Wait()
		process.finished.Store(true)
	}()
	return process, nil
}

// newPassthroughProcess はオリジナル画質向けのパススルーを表す疑似プロセスを生成する。
// Python 版は標準入力をそのまま標準出力に流すだけのプロセスを起動するが、
// Go 版では tsreadex の出力を直接読み取るため、プロセスを起動しない。
func newPassthroughProcess(stdout io.ReadCloser) *runningProcess {
	return &runningProcess{stdout: stdout, stderr: io.NopCloser(strings.NewReader(""))}
}

// exited はプロセスが終了したかどうかを返す。
func (p *runningProcess) exited() bool {
	return p.finished.Load()
}

// kill はプロセスを終了する。
func (p *runningProcess) kill() {
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	if p.command != nil && p.command.Process != nil {
		_ = p.command.Process.Kill()
	}
}

// waitWithTimeout は WaitGroup の完了を指定されたタイムアウトまで待つ。
func waitWithTimeout(waitGroup *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		waitGroup.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}
