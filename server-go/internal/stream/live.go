package stream

import (
	"crypto/rand"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LiveStreamStatus はライブストリームの状態 (server/app/schemas.py の LiveStreamStatus と一致させること) 。
type LiveStreamStatus struct {
	// Status は Offline / Standby / ONAir / Idling / Restart のいずれか。
	Status string `json:"status"`
	// Detail はステータスの詳細情報。
	Detail string `json:"detail"`
	// StartedAt はライブストリームが開始された (ステータスが Offline or Restart → Standby に移行した) 時刻。
	StartedAt float64 `json:"started_at"`
	// UpdatedAt はライブストリームのステータスが最後に更新された時刻。
	UpdatedAt float64 `json:"updated_at"`
	// ClientCount はライブストリームに接続中のクライアント数。
	ClientCount int `json:"client_count"`
}

// LiveStreamClient はライブストリームのクライアントを表す。
type LiveStreamClient struct {
	// ID はクライアント ID。
	ID string
	// ClientType はクライアントの種別 (現在は mpegts のみ) 。
	ClientType string
	// queue はストリームデータが入るキュー (nil が書き込まれるとエンコードタスクの終了を表す) 。
	queue chan []byte
	// closed は既に切断済みかどうか。
	closed bool
	// mu は streamDataReadAt を保護する。
	mu sync.Mutex
	// streamDataReadAt はストリームデータの最終読み取り時刻 (Unix 時間) 。
	streamDataReadAt float64
}

// StreamDataReadAt はストリームデータの最終読み取り時刻を返す。
func (c *LiveStreamClient) StreamDataReadAt() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streamDataReadAt
}

// ReadStreamData は自分自身のキューからストリームデータを読み取って返す。
// エンコードタスクが終了した場合は ok に false を返す。
func (c *LiveStreamClient) ReadStreamData() ([]byte, bool) {
	if c.ClientType != "mpegts" {
		return nil, false
	}
	c.mu.Lock()
	c.streamDataReadAt = float64(time.Now().UnixNano()) / 1e9
	c.mu.Unlock()
	data, ok := <-c.queue
	if !ok {
		return nil, false
	}
	return data, true
}

// Queue はストリームデータが入るキューを返す (エンコードタスクが終了すると閉じられる) 。
func (c *LiveStreamClient) Queue() <-chan []byte {
	return c.queue
}

// writeStreamData は自分自身のキューにストリームデータを書き込む。
func (c *LiveStreamClient) writeStreamData(data []byte) {
	if c.ClientType != "mpegts" {
		return
	}
	select {
	case c.queue <- data:
	default:
		// キューが満杯のクライアントは読み取りが追いついていないため、切断扱いにする
		// (Python 版は無制限の Queue を使うが、Go 版ではメモリを有限に保つためバッファ上限を設けている) 。
		close(c.queue)
		c.closed = true
	}
}

// closeQueue はキューを閉じてエンコードタスクの終了を通知する。
func (c *LiveStreamClient) closeQueue() {
	if c.closed {
		return
	}
	close(c.queue)
	c.closed = true
}

// LiveStream はライブストリームを管理する (server/app/streams/LiveStream.py の LiveStream 相当) 。
// ライブストリーム ID ごとに1つのインスタンスになる。
type LiveStream struct {
	// ID はライブストリーム ID (チャンネル ID-画質+エンコードオプション) 。
	ID string
	// DisplayChannelID はチャンネル ID。
	DisplayChannelID string
	// Quality は映像の品質。
	Quality string
	// EncodingOptions はベース画質に追加するエンコードオプション。
	EncodingOptions StreamEncodingOptions

	// manager はこのインスタンスを所有するマネージャー。
	manager *Manager

	// mu はクライアント一覧とステータスを保護する。
	mu sync.Mutex
	// clients は接続中のクライアント。
	clients []*LiveStreamClient
	// status は Offline / Standby / ONAir / Idling / Restart のいずれか。
	status string
	// detail はステータスの詳細。
	detail string
	// startedAt はストリームの開始時刻。
	startedAt float64
	// updatedAt はステータスの最終更新時刻。
	updatedAt float64
	// streamDataWrittenAt はストリームデータの最終書き込み時刻。
	streamDataWrittenAt float64
	// taskCancel は実行中のエンコードタスクを停止する関数。
	taskCancel func()
	// taskDone は実行中のエンコードタスクの完了を待つチャネル。
	taskDone chan struct{}
	// psiDataArchiver は PSI/SI データアーカイバー (IPTV では常に nil) 。
	psiDataArchiver *LivePSIDataArchiver
}

// LogPrefix はログのプレフィックスを返す。
func (s *LiveStream) LogPrefix() string {
	return "[Live: " + s.ID + "]"
}

// GetStatus はライブストリームのステータスを取得する。
func (s *LiveStream) GetStatus() LiveStreamStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return LiveStreamStatus{
		Status:      s.status,
		Detail:      s.detail,
		StartedAt:   s.startedAt,
		UpdatedAt:   s.updatedAt,
		ClientCount: len(s.clients),
	}
}

// GetStreamDataWrittenAt はストリームデータの最終書き込み時刻を取得する。
func (s *LiveStream) GetStreamDataWrittenAt() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamDataWrittenAt
}

// SetStatus はライブストリームのステータスを設定する。
// ステータスが更新された場合は true を返す。
// 移植元: LiveStream.setStatus()
func (s *LiveStream) SetStatus(status string, detail string, quiet bool) bool {
	s.mu.Lock()

	// ステータスも詳細も現在の状態と重複しているなら、更新を行わない (同じ内容のイベントが複数発生するのを防ぐ) 。
	if s.status == status && s.detail == detail {
		s.mu.Unlock()
		return false
	}

	// ステータスが Offline or Restart かつ現在の状態と重複している場合は、更新を行わない。
	if (status == "Offline" || status == "Restart") && status == s.status {
		s.mu.Unlock()
		return false
	}

	// ステータスは Offline から Restart に移行してはならない。
	if s.status == "Offline" && status == "Restart" {
		s.mu.Unlock()
		return false
	}

	// ストリーム開始 (Offline or Restart → Standby) 時、started_at と stream_data_written_at を更新する。
	// 同一呼出し内の時刻は一回だけ取得する。Standby 移行時に started_at と updated_at が
	// 時計の分解能 (Windows は粗く Linux は ns) によって食い違わないようにする。
	now := float64(time.Now().UnixNano()) / 1e9
	if (s.status == "Offline" || s.status == "Restart") && status == "Standby" {
		s.startedAt = now
		s.streamDataWrittenAt = now
	}

	// ストリーム起動完了時 (Standby → ONAir) 時のみ、ストリームの起動にかかった時間も出力する。
	startupComplete := s.status == "Standby" && status == "ONAir"
	startupElapsed := now - s.startedAt

	s.status = status
	s.detail = detail
	s.updatedAt = now
	s.mu.Unlock()

	if !quiet {
		s.manager.logger.Info(s.LogPrefix()+" [Status: "+status+"] "+detail, "live_stream_id", s.ID)
	}
	if startupComplete {
		s.manager.logger.Info(s.LogPrefix()+" Startup complete. ("+formatSeconds(startupElapsed)+" sec)", "live_stream_id", s.ID)
	}
	return true
}

// formatSeconds は起動時間を Python の round(value, 2) 相当の文字列にする。
func formatSeconds(seconds float64) string {
	rounded := float64(int(seconds*100+0.5)) / 100
	text := strconv.FormatFloat(rounded, 'f', -1, 64)
	// Python の str(float) は整数値でも "1.0" のように小数点以下を表示する。
	if !strings.Contains(text, ".") {
		text += ".0"
	}
	return text
}

// WriteStreamData は接続している全ての mpegts クライアントのキューにストリームデータを書き込む。
// 同時にストリームデータの最終書き込み時刻を更新し、クライアントがタイムアウトしていたら削除する。
// 移植元: LiveStream.writeStreamData()
func (s *LiveStream) WriteStreamData(data []byte) {
	now := float64(time.Now().UnixNano()) / 1e9

	s.mu.Lock()
	remaining := s.clients[:0]
	for _, client := range s.clients {
		// 最終読み取り時刻から 10 秒を過ぎたクライアントはタイムアウトと判断し、クライアントを削除する。
		if now-client.StreamDataReadAt() > 10 {
			client.closeQueue()
			s.manager.logger.Info(s.LogPrefix()+" Client Disconnected (Timeout). Client ID: "+client.ID, "live_stream_id", s.ID)
			continue
		}
		remaining = append(remaining, client)
	}
	s.clients = remaining
	if len(data) > 0 {
		s.streamDataWrittenAt = now
	}
	clients := append([]*LiveStreamClient{}, s.clients...)
	s.mu.Unlock()

	// ロックの外でキューに書き込む (書き込みはブロックしない) 。
	for _, client := range clients {
		client.writeStreamData(data)
	}
}

// Disconnect は指定されたクライアントのライブストリームへの接続を切断する。
// 移植元: LiveStream.disconnect()
func (s *LiveStream) Disconnect(client *LiveStreamClient) {
	s.mu.Lock()
	for i, c := range s.clients {
		if c == client {
			s.clients = append(s.clients[:i], s.clients[i+1:]...)
			s.manager.logger.Info(s.LogPrefix()+" Client Disconnected. Client ID: "+client.ID, "live_stream_id", s.ID)
			break
		}
	}
	s.mu.Unlock()
	client.closeQueue()
}

// DisconnectAll はすべてのクライアントのライブストリームへの接続を切断する。
// 移植元: LiveStream.disconnectAll()
func (s *LiveStream) DisconnectAll() {
	s.mu.Lock()
	clients := s.clients
	s.clients = []*LiveStreamClient{}
	s.mu.Unlock()
	for _, client := range clients {
		client.closeQueue()
	}
}

// SetPSIDataArchiver は PSI/SI データアーカイバーを設定する。
func (s *LiveStream) SetPSIDataArchiver(archiver *LivePSIDataArchiver) {
	s.mu.Lock()
	s.psiDataArchiver = archiver
	s.mu.Unlock()
}

// PSIDataArchiver は PSI/SI データアーカイバーを取得する。
func (s *LiveStream) PSIDataArchiver() *LivePSIDataArchiver {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.psiDataArchiver
}

// SetTask は実行中のエンコードタスクを設定する。
func (s *LiveStream) SetTask(cancel func(), done chan struct{}) {
	s.mu.Lock()
	s.taskCancel = cancel
	s.taskDone = done
	s.mu.Unlock()
}

// TakeTask は実行中のエンコードタスクを取り出し、参照をクリアする。
func (s *LiveStream) TakeTask() (func(), chan struct{}) {
	s.mu.Lock()
	cancel, done := s.taskCancel, s.taskDone
	s.taskCancel, s.taskDone = nil, nil
	s.mu.Unlock()
	return cancel, done
}

// Manager はライブストリームのインスタンスを管理する。
type Manager struct {
	// logger はログ出力に使うロガー。
	logger *slog.Logger

	// mu は streams と order を保護する。
	mu sync.Mutex
	// streams はライブストリーム ID をキーとしたライブストリームの辞書。
	streams map[string]*LiveStream
	// order はライブストリームの生成順 (Python 版の辞書の挿入順と同じ) 。
	order []string

	// startTask はエンコードタスクを起動する関数 (live_task.go で実装) 。
	startTask func(stream *LiveStream)
	// isIPTVChannel は指定されたチャンネルが IPTV の疑似チャンネルかどうかを返す関数。
	isIPTVChannel func(displayChannelID string) bool
	// taskOptions はエンコードタスクの依存関係 (EnableEncodingTasks で設定する) 。
	taskOptions *TaskOptions
}

// ClearTask は実行中のエンコードタスクの参照をクリアする。
func (s *LiveStream) ClearTask() {
	s.mu.Lock()
	s.taskCancel, s.taskDone = nil, nil
	s.mu.Unlock()
}

// SetTaskStarter はエンコードタスクを起動する関数を設定する。
func (m *Manager) SetTaskStarter(startTask func(stream *LiveStream)) {
	m.startTask = startTask
}

// SetIPTVChannelChecker は IPTV の疑似チャンネルかどうかを判定する関数を設定する。
func (m *Manager) SetIPTVChannelChecker(checker func(displayChannelID string) bool) {
	m.isIPTVChannel = checker
}

// NewManager はライブストリームのマネージャーを生成する。
func NewManager(logger *slog.Logger) *Manager {
	return &Manager{
		logger:  logger,
		streams: map[string]*LiveStream{},
	}
}

// GetLiveStream はライブストリームのインスタンスを取得する (存在しない場合は生成する) 。
// 移植元: LiveStream.__new__()
func (m *Manager) GetLiveStream(displayChannelID string, quality string, encodingOptions StreamEncodingOptions) *LiveStream {
	liveStreamID := displayChannelID + "-" + quality + encodingOptions.BuildSuffix()

	m.mu.Lock()
	defer m.mu.Unlock()
	if instance, ok := m.streams[liveStreamID]; ok {
		return instance
	}
	instance := &LiveStream{
		ID:               liveStreamID,
		DisplayChannelID: displayChannelID,
		Quality:          quality,
		EncodingOptions:  encodingOptions,
		manager:          m,
		status:           "Offline",
		detail:           "ライブストリームは Offline です。",
	}
	m.streams[liveStreamID] = instance
	m.order = append(m.order, liveStreamID)
	return instance
}

// GetAllLiveStreams は全てのライブストリームのインスタンスを生成順で取得する。
func (m *Manager) GetAllLiveStreams() []*LiveStream {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*LiveStream, 0, len(m.order))
	for _, id := range m.order {
		result = append(result, m.streams[id])
	}
	return result
}

// GetLiveStreamsByStatus は指定されたステータスのライブストリームのインスタンスを生成順で取得する。
func (m *Manager) GetLiveStreamsByStatus(status string) []*LiveStream {
	result := []*LiveStream{}
	for _, liveStream := range m.GetAllLiveStreams() {
		if liveStream.GetStatus().Status == status {
			result = append(result, liveStream)
		}
	}
	return result
}

// GetViewerCount は指定されたチャンネルのライブストリームの現在の視聴者数を取得する。
// 移植元: LiveStream.getViewerCount()
func (m *Manager) GetViewerCount(displayChannelID string) int {
	viewerCount := 0
	for _, liveStream := range m.GetAllLiveStreams() {
		if liveStream.DisplayChannelID == displayChannelID {
			viewerCount += liveStream.GetStatus().ClientCount
		}
	}
	return viewerCount
}

// GetChannelViewerCounts はチャンネル ID をキーとした視聴者数の一覧を返す。
// 視聴者がいないチャンネルは含まれない。
func (m *Manager) GetChannelViewerCounts() map[string]int {
	counts := map[string]int{}
	for _, liveStream := range m.GetAllLiveStreams() {
		count := liveStream.GetStatus().ClientCount
		if count > 0 {
			counts[liveStream.DisplayChannelID] += count
		}
	}
	return counts
}

// StatusesByStatus はステータスごとに分類されたすべてのライブストリームの状態を返す。
// 移植元: LiveStreamsRouter.LiveStreamsAPI()
func (m *Manager) StatusesByStatus() map[string]map[string]LiveStreamStatus {
	result := map[string]map[string]LiveStreamStatus{
		"Restart": {},
		"Idling":  {},
		"ONAir":   {},
		"Standby": {},
		"Offline": {},
	}
	for _, liveStream := range m.GetAllLiveStreams() {
		status := liveStream.GetStatus()
		if _, ok := result[status.Status]; !ok {
			result[status.Status] = map[string]LiveStreamStatus{}
		}
		result[status.Status][liveStream.ID] = status
	}
	return result
}

// Connect はライブストリームに接続して、新しくライブストリームに登録されたクライアントを返す。
// この時点でライブストリームが Offline ならば、新たにエンコードタスクが起動される。
// 移植元: LiveStream.connect()
func (m *Manager) Connect(liveStream *LiveStream, clientType string) *LiveStreamClient {
	liveStream.mu.Lock()
	currentStatus := liveStream.status
	liveStream.mu.Unlock()

	shouldStartTask := false

	// ライブストリームが Offline な場合、新たにエンコードタスクを起動する。
	if currentStatus == "Offline" {
		// ステータスを Standby に設定する。
		// 現在 Idling 状態のライブストリームを探す前に設定しないと多重にエンコードタスクが起動しかねない。
		liveStream.mu.Lock()
		if liveStream.status == "Offline" {
			liveStream.mu.Unlock()
			liveStream.SetStatus("Standby", "エンコードタスクを起動しています…", false)
			shouldStartTask = true
		} else {
			liveStream.mu.Unlock()
		}
	}

	// チューナーリソースを解放する (IPTV の疑似チャンネルはチューナーを利用しないため対象外) 。
	if shouldStartTask && m.usesTuner(liveStream.DisplayChannelID) {
		m.releaseIdlingLiveStreams()
	}

	// エンコードタスクを非同期で実行する。
	if shouldStartTask && m.startTask != nil {
		m.startTask(liveStream)
	}

	// ライブストリームクライアントのインスタンスを生成・登録する。
	client := &LiveStreamClient{
		ID:               "MPEGTS-" + buildClientID(),
		ClientType:       clientType,
		queue:            make(chan []byte, liveStreamClientQueueSize),
		streamDataReadAt: float64(time.Now().UnixNano()) / 1e9,
	}
	liveStream.mu.Lock()
	liveStream.clients = append(liveStream.clients, client)
	liveStream.mu.Unlock()
	m.logger.Info(liveStream.LogPrefix()+" Client Connected. Client ID: "+client.ID, "live_stream_id", liveStream.ID)

	// ライブストリームが Idling 状態な場合、ONAir 状態に戻す (アイドリングから復帰) 。
	if currentStatus == "Idling" {
		liveStream.SetStatus("ONAir", "ライブストリームは ONAir です。", false)
	}

	return client
}

// usesTuner は指定されたチャンネルがチューナーを利用するかどうかを返す (IPTV の疑似チャンネルは利用しない) 。
func (m *Manager) usesTuner(displayChannelID string) bool {
	if m.isIPTVChannel == nil {
		return true
	}
	return !m.isIPTVChannel(displayChannelID)
}

// releaseIdlingLiveStreams は現在 Idling 状態のライブストリームを Offline にしてチューナーリソースを解放する。
// 移植元: LiveStream.connect() の Mirakurun バックエンド向け処理
func (m *Manager) releaseIdlingLiveStreams() {
	// 画質切り替えなどタイミングの問題で Idling なストリームがない事もあるので、リトライする。
	for attempt := 0; attempt < 15; attempt++ {
		idlingLiveStreams := m.GetLiveStreamsByStatus("Idling")
		if len(idlingLiveStreams) > 0 {
			idlingLiveStreams[0].SetStatus("Offline", "新しいライブストリームが開始されたため、チューナーリソースを解放しました。", false)
			return
		}
		// 現在 ONAir 状態のライブストリームがなければ、リトライしたところで Idling なライブストリームは取得できない。
		if len(m.GetLiveStreamsByStatus("ONAir")) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// liveStreamClientQueueSize はクライアントごとのストリームデータキューのバッファ数。
// 1 チャンクは最大 64KiB で、10〜20 チャンク/秒程度になるため、十分な余裕がある。
const liveStreamClientQueueSize = 512

// hashidsAlphabet は Python 版の hashids のデフォルトのアルファベット。
// Python 版は Hashids(min_length=10).encode(int(time.time() * 1000)) でクライアント ID を生成するが、
// Go 版では同じ長さのランダムな ID を生成する (クライアント ID は API レスポンスに含まれないため) 。
const hashidsAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890"

// buildClientID はクライアント ID のランダム部分 (10文字) を生成する。
func buildClientID() string {
	random := make([]byte, 10)
	if _, err := rand.Read(random); err != nil {
		return strconv.FormatInt(time.Now().UnixMilli(), 36)
	}
	result := make([]byte, 10)
	for i, value := range random {
		result[i] = hashidsAlphabet[int(value)%len(hashidsAlphabet)]
	}
	return string(result)
}
