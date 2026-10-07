package videostream

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/stream"
)

// 録画視聴セッション (移植元: server/app/streams/VideoStream.py の VideoStream クラス) 。
const (
	// DefaultSessionTimeout は録画視聴セッションが再生されていない場合に自動破棄されるまでの時間 (VideoStream.SESSION_TIMEOUT) 。
	DefaultSessionTimeout = 10 * time.Second
	// MaxReadedSegments は一度でも読み取られた HLS セグメントの最大保持数 (VideoStream.MAX_READED_SEGMENTS) 。
	MaxReadedSegments = 10
	// DTSWrapAvoidanceSeconds は QSVEncC で 33bit ラップ直前を避けるための余裕 (VideoStream.DTS_WRAP_AVOIDANCE_SECONDS) 。
	DTSWrapAvoidanceSeconds = 60
	// DTSWrapAvoidanceMaxBacktrackSegments は DTS ラップ回避で遡る最大セグメント数。
	DTSWrapAvoidanceMaxBacktrackSegments = 40
	// SegmentMapSaveBatchSize は segment_map を DB へ保存する最小件数 (VideoStream.SEGMENT_MAP_SAVE_BATCH_SIZE) 。
	SegmentMapSaveBatchSize = 16
	// encoderShutdownTimeout は旧エンコードタスクの終了待機時間 (VideoStream.__cancelVideoEncodingTask の timeout_seconds) 。
	encoderShutdownTimeout = 500 * time.Millisecond
)

// Python 側の例外に対応するエラー。API 層でステータスコードを決める。
var (
	// ErrSessionNotExist は未知の session_id が指定された (Python: HTTPException 422 'Session does not exist') 。
	ErrSessionNotExist = errors.New("Session does not exist")
	// ErrSessionProgramMismatch は既存セッションと録画番組 ID / 画質が一致しない。
	ErrSessionProgramMismatch = errors.New("Session exists but program_id or quality mismatch")
	// ErrSessionOptionsMismatch は既存セッションとエンコードオプションが一致しない。
	ErrSessionOptionsMismatch = errors.New("Session exists but encoding options mismatch")
	// ErrValue は Python の ValueError 相当 (副音声抽出失敗など) 。API では 422 になる。
	ErrValue = errors.New("value error")
	// ErrRuntime は Python の RuntimeError 相当 (空データ・入力位置解決失敗など) 。API では 500 になる。
	ErrRuntime = errors.New("runtime error")
	// ErrEncoderUnavailable はエンコーダー (SegmentEncoderFactory) が未登録。ErrRuntime の一種として 500 になる。
	ErrEncoderUnavailable = fmt.Errorf("%w: segment encoder is not registered", ErrRuntime)
)

// SegmentEncoder は 1 つの録画視聴セッションのエンコードタスク (移植元: VideoEncodingTask) 。実装は別担当。
//
// 契約:
//   - Session は Run を呼ぶ前に、全セグメントのうち Pending 以外または結果確定済みのものを Pending へ戻し、
//     startSequence のセグメントを Encoding にしている (Run 側で ResetState してはいけない) 。
//   - Run はセグメントを完成させるたびに Segment.SetEncoded を呼び、次のセグメントへ進むとき Segment.MarkEncoding を呼ぶ。
//   - Cancel は Run を中断させる (複数回呼ばれてよい) 。ctx もキャンセルされる。
//   - Run が ctx キャンセル以外で戻った時点で Encoding のままのセグメントは、Session が空データで失敗扱い (HTTP 500) にする。
type SegmentEncoder interface {
	Run(ctx context.Context, startSequence int) error
	Cancel()
}

// SegmentEncoderFactory はセッションごとに新しい SegmentEncoder を作る (エンコードタスクは使い回さない) 。
type SegmentEncoderFactory func(session *Session) SegmentEncoder

// SegmentStatus は HLS セグメントのエンコード状態。
type SegmentStatus int

const (
	// SegmentPending はまだエンコードされていない。
	SegmentPending SegmentStatus = iota
	// SegmentEncoding はエンコード中。
	SegmentEncoding
	// SegmentCompleted はエンコード完了。
	SegmentCompleted
)

// String は Python 版の encode_status 文字列を返す。
func (s SegmentStatus) String() string {
	switch s {
	case SegmentEncoding:
		return "Encoding"
	case SegmentCompleted:
		return "Completed"
	default:
		return "Pending"
	}
}

// segmentFuture は asyncio.Future[bytes] 相当 (1 回だけ確定する。空データは取得中断を表す) 。
type segmentFuture struct {
	done     chan struct{}
	data     []byte
	resolved bool
}

func newSegmentFuture() *segmentFuture { return &segmentFuture{done: make(chan struct{})} }

// resolve は未確定のときだけ結果を確定する (呼び出し側が Session.mu を保持していること) 。
func (f *segmentFuture) resolve(data []byte) {
	if f.resolved {
		return
	}
	f.resolved = true
	f.data = data
	close(f.done)
}

// Segment は HLS セグメントを表す (移植元: VideoStreamSegment) 。可変フィールドは Session.mu で保護する。
type Segment struct {
	session *Session
	// SequenceIndex は HLS セグメントのシーケンス番号 (0 始まり、Segments() のインデックスと一致) 。
	SequenceIndex int
	// PlaylistStartSeconds は HLS プレイリスト上の開始時刻 (秒) 。
	PlaylistStartSeconds float64
	// DurationSeconds は HLS セグメント長 (秒) 。
	DurationSeconds float64

	sourceFilePosition *int64
	sourceStartDTS     *int64
	status             SegmentStatus
	future             *segmentFuture
	readed             bool
}

// Status は現在のエンコード状態を返す。
func (g *Segment) Status() SegmentStatus {
	g.session.mu.RLock()
	defer g.session.mu.RUnlock()
	return g.status
}

// SourceStartDTS はエンコードを開始する入力ソース側の DTS (90kHz) を返す。未解決なら ok=false 。
func (g *Segment) SourceStartDTS() (dts int64, ok bool) {
	g.session.mu.RLock()
	defer g.session.mu.RUnlock()
	if g.sourceStartDTS == nil {
		return 0, false
	}
	return *g.sourceStartDTS, true
}

// SourceFilePosition はエンコードを開始する入力ファイルのバイト位置を返す。MP4 や未解決の場合は ok=false 。
func (g *Segment) SourceFilePosition() (position int64, ok bool) {
	g.session.mu.RLock()
	defer g.session.mu.RUnlock()
	if g.sourceFilePosition == nil {
		return 0, false
	}
	return *g.sourceFilePosition, true
}

// SetEncoded はエンコード済み MPEG-TS データを確定し、状態を Completed にする。
// 結果が既に確定済み (取得中断など) のときはデータを変更せず状態だけ Completed にする (Python と同じ) 。
// data は呼び出し後に書き換えないこと。
func (g *Segment) SetEncoded(data []byte) {
	g.session.mu.Lock()
	defer g.session.mu.Unlock()
	g.future.resolve(data)
	g.status = SegmentCompleted
}

// MarkEncoding は状態を Encoding にする。
func (g *Segment) MarkEncoding() {
	g.session.mu.Lock()
	defer g.session.mu.Unlock()
	g.status = SegmentEncoding
}

// MarkPending は状態を Pending にする (結果チャネルは変更しない。再初期化は ResetState) 。
func (g *Segment) MarkPending() {
	g.session.mu.Lock()
	defer g.session.mu.Unlock()
	g.status = SegmentPending
}

// ResetState はセグメントを初期状態へ戻す (VideoStreamSegment.resetState) 。
// 未確定の結果は空データで確定して待機者を解放し、新しい結果チャネルへ交換する。
func (g *Segment) ResetState() {
	g.session.mu.Lock()
	defer g.session.mu.Unlock()
	g.resetStateLocked()
}

func (g *Segment) resetStateLocked() {
	g.future.resolve([]byte{})
	g.status = SegmentPending
	g.future = newSegmentFuture()
	g.readed = false
}

// encodingTask は実行中の SegmentEncoder とその制御情報。
type encodingTask struct {
	encoder SegmentEncoder
	cancel  context.CancelFunc
	done    chan struct{}
}

// Session は録画視聴セッション (移植元: VideoStream) 。
type Session struct {
	manager *Manager
	logger  *slog.Logger

	// SessionID はセッション ID 。
	SessionID string
	// Program は録画番組・録画ファイル・チャンネルの情報。
	Program *database.RecordedProgramDetail
	// Quality はベース画質 (例: 1080p-hevc) 。
	quality string
	// EncodingOptions はベース画質に追加するエンコードオプション。
	encodingOptions stream.StreamEncodingOptions
	// encoderName は config.general.encoder (QSVEncC 判定用) 。
	encoderName string
	writeDB     *sql.DB

	segmentDurationSeconds float64

	mu                 sync.RWMutex
	segments           []*Segment
	segmentMapBySeq    map[int]SegmentMapEntry
	tsStreamInfo       *StreamInfo
	tsSourceBaseDTS    *int64
	mp4KeyFrameDTSList []int64
	mp4Loaded          bool
	destroyed          bool
	destroyTimer       *time.Timer
	currentTask        *encodingTask
	collectedKeyFrames []KeyFrame
	lastFlushedCount   int

	// sourceMu は入力ソース位置解決を直列化する (Python: _source_position_lock) 。
	sourceMu sync.Mutex
	// taskMu はエンコードタスクの停止と起動を直列化する (Python: _video_encoding_task_lock) 。
	taskMu sync.Mutex

	// テスト差し替え用 (既定は seeker.go / mp4.go の実装) 。
	findStreamInfoFn func(path string) (*StreamInfo, error)
	findBaseDTSFn    func(path string, info *StreamInfo) (int64, error)
	seekFn           func(path string, info *StreamInfo, playlistStart float64, baseDTS int64, maxAge int64) (*KeyFramePosition, error)
	readMP4DTSFn     func(path string) ([]int64, error)
}

// Quality はベース画質を返す。
func (s *Session) Quality() string { return s.quality }

// EncodingOptions は追加エンコードオプションを返す。
func (s *Session) EncodingOptions() stream.StreamEncodingOptions { return s.encodingOptions }

// Encoder は config.general.encoder の値を返す。
func (s *Session) Encoder() string { return s.encoderName }

// SegmentDurationSeconds は HLS セグメントの基準長 (秒) を返す。
func (s *Session) SegmentDurationSeconds() float64 { return s.segmentDurationSeconds }

// LogPrefix はログの接頭辞 (Python: log_prefix) 。
func (s *Session) LogPrefix() string {
	return fmt.Sprintf("[Video: %d/%s/%s%s]", s.Program.Program.ID, s.SessionID, s.quality, s.encodingOptions.BuildSuffix())
}

// Segments は HLS セグメントの一覧を返す (スライスはコピー。要素は共有) 。破棄後は空。
func (s *Session) Segments() []*Segment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]*Segment(nil), s.segments...)
}

// IsDestroyed は破棄済みかどうかを返す。
func (s *Session) IsDestroyed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.destroyed
}

// TSStreamInfo は MPEG-TS のオンデマンド探索で得たストリーム情報を返す (未解決なら nil) 。
func (s *Session) TSStreamInfo() *StreamInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tsStreamInfo
}

// TSSourceBaseDTS は MPEG-TS の先頭キーフレーム DTS を返す (未解決なら ok=false) 。
func (s *Session) TSSourceBaseDTS() (dts int64, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.tsSourceBaseDTS == nil {
		return 0, false
	}
	return *s.tsSourceBaseDTS, true
}

// KeepAlive は録画視聴セッションのアクティブ状態を維持する (Python: keepAlive) 。
func (s *Session) KeepAlive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.destroyed {
		return
	}
	s.armDestroyTimerLocked()
}

func (s *Session) armDestroyTimerLocked() {
	if s.destroyTimer != nil {
		s.destroyTimer.Stop()
	}
	timeout := s.manager.sessionTimeout()
	s.destroyTimer = time.AfterFunc(timeout, func() { s.Destroy() })
}

// GetBufferRange はエンコード完了済みセグメントのバッファ範囲 (秒) を返す。無ければ (0, 0) 。
func (s *Session) GetBufferRange() (begin float64, end float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var first, last *Segment
	for _, segment := range s.segments {
		if segment.status == SegmentCompleted {
			if first == nil {
				first = segment
			}
			last = segment
		}
	}
	if first == nil {
		return 0, 0
	}
	return first.PlaylistStartSeconds, last.PlaylistStartSeconds + last.DurationSeconds
}

// GetVirtualPlaylist は仮想 HLS M3U8 プレイリストを返す (Python: getVirtualPlaylist) 。
// cacheKey が空のときは乱数 (uuid4 hex の先頭 8 桁) を使う。audio は "primary" / "secondary" 。
func (s *Session) GetVirtualPlaylist(cacheKey string, audio string) string {
	s.KeepAlive()

	video := s.Program.Video
	s.mu.Lock()
	if len(s.segments) == 0 {
		count := max(1, int(math.Ceil(video.Duration/s.segmentDurationSeconds)))
		for sequence := range count {
			start := float64(sequence) * s.segmentDurationSeconds
			remaining := video.Duration - start
			s.segments = append(s.segments, &Segment{
				session:              s,
				SequenceIndex:        sequence,
				PlaylistStartSeconds: start,
				DurationSeconds:      math.Min(s.segmentDurationSeconds, math.Max(remaining, 0.001)),
				future:               newSegmentFuture(),
			})
		}
		s.logger.Info(fmt.Sprintf("%s Total %d virtual segments (segment_duration: %.6fs).", s.LogPrefix(), len(s.segments), s.segmentDurationSeconds))
	}
	s.mu.Unlock()

	if cacheKey == "" {
		random := make([]byte, 16)
		_, _ = rand.Read(random)
		cacheKey = hex.EncodeToString(random)[:8]
	}
	return BuildVirtualPlaylist(s.SessionID, cacheKey, video.VideoFrameRate, video.Duration, audio)
}

// Destroy は実行中のエンコードを終了し、セッションを破棄する (Python: destroy) 。
func (s *Session) Destroy() {
	s.mu.Lock()
	if s.destroyed {
		s.mu.Unlock()
		return
	}
	s.destroyed = true
	if s.destroyTimer != nil {
		s.destroyTimer.Stop()
	}
	s.mu.Unlock()

	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if !s.manager.isRegistered(s) {
		return
	}
	s.cancelTask(true)

	// セグメントの完成を待つリクエストも、セッション終了に伴う取得中断として解放する
	s.mu.Lock()
	for _, segment := range s.segments {
		segment.future.resolve([]byte{})
	}
	s.segments = nil
	s.mu.Unlock()
	s.manager.unregister(s)
	s.logger.Info(s.LogPrefix() + " Streaming Session Finished.")
}

// cancelTask は現在のエンコードタスクをキャンセルする。wait が true のときは短時間だけ終了を待つ。taskMu 保持前提。
func (s *Session) cancelTask(wait bool) {
	s.mu.Lock()
	task := s.currentTask
	s.currentTask = nil
	s.mu.Unlock()
	if task == nil {
		return
	}
	task.encoder.Cancel()
	task.cancel()
	if !wait {
		return
	}
	select {
	case <-task.done:
	case <-time.After(encoderShutdownTimeout):
		s.logger.Warn(s.LogPrefix() + " Encoding task shutdown timed out. Proceeding with restart.")
	}
}

// Manager は session_id をキーにした録画視聴セッションの Singleton 辞書 (Python: VideoStream.__instances) 。
type Manager struct {
	logger *slog.Logger

	// SessionTimeout は自動破棄までの時間 (0 なら DefaultSessionTimeout) 。テストで短縮できる。
	SessionTimeout time.Duration

	mu       sync.Mutex
	sessions map[string]*Session
	factory  SegmentEncoderFactory
}

// NewManager は Manager を生成する。
func NewManager(logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{logger: logger, sessions: map[string]*Session{}}
}

// SetEncoderFactory はエンコーダーの工場を登録する (未登録なら GetSegment は ErrEncoderUnavailable を返す) 。
func (m *Manager) SetEncoderFactory(factory SegmentEncoderFactory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.factory = factory
}

func (m *Manager) encoderFactory() SegmentEncoderFactory {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.factory
}

func (m *Manager) sessionTimeout() time.Duration {
	if m.SessionTimeout > 0 {
		return m.SessionTimeout
	}
	return DefaultSessionTimeout
}

func (m *Manager) isRegistered(s *Session) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[s.SessionID] == s
}

func (m *Manager) unregister(s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[s.SessionID] == s {
		delete(m.sessions, s.SessionID)
	}
}

// Count は現在のセッション数を返す。
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Lookup は登録済みのセッションを返す (無ければ nil) 。
func (m *Manager) Lookup(sessionID string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[sessionID]
}

// SessionParams は Manager.Get の引数。
type SessionParams struct {
	SessionID string
	// Program は録画番組情報 (録画ファイル情報を含む) 。
	Program *database.RecordedProgramDetail
	// SegmentMap は DB に保存済みの segment_map (新規作成時のみ使われる) 。
	SegmentMap      []SegmentMapEntry
	Quality         string
	EncodingOptions stream.StreamEncodingOptions
	// Encoder は config.general.encoder の値 (FFmpeg / QSVEncC など) 。
	Encoder string
	// WriteDB は segment_map の保存に使う書き込み用 DB 。nil なら保存しない。
	WriteDB *sql.DB
}

// Get はセッションを取得する。無い場合は allowNew のときだけ新規作成する (Python: VideoStream.__new__) 。
func (m *Manager) Get(params SessionParams, allowNew bool) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	suffix := params.EncodingOptions.BuildSuffix()
	existing, ok := m.sessions[params.SessionID]
	if !ok {
		if !allowNew {
			m.logger.Error(fmt.Sprintf("[Video: %d/%s/%s%s] Session does not exist.", params.Program.Program.ID, params.SessionID, params.Quality, suffix))
			return nil, ErrSessionNotExist
		}
		s := &Session{
			manager:                m,
			logger:                 m.logger,
			SessionID:              params.SessionID,
			Program:                params.Program,
			quality:                params.Quality,
			encodingOptions:        params.EncodingOptions,
			encoderName:            params.Encoder,
			writeDB:                params.WriteDB,
			segmentDurationSeconds: ComputeSegmentDurationSeconds(params.Program.Video.VideoFrameRate),
			segmentMapBySeq:        map[int]SegmentMapEntry{},
			findStreamInfoFn:       func(path string) (*StreamInfo, error) { return FindStreamInfo(path, 0, 0) },
			findBaseDTSFn:          FindBaseDTS,
			seekFn:                 Seek,
			readMP4DTSFn:           ReadVideoKeyFrameDTS,
		}
		for _, entry := range params.SegmentMap {
			s.segmentMapBySeq[entry.SequenceIndex] = entry
		}
		s.mu.Lock()
		s.armDestroyTimerLocked()
		s.mu.Unlock()
		m.sessions[params.SessionID] = s
		m.logger.Info(s.LogPrefix() + " Streaming Session Started.")
		return s, nil
	}

	if existing.Program.Program.ID != params.Program.Program.ID || existing.quality != params.Quality {
		m.logger.Error(fmt.Sprintf("%s Session exists but program_id or quality mismatch. [program_id: %d, quality: %s%s]",
			existing.LogPrefix(), params.Program.Program.ID, params.Quality, suffix))
		return nil, ErrSessionProgramMismatch
	}
	if existing.encodingOptions != params.EncodingOptions {
		m.logger.Error(fmt.Sprintf("%s Session exists but encoding options mismatch. [is_hevc_10bit_enabled: %v, is_24fps_mode_enabled: %v]",
			existing.LogPrefix(), params.EncodingOptions.IsHEVC10bitEnabled, params.EncodingOptions.Is24fpsModeEnabled))
		return nil, ErrSessionOptionsMismatch
	}
	return existing, nil
}
