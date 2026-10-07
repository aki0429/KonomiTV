package videostream

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/stream"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testProgram はテスト用の録画番組情報を作る (29.97fps → セグメント長 6.006 秒) 。
func testProgram(duration float64, container string) *database.RecordedProgramDetail {
	return &database.RecordedProgramDetail{
		Program: database.RecordedProgram{ID: 7},
		Video: database.RecordedVideo{
			ID: 3, RecordedProgramID: 7, FilePath: "dummy.ts", ContainerFormat: container,
			Duration: duration, VideoFrameRate: 29.97,
		},
	}
}

func newTestSession(t *testing.T, manager *Manager, id string, program *database.RecordedProgramDetail, encoder string, writeDB *sql.DB) *Session {
	t.Helper()
	session, err := manager.Get(SessionParams{
		SessionID: id, Program: program, Quality: "1080p", Encoder: encoder, WriteDB: writeDB,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Destroy)
	return session
}

func newSegmentMapDB(t *testing.T, initial string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "t.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE recorded_videos (id INTEGER PRIMARY KEY, segment_map JSON NOT NULL DEFAULT '[]')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO recorded_videos (id, segment_map) VALUES (3, ?)`, initial); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestManagerSingletonAndErrors(t *testing.T) {
	manager := NewManager(testLogger())
	program := testProgram(60, "MPEG-TS")
	params := SessionParams{SessionID: "abc", Program: program, Quality: "1080p"}
	if _, err := manager.Get(params, false); !errors.Is(err, ErrSessionNotExist) {
		t.Fatalf("err = %v, want ErrSessionNotExist", err)
	}
	first, err := manager.Get(params, true)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Destroy()
	second, err := manager.Get(params, false)
	if err != nil || second != first {
		t.Fatalf("Singleton ではありません: %v", err)
	}
	other := params
	other.Quality = "720p"
	if _, err := manager.Get(other, true); !errors.Is(err, ErrSessionProgramMismatch) {
		t.Errorf("err = %v, want ErrSessionProgramMismatch", err)
	}
	other = params
	other.EncodingOptions = stream.StreamEncodingOptions{Is24fpsModeEnabled: true}
	if _, err := manager.Get(other, true); !errors.Is(err, ErrSessionOptionsMismatch) {
		t.Errorf("err = %v, want ErrSessionOptionsMismatch", err)
	}
	if got := first.LogPrefix(); got != "[Video: 7/abc/1080p]" {
		t.Errorf("LogPrefix = %q", got)
	}
}

func TestSessionAutoDestroyAndKeepAlive(t *testing.T) {
	manager := NewManager(testLogger())
	manager.SessionTimeout = 120 * time.Millisecond
	session, err := manager.Get(SessionParams{SessionID: "s", Program: testProgram(60, "MPEG-TS"), Quality: "1080p"}, true)
	if err != nil {
		t.Fatal(err)
	}
	// KeepAlive で期限を延ばし続ける間は破棄されない
	for range 4 {
		time.Sleep(60 * time.Millisecond)
		session.KeepAlive()
	}
	if manager.Count() != 1 || session.IsDestroyed() {
		t.Fatal("KeepAlive 中に破棄されました")
	}
	time.Sleep(400 * time.Millisecond)
	if manager.Count() != 0 || !session.IsDestroyed() {
		t.Fatal("タイムアウト後に破棄されていません")
	}
	// 破棄後の KeepAlive は復活させない
	session.KeepAlive()
	if _, err := manager.Get(SessionParams{SessionID: "s", Program: testProgram(60, "MPEG-TS"), Quality: "1080p"}, false); !errors.Is(err, ErrSessionNotExist) {
		t.Errorf("破棄後に取得できました: %v", err)
	}
}

func TestVirtualPlaylistAndBufferRange(t *testing.T) {
	manager := NewManager(testLogger())
	program := testProgram(20, "MPEG-TS") // 20 / 6.006 → 4 セグメント
	session := newTestSession(t, manager, "pl", program, "FFmpeg", nil)

	if begin, end := session.GetBufferRange(); begin != 0 || end != 0 {
		t.Errorf("初期バッファ範囲 = (%v, %v)", begin, end)
	}
	playlist := session.GetVirtualPlaylist("key12345", "primary")
	want := BuildVirtualPlaylist("pl", "key12345", 29.97, 20, "primary")
	if playlist != want {
		t.Errorf("playlist が BuildVirtualPlaylist と一致しません:\n%s", playlist)
	}
	if got := len(session.Segments()); got != 4 {
		t.Fatalf("セグメント数 = %d, want 4", got)
	}
	if !strings.Contains(session.GetVirtualPlaylist("", "secondary"), "&audio=secondary") {
		t.Error("副音声のクエリがありません")
	}
	// cache_key 未指定時は 8 桁
	generated := session.GetVirtualPlaylist("", "primary")
	line := strings.Split(generated, "\n")[5]
	key := line[strings.LastIndex(line, "cache_key=")+len("cache_key="):]
	if len(key) != 8 {
		t.Errorf("生成された cache_key = %q", key)
	}
	// 最終セグメントは録画時間で切り詰め
	segments := session.Segments()
	if d := segments[3].DurationSeconds; d < 1.98 || d > 1.99 {
		t.Errorf("最終セグメント長 = %v", d)
	}
	// セグメント 1 と 2 のみ Completed → バッファは 1 の開始から 2 の終了まで
	segments[1].SetEncoded([]byte("a"))
	segments[2].SetEncoded([]byte("b"))
	begin, end := session.GetBufferRange()
	if begin != segments[1].PlaylistStartSeconds || end != segments[2].PlaylistStartSeconds+segments[2].DurationSeconds {
		t.Errorf("バッファ範囲 = (%v, %v)", begin, end)
	}
}

// 29.97fps (6.006 秒 = 540540 ticks) のセグメント境界に合わせたキーフレーム列で Python と同じ割り当てになることを検証する。
func TestCreateSegmentMapEntriesFromKeyFrames(t *testing.T) {
	manager := NewManager(testLogger())
	session := newTestSession(t, manager, "km", testProgram(60, "MPEG-TS"), "FFmpeg", nil)
	session.GetVirtualPlaylist("k", "primary")
	const base = int64(1000)
	const ticks = int64(540540)
	keyFrames := []KeyFrame{
		{DTS: base, Offset: 0},
		{DTS: base + ticks, Offset: 100},
		{DTS: base + 2*ticks + 10, Offset: 200},
		{DTS: base + 3*ticks, Offset: 300}, // 最後のキーフレームは終端未確認のため使わない
	}
	// ベース DTS 未解決なら空
	if got := session.CreateSegmentMapEntriesFromKeyFrames(keyFrames); len(got) != 0 {
		t.Fatalf("base DTS 未解決で %d 件", len(got))
	}
	session.setBaseDTSForTest(base)
	if got := session.CreateSegmentMapEntriesFromKeyFrames(keyFrames[:1]); len(got) != 0 {
		t.Fatalf("キーフレーム 1 件で %d 件", len(got))
	}
	got := session.CreateSegmentMapEntriesFromKeyFrames(keyFrames)
	want := []SegmentMapEntry{
		{SequenceIndex: 0, SourceFilePosition: 0, SourceStartDTS: base},
		{SequenceIndex: 1, SourceFilePosition: 100, SourceStartDTS: base + ticks},
	}
	// seg2 は直前キーフレームが 1 セグメント分以上手前、seg3 は最後のキーフレームなので除外される
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("entries = %+v, want %+v", got, want)
	}
	// 既存キャッシュのシーケンスと同一入力位置は除外される
	session.setSegmentMapForTest(SegmentMapEntry{SequenceIndex: 0, SourceFilePosition: 999, SourceStartDTS: 5})
	got = session.CreateSegmentMapEntriesFromKeyFrames(keyFrames)
	if len(got) != 1 || got[0].SequenceIndex != 1 {
		t.Fatalf("既存除外後 entries = %+v", got)
	}
	session.setSegmentMapForTest(SegmentMapEntry{SequenceIndex: 9, SourceFilePosition: 100, SourceStartDTS: base + ticks})
	if got = session.CreateSegmentMapEntriesFromKeyFrames(keyFrames); len(got) != 0 {
		t.Fatalf("同一入力位置の重複が除外されていません: %+v", got)
	}
}

func TestSaveSegmentMapEntriesMerge(t *testing.T) {
	db := newSegmentMapDB(t, `[{"sequence_index":1,"source_file_position":10,"source_start_dts":100}]`)
	manager := NewManager(testLogger())
	session := newTestSession(t, manager, "sv", testProgram(60, "MPEG-TS"), "FFmpeg", db)

	session.SaveSegmentMapEntries(context.Background(), []SegmentMapEntry{
		{SequenceIndex: 1, SourceFilePosition: 11, SourceStartDTS: 111}, // DB に同シーケンスあり → 破棄
		{SequenceIndex: 2, SourceFilePosition: 10, SourceStartDTS: 100}, // 既存と同じ入力位置 → 破棄
		{SequenceIndex: 4, SourceFilePosition: 40, SourceStartDTS: 400},
		{SequenceIndex: 3, SourceFilePosition: 30, SourceStartDTS: 300},
		{SequenceIndex: 3, SourceFilePosition: 31, SourceStartDTS: 301}, // バッチ内重複は先勝ち
	})
	got, err := LoadSegmentMap(context.Background(), db, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []SegmentMapEntry{
		{SequenceIndex: 1, SourceFilePosition: 10, SourceStartDTS: 100},
		{SequenceIndex: 3, SourceFilePosition: 30, SourceStartDTS: 300},
		{SequenceIndex: 4, SourceFilePosition: 40, SourceStartDTS: 400},
	}
	if len(got) != len(want) {
		t.Fatalf("DB = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DB[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// セッション側キャッシュにも反映される
	if cache := session.segmentMapSnapshot(); len(cache) != 3 || cache[3].SourceFilePosition != 30 {
		t.Errorf("セッションキャッシュ = %+v", cache)
	}

	// 保存対象が無いときも別セッション保存分をセッションへ取り込む
	if _, err := db.Exec(`UPDATE recorded_videos SET segment_map = ? WHERE id = 3`,
		`[{"sequence_index":8,"source_file_position":80,"source_start_dts":800}]`); err != nil {
		t.Fatal(err)
	}
	session.SaveSegmentMapEntries(context.Background(), []SegmentMapEntry{{SequenceIndex: 8, SourceFilePosition: 1, SourceStartDTS: 1}})
	if cache := session.segmentMapSnapshot(); len(cache) != 1 || cache[8].SourceFilePosition != 80 {
		t.Errorf("DB 読み直しがキャッシュへ反映されていません: %+v", cache)
	}
	// 存在しない録画ファイルでは例外にならず警告のみ
	missing := newTestSession(t, manager, "sv2", func() *database.RecordedProgramDetail {
		p := testProgram(60, "MPEG-TS")
		p.Video.ID = 99
		return p
	}(), "FFmpeg", db)
	missing.SaveSegmentMapEntries(context.Background(), []SegmentMapEntry{{SequenceIndex: 1}})
}

func TestResolveSegmentSourcePosition(t *testing.T) {
	db := newSegmentMapDB(t, `[]`)
	manager := NewManager(testLogger())
	session := newTestSession(t, manager, "rs", testProgram(60, "MPEG-TS"), "FFmpeg", db)
	session.GetVirtualPlaylist("k", "primary")
	session.setSegmentMapForTest(SegmentMapEntry{SequenceIndex: 2, SourceFilePosition: 222, SourceStartDTS: 2222})

	var seekCalls, infoCalls atomic.Int32
	session.findStreamInfoFn = func(string) (*StreamInfo, error) { infoCalls.Add(1); return &StreamInfo{VideoPID: 256}, nil }
	session.findBaseDTSFn = func(string, *StreamInfo) (int64, error) { return 900000, nil }
	session.seekFn = func(_ string, _ *StreamInfo, start float64, base int64, maxAge int64) (*KeyFramePosition, error) {
		seekCalls.Add(1)
		if base != 900000 || maxAge != 540540 {
			t.Errorf("seek 引数 base=%d maxAge=%d", base, maxAge)
		}
		return &KeyFramePosition{SourceFilePosition: int64(start * 1000), SourceStartDTS: base + int64(start*90000)}, nil
	}

	// segment_map にあればファイル I/O なし
	if err := session.ResolveSegmentSourcePosition(2); err != nil {
		t.Fatal(err)
	}
	seg2 := session.Segments()[2]
	if dts, _ := seg2.SourceStartDTS(); dts != 2222 {
		t.Errorf("seg2 dts = %d", dts)
	}
	if pos, ok := seg2.SourceFilePosition(); !ok || pos != 222 {
		t.Errorf("seg2 pos = %d", pos)
	}
	if seekCalls.Load() != 0 || infoCalls.Load() != 0 {
		t.Error("segment_map ヒット時に探索が走りました")
	}
	// キャッシュ無し → Seek し、DB に保存。2 回目は解決済みなので探索しない
	for range 2 {
		if err := session.ResolveSegmentSourcePosition(3); err != nil {
			t.Fatal(err)
		}
	}
	if seekCalls.Load() != 1 || infoCalls.Load() != 1 {
		t.Errorf("seek=%d info=%d, want 1/1", seekCalls.Load(), infoCalls.Load())
	}
	saved, _ := LoadSegmentMap(context.Background(), db, 3)
	if len(saved) != 1 || saved[0].SequenceIndex != 3 {
		t.Errorf("DB 保存 = %+v", saved)
	}
	// 探索失敗は ErrRuntime
	session.seekFn = func(string, *StreamInfo, float64, int64, int64) (*KeyFramePosition, error) {
		return nil, errors.New("boom")
	}
	if err := session.ResolveSegmentSourcePosition(5); !errors.Is(err, ErrRuntime) {
		t.Errorf("err = %v, want ErrRuntime", err)
	}

	// MP4: キーフレーム DTS 表から (DB は更新しない)
	mp4Manager := NewManager(testLogger())
	mp4 := newTestSession(t, mp4Manager, "mp4", testProgram(60, "MPEG-4"), "FFmpeg", db)
	mp4.GetVirtualPlaylist("k", "primary")
	mp4.readMP4DTSFn = func(string) ([]int64, error) { return []int64{0, 500000, 1000000, 2000000}, nil }
	if err := mp4.ResolveSegmentSourcePosition(2); err != nil { // 12.012 秒 = 1081080 ticks
		t.Fatal(err)
	}
	seg := mp4.Segments()[2]
	if dts, ok := seg.SourceStartDTS(); !ok || dts != 1000000 {
		t.Errorf("mp4 dts = %d ok=%v", dts, ok)
	}
	if _, ok := seg.SourceFilePosition(); ok {
		t.Error("MP4 は source_file_position を持たないはず")
	}
}

// ---- GetSegment ----

type fakeEncoder struct {
	session  *Session
	factory  *fakeFactory
	canceled atomic.Bool
}

type fakeFactory struct {
	mu     sync.Mutex
	starts []int
	run    func(e *fakeEncoder, ctx context.Context, start int) error
	made   []*fakeEncoder
}

func (f *fakeFactory) New(session *Session) SegmentEncoder {
	e := &fakeEncoder{session: session, factory: f}
	f.mu.Lock()
	f.made = append(f.made, e)
	f.mu.Unlock()
	return e
}

func (e *fakeEncoder) Run(ctx context.Context, start int) error {
	e.factory.mu.Lock()
	e.factory.starts = append(e.factory.starts, start)
	e.factory.mu.Unlock()
	return e.factory.run(e, ctx, start)
}
func (e *fakeEncoder) Cancel() { e.canceled.Store(true) }

func (f *fakeFactory) startList() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.starts...)
}

// completeAll は start から順に全セグメントをエンコード済みにするフェイクの Run 。
func completeFrom(delay time.Duration, count int) func(e *fakeEncoder, ctx context.Context, start int) error {
	return func(e *fakeEncoder, ctx context.Context, start int) error {
		segments := e.session.Segments()
		for i := start; i < len(segments) && i < start+count; i++ {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			e.session.CompleteAndAdvance(i, []byte{byte(i), 0xAA})
		}
		return nil
	}
}

func newEncodingSession(t *testing.T, program *database.RecordedProgramDetail, encoder string, factory *fakeFactory) *Session {
	t.Helper()
	manager := NewManager(testLogger())
	manager.SetEncoderFactory(factory.New)
	session := newTestSession(t, manager, "gs", program, encoder, nil)
	session.GetVirtualPlaylist("k", "primary")
	for _, segment := range session.Segments() {
		d := int64(segment.SequenceIndex) * 1000
		segment.sourceStartDTS = &d
	}
	return session
}

func TestGetSegmentInvalidSequenceAndNoEncoder(t *testing.T) {
	manager := NewManager(testLogger())
	session := newTestSession(t, manager, "ne", testProgram(20, "MPEG-TS"), "FFmpeg", nil)
	session.GetVirtualPlaylist("k", "primary")
	for _, sequence := range []int{-1, 4, 100} {
		data, err := session.GetSegment(context.Background(), sequence, "primary")
		if data != nil || err != nil {
			t.Errorf("sequence %d: (%v, %v), want (nil, nil)", sequence, data, err)
		}
	}
	// エンコーダー未登録は ErrRuntime (HTTP 500) の安全な既定
	if _, err := session.GetSegment(context.Background(), 0, "primary"); !errors.Is(err, ErrRuntime) || !errors.Is(err, ErrEncoderUnavailable) {
		t.Errorf("err = %v, want ErrEncoderUnavailable", err)
	}
}

func TestGetSegmentStartsEncoderAndSharesTask(t *testing.T) {
	factory := &fakeFactory{run: completeFrom(30*time.Millisecond, 100)}
	session := newEncodingSession(t, testProgram(30, "MPEG-TS"), "FFmpeg", factory)

	var wg sync.WaitGroup
	results := make([][]byte, 3)
	errs := make([]error, 3)
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = session.GetSegment(context.Background(), i, "primary")
		}()
		// seg1 要求時は seg0 が Encoding 中、seg2 要求時は seg1 が Encoding 中 (Pending のままだとシーク扱いで再起動する)
		time.Sleep(40 * time.Millisecond)
	}
	wg.Wait()
	for i := range 3 {
		if errs[i] != nil || len(results[i]) != 2 || results[i][0] != byte(i) {
			t.Errorf("segment %d = (%v, %v)", i, results[i], errs[i])
		}
	}
	// 隣り合うセグメント要求は同じタスクを共有する (エンコーダーは 1 回だけ起動)
	if starts := factory.startList(); len(starts) != 1 || starts[0] != 0 {
		t.Errorf("starts = %v, want [0]", starts)
	}
}

func TestGetSegmentSeekCancelsPreviousTaskAndResets(t *testing.T) {
	block := make(chan struct{})
	factory := &fakeFactory{}
	factory.run = func(e *fakeEncoder, ctx context.Context, start int) error {
		if start == 0 {
			e.session.CompleteAndAdvance(0, []byte{0, 1})
			<-block // 以降は進まない (シーク待ち)
			return nil
		}
		return completeFrom(0, 1)(e, ctx, start)
	}
	session := newEncodingSession(t, testProgram(60, "MPEG-TS"), "FFmpeg", factory)
	defer close(block)

	if data, err := session.GetSegment(context.Background(), 0, "primary"); err != nil || len(data) != 2 {
		t.Fatalf("seg0 = (%v, %v)", data, err)
	}
	// 次の要求でシーク (seg 6)
	data, err := session.GetSegment(context.Background(), 6, "primary")
	if err != nil || len(data) != 2 || data[0] != 6 {
		t.Fatalf("seg6 = (%v, %v)", data, err)
	}
	if starts := factory.startList(); len(starts) != 2 || starts[1] != 6 {
		t.Errorf("starts = %v, want [0 6]", starts)
	}
	if !factory.made[0].canceled.Load() {
		t.Error("旧エンコーダーが Cancel されていません")
	}
	// 新タスク起動で Pending 以外のセグメントはリセットされる (seg0 は Pending に戻る)
	if got := session.Segments()[0].Status(); got != SegmentPending {
		t.Errorf("seg0 status = %v, want Pending", got)
	}
}

func TestGetSegmentQSVEncCWrapAvoidance(t *testing.T) {
	factory := &fakeFactory{run: completeFrom(0, 1)}
	program := testProgram(120, "MPEG-TS") // 20 セグメント
	session := newEncodingSession(t, program, "QSVEncC", factory)
	// seg 0..9 は 33bit ラップの 10 秒前 (=ラップ回避の 60 秒以内) 、seg 10 以降はラップ後の十分遠い値
	for _, segment := range session.Segments() {
		var dts int64
		if segment.SequenceIndex <= 9 {
			dts = PCRCycle - 900000 + int64(segment.SequenceIndex)*10 // 残り 10 秒前後
		} else {
			dts = int64(segment.SequenceIndex-10) * 540540 // ラップ直後 (距離は十分大きい)
		}
		segment.sourceStartDTS = &dts
	}
	// seg 12 を要求 → seg 12 は余裕あり (distance > 60s) なのでそのまま 12 から
	if _, err := session.GetSegment(context.Background(), 12, "primary"); err != nil {
		t.Fatal(err)
	}
	if starts := factory.startList(); len(starts) != 1 || starts[0] != 12 {
		t.Fatalf("starts = %v, want [12]", starts)
	}
	// seg 5 を要求 → 5, 4, ..., 0 すべてラップ直前 → 0 まで遡る。遡った先 0 から連続してエンコード
	factory.run = func(e *fakeEncoder, ctx context.Context, start int) error {
		for i := start; i <= 5; i++ {
			e.session.CompleteAndAdvance(i, []byte{byte(i), 0})
		}
		return nil
	}
	data, err := session.GetSegment(context.Background(), 5, "primary")
	if err != nil || len(data) != 2 || data[0] != 5 {
		t.Fatalf("seg5 = (%v, %v)", data, err)
	}
	if starts := factory.startList(); len(starts) != 2 || starts[1] != 0 {
		t.Fatalf("starts = %v, want [12 0]", starts)
	}
}

func TestGetSegmentWrapAvoidanceBacktrackLimitAndConditions(t *testing.T) {
	// 全セグメントがラップ直前 → 上限 40 回で打ち切り
	factory := &fakeFactory{run: completeFrom(0, 1)}
	session := newEncodingSession(t, testProgram(400, "MPEG-TS"), "QSVEncC", factory) // 67 セグメント
	for _, segment := range session.Segments() {
		dts := PCRCycle - 10
		segment.sourceStartDTS = &dts
	}
	factory.run = func(e *fakeEncoder, ctx context.Context, start int) error {
		e.session.CompleteAndAdvance(50, []byte{50, 0})
		return nil
	}
	if _, err := session.GetSegment(context.Background(), 50, "primary"); err != nil {
		t.Fatal(err)
	}
	// 50 から 40 回遡った 10 で停止する
	if starts := factory.startList(); len(starts) != 1 || starts[0] != 10 {
		t.Errorf("starts = %v, want [10]", starts)
	}

	// QSVEncC 以外では遡らない
	factory2 := &fakeFactory{run: func(e *fakeEncoder, ctx context.Context, start int) error {
		e.session.CompleteAndAdvance(5, []byte{5, 0})
		return nil
	}}
	s2 := newEncodingSession(t, testProgram(120, "MPEG-TS"), "FFmpeg", factory2)
	for _, segment := range s2.Segments() {
		dts := PCRCycle - 10
		segment.sourceStartDTS = &dts
	}
	if _, err := s2.GetSegment(context.Background(), 5, "primary"); err != nil {
		t.Fatal(err)
	}
	if starts := factory2.startList(); len(starts) != 1 || starts[0] != 5 {
		t.Errorf("FFmpeg starts = %v, want [5]", starts)
	}

	// 映像構成変更ありの録画も遡らない
	factory3 := &fakeFactory{run: factory2.run}
	changed := testProgram(120, "MPEG-TS")
	changed.Video.HasVideoStreamChanges = true
	s3 := newEncodingSession(t, changed, "QSVEncC", factory3)
	for _, segment := range s3.Segments() {
		dts := PCRCycle - 10
		segment.sourceStartDTS = &dts
	}
	if _, err := s3.GetSegment(context.Background(), 5, "primary"); err != nil {
		t.Fatal(err)
	}
	if starts := factory3.startList(); len(starts) != 1 || starts[0] != 5 {
		t.Errorf("変更あり starts = %v, want [5]", starts)
	}
}

func TestGetSegmentEmptyDataAndAborts(t *testing.T) {
	// エンコーダーが異常終了 (空データ) → ErrRuntime
	factory := &fakeFactory{run: func(e *fakeEncoder, ctx context.Context, start int) error {
		return errors.New("encoder crashed")
	}}
	session := newEncodingSession(t, testProgram(30, "MPEG-TS"), "FFmpeg", factory)
	if _, err := session.GetSegment(context.Background(), 1, "primary"); !errors.Is(err, ErrRuntime) {
		t.Errorf("err = %v, want ErrRuntime", err)
	}

	// 待機中に Destroy → (nil, nil)
	started := make(chan struct{})
	factory2 := &fakeFactory{run: func(e *fakeEncoder, ctx context.Context, start int) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	s2 := newEncodingSession(t, testProgram(30, "MPEG-TS"), "FFmpeg", factory2)
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := s2.GetSegment(context.Background(), 0, "primary")
		done <- result{data, err}
	}()
	<-started
	s2.Destroy()
	select {
	case r := <-done:
		if r.data != nil || r.err != nil {
			t.Errorf("Destroy 後 = (%v, %v), want (nil, nil)", r.data, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Destroy で待機が解放されません")
	}
	if !factory2.made[0].canceled.Load() {
		t.Error("Destroy でエンコーダーが Cancel されていません")
	}
	if got := s2.Segments(); len(got) != 0 {
		t.Errorf("破棄後のセグメント数 = %d", len(got))
	}
	// 破棄後の GetSegment は nil
	if data, err := s2.GetSegment(context.Background(), 0, "primary"); data != nil || err != nil {
		t.Errorf("破棄後 GetSegment = (%v, %v)", data, err)
	}

	// 取得側のコンテキストキャンセルはエラー (クライアント切断)
	factory3 := &fakeFactory{run: func(e *fakeEncoder, ctx context.Context, start int) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	s3 := newEncodingSession(t, testProgram(30, "MPEG-TS"), "FFmpeg", factory3)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s3.GetSegment(ctx, 0, "primary"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

func TestGetSegmentSecondaryAudioInvalidIsValueError(t *testing.T) {
	factory := &fakeFactory{run: completeFrom(0, 100)}
	session := newEncodingSession(t, testProgram(30, "MPEG-TS"), "FFmpeg", factory)
	// 188 バイトの倍数でないデータは ExtractSecondaryAudio が失敗 → ErrValue
	if _, err := session.GetSegment(context.Background(), 0, "secondary"); !errors.Is(err, ErrValue) {
		t.Errorf("err = %v, want ErrValue", err)
	}
	// 主音声はそのまま
	if data, err := session.GetSegment(context.Background(), 0, "primary"); err != nil || len(data) != 2 {
		t.Errorf("primary = (%v, %v)", data, err)
	}
}

func TestGetSegmentResetsOldestReadedSegments(t *testing.T) {
	factory := &fakeFactory{run: completeFrom(0, 100)}
	session := newEncodingSession(t, testProgram(120, "MPEG-TS"), "FFmpeg", factory)
	for i := range MaxReadedSegments {
		if _, err := session.GetSegment(context.Background(), i, "primary"); err != nil {
			t.Fatal(err)
		}
	}
	segments := session.Segments()
	// 10 個目を読んだ時点で最も古い seg0 が初期化される
	if got := segments[0].Status(); got != SegmentPending {
		t.Errorf("seg0 status = %v, want Pending (reset)", got)
	}
	if got := segments[1].Status(); got != SegmentCompleted {
		t.Errorf("seg1 status = %v, want Completed", got)
	}
}

func TestAddKeyFramesSavesInBatches(t *testing.T) {
	db := newSegmentMapDB(t, `[]`)
	manager := NewManager(testLogger())
	session := newTestSession(t, manager, "kf", testProgram(200, "MPEG-TS"), "FFmpeg", db)
	session.GetVirtualPlaylist("k", "primary")
	session.setBaseDTSForTest(0)
	const ticks = int64(540540)
	var keyFrames []KeyFrame
	for i := range 20 {
		keyFrames = append(keyFrames, KeyFrame{DTS: int64(i) * ticks, Offset: int64(i) * 100})
	}
	// 順不同・重複で渡しても整列・重複排除される。最初の 10 件では閾値 16 に満たず保存されない
	session.AddKeyFrames(context.Background(), nil)
	session.AddKeyFrames(context.Background(), []KeyFrame{keyFrames[5], keyFrames[3], keyFrames[0], keyFrames[1], keyFrames[2], keyFrames[4], keyFrames[3]})
	if saved, _ := LoadSegmentMap(context.Background(), db, 3); len(saved) != 0 {
		t.Fatalf("閾値未満で保存されました: %+v", saved)
	}
	session.AddKeyFrames(context.Background(), keyFrames[6:])
	saved, _ := LoadSegmentMap(context.Background(), db, 3)
	if len(saved) != 19 { // 最後のキーフレームを除く 19 件
		t.Errorf("保存件数 = %d, want 19", len(saved))
	}
	// 強制フラッシュ (タスク終了時) は閾値を無視する
	db2 := newSegmentMapDB(t, `[]`)
	s2 := newTestSession(t, manager, "kf2", testProgram(200, "MPEG-TS"), "FFmpeg", db2)
	s2.GetVirtualPlaylist("k", "primary")
	s2.setBaseDTSForTest(0)
	s2.AddKeyFrames(context.Background(), keyFrames[:4])
	s2.FlushKeyFrames(context.Background())
	if saved, _ := LoadSegmentMap(context.Background(), db2, 3); len(saved) != 3 {
		t.Errorf("強制保存件数 = %d, want 3", len(saved))
	}
}

// ---- テスト用ヘルパー ----

func (s *Session) setBaseDTSForTest(dts int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tsSourceBaseDTS = &dts
}

func (s *Session) setSegmentMapForTest(entry SegmentMapEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.segmentMapBySeq[entry.SequenceIndex] = entry
}

func (s *Session) segmentMapSnapshot() map[int]SegmentMapEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	copied := make(map[int]SegmentMapEntry, len(s.segmentMapBySeq))
	for key, value := range s.segmentMapBySeq {
		copied[key] = value
	}
	return copied
}
