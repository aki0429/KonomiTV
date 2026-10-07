package videostream

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- segment_map の永続化 (移植元: VideoStream.saveSegmentMapEntries) ----

// segmentMapSaveLocks は recorded_videos.segment_map の読み直し→保存を録画ファイル単位で直列化するロック。
// 使い終えた録画 ID は参照カウントで自動的に辞書から消す (Python: WeakValueDictionary) 。
var segmentMapSaveLocks = struct {
	mu    sync.Mutex
	locks map[int64]*segmentMapLock
}{locks: map[int64]*segmentMapLock{}}

type segmentMapLock struct {
	mu   sync.Mutex
	refs int
}

func lockSegmentMap(videoID int64) func() {
	segmentMapSaveLocks.mu.Lock()
	lock := segmentMapSaveLocks.locks[videoID]
	if lock == nil {
		lock = &segmentMapLock{}
		segmentMapSaveLocks.locks[videoID] = lock
	}
	lock.refs++
	segmentMapSaveLocks.mu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		segmentMapSaveLocks.mu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(segmentMapSaveLocks.locks, videoID)
		}
		segmentMapSaveLocks.mu.Unlock()
	}
}

// segmentMapEntryJSON は DB 上の JSON 表現 (server/app/schemas.py の SegmentMapEntry) 。
type segmentMapEntryJSON struct {
	SequenceIndex      int   `json:"sequence_index"`
	SourceFilePosition int64 `json:"source_file_position"`
	SourceStartDTS     int64 `json:"source_start_dts"`
}

// ParseSegmentMap は recorded_videos.segment_map の JSON 文字列を解析する (空・null は空配列) 。
func ParseSegmentMap(raw string) ([]SegmentMapEntry, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return []SegmentMapEntry{}, nil
	}
	var decoded []segmentMapEntryJSON
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, fmt.Errorf("failed to parse segment_map: %w", err)
	}
	entries := make([]SegmentMapEntry, len(decoded))
	for i, entry := range decoded {
		entries[i] = SegmentMapEntry(entry)
	}
	return entries, nil
}

// MarshalSegmentMap は Python の json.dumps (区切りは ", " と ": ") と同じ形式で segment_map を直列化する。
func MarshalSegmentMap(entries []SegmentMapEntry) string {
	var builder strings.Builder
	builder.WriteByte('[')
	for i, entry := range entries {
		if i > 0 {
			builder.WriteString(", ")
		}
		builder.WriteString(`{"sequence_index": ` + strconv.Itoa(entry.SequenceIndex))
		builder.WriteString(`, "source_file_position": ` + strconv.FormatInt(entry.SourceFilePosition, 10))
		builder.WriteString(`, "source_start_dts": ` + strconv.FormatInt(entry.SourceStartDTS, 10) + `}`)
	}
	builder.WriteByte(']')
	return builder.String()
}

// LoadSegmentMap は録画ファイル ID の segment_map を DB から読み込む。行が無ければ sql.ErrNoRows を返す。
func LoadSegmentMap(ctx context.Context, db *sql.DB, recordedVideoID int64) ([]SegmentMapEntry, error) {
	var raw sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT segment_map FROM recorded_videos WHERE id = ?`, recordedVideoID).Scan(&raw); err != nil {
		return nil, err
	}
	return ParseSegmentMap(raw.String)
}

// MergeSegmentMapEntries は DB の最新 segment_map へ今回の候補をマージする (移植元: saveSegmentMapEntries 内のマージ処理) 。
// 候補はシーケンス番号の重複を先勝ちで畳み、DB に同一シーケンスがあるもの・既存と同じ入力位置のものは捨てる。
// 戻り値はシーケンス番号順の新しい segment_map と、追加できた件数。
func MergeSegmentMapEntries(latest []SegmentMapEntry, candidates []SegmentMapEntry) ([]SegmentMapEntry, int) {
	deduplicated := []SegmentMapEntry{}
	seen := map[int]bool{}
	for _, entry := range candidates {
		if seen[entry.SequenceIndex] {
			continue
		}
		seen[entry.SequenceIndex] = true
		deduplicated = append(deduplicated, entry)
	}

	updated := append([]SegmentMapEntry{}, latest...)
	existingSequences := map[int]bool{}
	type position struct{ file, dts int64 }
	existingPositions := map[position]bool{}
	for _, entry := range updated {
		existingSequences[entry.SequenceIndex] = true
		existingPositions[position{entry.SourceFilePosition, entry.SourceStartDTS}] = true
	}
	saved := 0
	for _, entry := range deduplicated {
		if existingSequences[entry.SequenceIndex] {
			continue
		}
		key := position{entry.SourceFilePosition, entry.SourceStartDTS}
		if existingPositions[key] {
			continue
		}
		updated = append(updated, entry)
		existingSequences[entry.SequenceIndex] = true
		existingPositions[key] = true
		saved++
	}
	if saved > 0 {
		sort.SliceStable(updated, func(i, j int) bool { return updated[i].SequenceIndex < updated[j].SequenceIndex })
	}
	return updated, saved
}

// SaveSegmentMapEntries はオンデマンド探索や再生中解析で得た segment_map を録画レコードへ保存する。
// 保存失敗は再生失敗に直結しないため、警告ログのみで戻る (移植元: saveSegmentMapEntries) 。
func (s *Session) SaveSegmentMapEntries(ctx context.Context, entries []SegmentMapEntry) {
	if len(entries) == 0 || s.writeDB == nil {
		return
	}
	videoID := s.Program.Video.ID
	unlock := lockSegmentMap(videoID)
	defer unlock()

	updated, saved, err := s.mergeAndSave(ctx, videoID, entries)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.logger.Warn(s.LogPrefix() + " RecordedVideo was not found while saving segment map entries.")
			return
		}
		s.logger.Warn(s.LogPrefix()+" Failed to save segment map entries: "+err.Error(), "error", err)
		return
	}
	// 最新値を現在の視聴セッションにも反映し、次回の同じセグメント要求を DB なしで返す
	s.mu.Lock()
	s.segmentMapBySeq = make(map[int]SegmentMapEntry, len(updated))
	for _, entry := range updated {
		s.segmentMapBySeq[entry.SequenceIndex] = entry
	}
	s.mu.Unlock()
	if saved > 0 {
		s.logger.Info(fmt.Sprintf("%s Segment map entries saved. [count: %d]", s.LogPrefix(), saved))
	}
}

// mergeAndSave は 1 トランザクションで DB から読み直し、マージして保存する。
func (s *Session) mergeAndSave(ctx context.Context, videoID int64, entries []SegmentMapEntry) ([]SegmentMapEntry, int, error) {
	transaction, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = transaction.Rollback() }()

	var raw sql.NullString
	if err := transaction.QueryRowContext(ctx, `SELECT segment_map FROM recorded_videos WHERE id = ?`, videoID).Scan(&raw); err != nil {
		return nil, 0, err
	}
	latest, err := ParseSegmentMap(raw.String)
	if err != nil {
		return nil, 0, err
	}
	updated, saved := MergeSegmentMapEntries(latest, entries)
	if saved == 0 {
		// 追加できる候補が無くても、別セッションが保存した値を取り込んでおく (DB は書き換えない)
		return updated, 0, nil
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE recorded_videos SET segment_map = ? WHERE id = ?`, MarshalSegmentMap(updated), videoID); err != nil {
		return nil, 0, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, 0, err
	}
	return updated, saved, nil
}

// ---- キーフレーム収集 (移植元: createSegmentMapEntriesFromKeyFrames と VideoEncodingTask.FlushCollectedSegmentMap) ----

// CreateSegmentMapEntriesFromKeyFrames は再生中に見つかった入力 TS キーフレーム (DTS 昇順) から segment_map 保存候補を作る。
func (s *Session) CreateSegmentMapEntriesFromKeyFrames(keyFrames []KeyFrame) []SegmentMapEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// 最後に見つかったキーフレームは終端未確認のため保存しない
	if len(keyFrames) < 2 || s.tsSourceBaseDTS == nil {
		return []SegmentMapEntry{}
	}
	segmentDurationTicks := int64(math.RoundToEven(s.segmentDurationSeconds * float64(HZ)))
	type position struct{ file, dts int64 }
	used := map[position]bool{}
	for _, entry := range s.segmentMapBySeq {
		used[position{entry.SourceFilePosition, entry.SourceStartDTS}] = true
	}
	entries := []SegmentMapEntry{}
	for _, segment := range s.segments {
		if _, exists := s.segmentMapBySeq[segment.SequenceIndex]; exists {
			continue
		}
		targetDTS := *s.tsSourceBaseDTS + int64(math.RoundToEven(segment.PlaylistStartSeconds*float64(HZ)))
		// bisect_right - 1 : target 以下で最も近いキーフレーム
		index := sort.Search(len(keyFrames), func(i int) bool { return keyFrames[i].DTS > targetDTS }) - 1
		if index < 0 || index >= len(keyFrames)-1 {
			continue
		}
		keyFrame := keyFrames[index]
		age := targetDTS - keyFrame.DTS
		if age < 0 || age >= segmentDurationTicks {
			continue
		}
		key := position{keyFrame.Offset, keyFrame.DTS}
		if used[key] {
			continue
		}
		used[key] = true
		entries = append(entries, SegmentMapEntry{
			SequenceIndex:      segment.SequenceIndex,
			SourceFilePosition: keyFrame.Offset,
			SourceStartDTS:     keyFrame.DTS,
		})
	}
	return entries
}

// AddKeyFrames はエンコーダーが入力 TS から見つけたキーフレームを渡す。
// 収集分を DTS・オフセット順に整列し重複を畳んだうえで segment_map 候補へ変換し、
// 候補が SegmentMapSaveBatchSize 件以上溜まったときだけ DB へ保存する。
func (s *Session) AddKeyFrames(ctx context.Context, keyFrames []KeyFrame) {
	s.mu.Lock()
	s.collectedKeyFrames = append(s.collectedKeyFrames, keyFrames...)
	s.mu.Unlock()
	s.flushKeyFrames(ctx, false)
}

// FlushKeyFrames は閾値を無視して収集済みの候補をすべて保存する (エンコードタスク終了時に呼ぶ) 。
func (s *Session) FlushKeyFrames(ctx context.Context) {
	s.flushKeyFrames(ctx, true)
}

func (s *Session) flushKeyFrames(ctx context.Context, force bool) {
	s.mu.Lock()
	count := len(s.collectedKeyFrames)
	if !force && count == s.lastFlushedCount {
		s.mu.Unlock()
		return
	}
	snapshot := append([]KeyFrame(nil), s.collectedKeyFrames...)
	s.mu.Unlock()

	sort.SliceStable(snapshot, func(i, j int) bool {
		if snapshot[i].DTS != snapshot[j].DTS {
			return snapshot[i].DTS < snapshot[j].DTS
		}
		return snapshot[i].Offset < snapshot[j].Offset
	})
	unique := make([]KeyFrame, 0, len(snapshot))
	for i, keyFrame := range snapshot {
		if i > 0 && keyFrame == snapshot[i-1] {
			continue
		}
		unique = append(unique, keyFrame)
	}
	entries := s.CreateSegmentMapEntriesFromKeyFrames(unique)

	s.mu.Lock()
	s.lastFlushedCount = count
	s.mu.Unlock()
	if len(entries) == 0 || (!force && len(entries) < SegmentMapSaveBatchSize) {
		return
	}
	s.SaveSegmentMapEntries(ctx, entries)
}

// ---- 入力ソース位置の解決 (移植元: ensureTSKeyFrameContext / resolveSegmentSourcePosition) ----

// EnsureTSKeyFrameContext は MPEG-TS のキーフレーム解析に必要なストリーム情報と先頭 DTS を用意する (MP4 では何もしない) 。
func (s *Session) EnsureTSKeyFrameContext() error {
	if s.Program.Video.ContainerFormat != "MPEG-TS" {
		return nil
	}
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	return s.ensureTSContextLocked()
}

// ensureTSContextLocked は sourceMu 保持前提。
func (s *Session) ensureTSContextLocked() error {
	path := s.Program.Video.FilePath
	s.mu.RLock()
	info, baseDTS := s.tsStreamInfo, s.tsSourceBaseDTS
	s.mu.RUnlock()
	if info == nil {
		found, err := s.findStreamInfoFn(path)
		if err != nil {
			return err
		}
		info = found
		s.mu.Lock()
		s.tsStreamInfo = info
		s.mu.Unlock()
	}
	if baseDTS == nil {
		value, err := s.findBaseDTSFn(path, info)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.tsSourceBaseDTS = &value
		s.mu.Unlock()
	}
	return nil
}

func (s *Session) segmentAt(sequence int) (*Segment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if sequence < 0 || sequence >= len(s.segments) {
		return nil, fmt.Errorf("%w: segment %d does not exist", ErrRuntime, sequence)
	}
	return s.segments[sequence], nil
}

// ResolveSegmentSourcePosition は指定セグメントをエンコード開始点として使えるよう、入力ソース側の位置と DTS を解決する。
// 失敗は ErrRuntime (HTTP 500) 。
func (s *Session) ResolveSegmentSourcePosition(sequence int) error {
	start := time.Now()
	segment, err := s.segmentAt(sequence)
	if err != nil {
		return err
	}
	if _, resolved := segment.SourceStartDTS(); resolved {
		return nil
	}

	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	// ロック待ちの間に別リクエストが解決している可能性がある
	if segment, err = s.segmentAt(sequence); err != nil {
		return err
	}
	if _, resolved := segment.SourceStartDTS(); resolved {
		return nil
	}

	video := s.Program.Video
	if video.ContainerFormat == "MPEG-TS" {
		// segment_map は再生開始位置のキャッシュなので、見つかればファイル I/O なしで即座に使う
		s.mu.RLock()
		entry, cached := s.segmentMapBySeq[sequence]
		s.mu.RUnlock()
		if cached {
			s.setSource(segment, &entry.SourceFilePosition, entry.SourceStartDTS)
			s.logger.Debug(fmt.Sprintf("%s[Segment %d] Segment source position resolved from segment_map. [elapsed: %.1fms]",
				s.LogPrefix(), sequence, float64(time.Since(start).Microseconds())/1000))
			return nil
		}
		if err := s.ensureTSContextLocked(); err != nil {
			return fmt.Errorf("%w: failed to read TS stream info: %v", ErrRuntime, err)
		}
		s.mu.RLock()
		info, baseDTS := s.tsStreamInfo, *s.tsSourceBaseDTS
		s.mu.RUnlock()
		maxAge := int64(math.RoundToEven(s.segmentDurationSeconds * float64(HZ)))
		position, err := s.seekFn(video.FilePath, info, segment.PlaylistStartSeconds, baseDTS, maxAge)
		if err != nil {
			return fmt.Errorf("%w: failed to seek the source position: %v", ErrRuntime, err)
		}
		s.setSource(segment, &position.SourceFilePosition, position.SourceStartDTS)
		// オンデマンド探索の結果は次回以降のシークを軽くするため DB に保存する (失敗しても続行)
		s.SaveSegmentMapEntries(context.Background(), []SegmentMapEntry{{
			SequenceIndex:      sequence,
			SourceFilePosition: position.SourceFilePosition,
			SourceStartDTS:     position.SourceStartDTS,
		}})
		s.logger.Info(fmt.Sprintf("%s[Segment %d] Segment source position resolved by TS seek. [elapsed: %.1fms]",
			s.LogPrefix(), sequence, float64(time.Since(start).Microseconds())/1000))
		return nil
	}

	// MP4 は moov 内テーブルから同期サンプル DTS を短時間で復元できるため、DB キャッシュを作らない
	s.mu.RLock()
	loaded, list := s.mp4Loaded, s.mp4KeyFrameDTSList
	s.mu.RUnlock()
	if !loaded {
		list, err = s.readMP4DTSFn(video.FilePath)
		if err != nil {
			return fmt.Errorf("%w: failed to read MP4 key frames: %v", ErrRuntime, err)
		}
		s.mu.Lock()
		s.mp4KeyFrameDTSList, s.mp4Loaded = list, true
		s.mu.Unlock()
	}
	dts, err := FindKeyFrameDTSBefore(list, segment.PlaylistStartSeconds)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRuntime, err)
	}
	s.setSource(segment, nil, dts)
	s.logger.Debug(fmt.Sprintf("%s[Segment %d] Segment source position resolved from MP4 keyframe table. [elapsed: %.1fms]",
		s.LogPrefix(), sequence, float64(time.Since(start).Microseconds())/1000))
	return nil
}

func (s *Session) setSource(segment *Segment, position *int64, dts int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	segment.sourceFilePosition = position
	segment.sourceStartDTS = &dts
}

// ---- セグメント取得 (移植元: VideoStream.getSegment) ----

// CompleteAndAdvance は sequence のセグメントをエンコード済みにし、次のセグメントがあれば同時に Encoding へ進める。
// 完了と次の Encoding 化を 1 つのロックで行うことで、直前セグメントの完了を待っていた GetSegment が
// 次セグメントを Pending と誤認して別タスクを起動する競合を防ぐ (Python は asyncio のため await 間で切り替わらない) 。
func (s *Session) CompleteAndAdvance(sequence int, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sequence < 0 || sequence >= len(s.segments) {
		return
	}
	segment := s.segments[sequence]
	segment.future.resolve(data)
	segment.status = SegmentCompleted
	if sequence+1 < len(s.segments) {
		s.segments[sequence+1].status = SegmentEncoding
	}
}

// GetSegment はエンコードされた HLS セグメントを取得する。
// まだエンコードされていなければ旧エンコードタスクを終了し、sequence を含む範囲から新たにエンコードを開始する。
//
// 戻り値:
//   - (nil, nil): シーケンス番号が不正、またはシークやセッション終了で取得が中断された (HTTP 422)
//   - (nil, error): errors.Is(err, ErrValue) は 422、ErrRuntime は 500、ctx のエラーはクライアント切断
func (s *Session) GetSegment(ctx context.Context, sequence int, audio string) ([]byte, error) {
	s.KeepAlive()
	if s.IsDestroyed() {
		return nil, nil
	}
	segment, err := s.segmentAt(sequence)
	if err != nil {
		return nil, nil // 不正なシーケンス番号 (破棄済み含む)
	}

	for s.statusOf(segment) == SegmentPending {
		if s.IsDestroyed() {
			return nil, nil
		}
		// 主音声と副音声が隣り合うセグメントを要求した場合は、直前のエンコード完了を待って同じタスクを共有する
		if sequence > 0 {
			previous, err := s.segmentAt(sequence - 1)
			if err != nil {
				return nil, nil
			}
			if future, encoding := s.encodingFuture(previous); encoding {
				data, err := waitFuture(ctx, future)
				if err != nil {
					return nil, err
				}
				if len(data) == 0 {
					return nil, nil // シークやセッション終了で直前のエンコードが中断された
				}
				continue
			}
		}

		retry, err := s.startEncodingIfPending(sequence, segment)
		if err != nil {
			return nil, err
		}
		if retry {
			continue
		}
		break
	}

	s.mu.RLock()
	future := segment.future
	s.mu.RUnlock()
	encoded, err := waitFuture(ctx, future)
	if err != nil {
		return nil, err
	}
	if len(encoded) == 0 {
		s.mu.RLock()
		aborted := s.destroyed || segment.future != future
		s.mu.RUnlock()
		if aborted {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: Encoded segment is empty. [sequence: %d]", ErrRuntime, sequence)
	}

	result := encoded
	if audio == "secondary" {
		extracted, err := ExtractSecondaryAudio(encoded)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrValue, err)
		}
		result = extracted
	}

	s.mu.Lock()
	segment.readed = true
	var readed []*Segment
	for _, candidate := range s.segments {
		if candidate.readed {
			readed = append(readed, candidate)
		}
	}
	var oldest *Segment
	if len(readed) >= MaxReadedSegments {
		oldest = readed[0]
		oldest.resetStateLocked()
	}
	s.mu.Unlock()
	if oldest != nil {
		s.logger.Info(fmt.Sprintf("%s[Segment %d] Reset segment data to free memory.", s.LogPrefix(), oldest.SequenceIndex))
	}
	return result, nil
}

func (s *Session) statusOf(segment *Segment) SegmentStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return segment.status
}

func (s *Session) encodingFuture(segment *Segment) (*segmentFuture, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return segment.future, segment.status == SegmentEncoding
}

// waitFuture は結果の確定か ctx の終了を待つ (asyncio.shield 相当: ctx 終了でも Future 自体は影響を受けない) 。
func waitFuture(ctx context.Context, future *segmentFuture) ([]byte, error) {
	select {
	case <-future.done:
		return future.data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// startEncodingIfPending はエンコードタスクのロックを取り、まだ Pending なら新しいタスクを起動する。
// 直前セグメントのエンコードが始まっていた場合は retry=true を返す。
func (s *Session) startEncodingIfPending(sequence int, segment *Segment) (retry bool, err error) {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()

	if s.IsDestroyed() {
		return false, nil
	}
	if sequence > 0 {
		if previous, segErr := s.segmentAt(sequence - 1); segErr == nil {
			if _, encoding := s.encodingFuture(previous); encoding {
				return true, nil
			}
		}
	}
	if s.statusOf(segment) != SegmentPending {
		return false, nil
	}

	factory := s.manager.encoderFactory()
	if factory == nil {
		s.logger.Error(s.LogPrefix() + " Segment encoder is not registered.")
		return false, ErrEncoderUnavailable
	}

	// シークでは旧エンコーダーが同じ録画ファイルを読み続けていると探索と I/O が競合するため、
	// source position 解決より前に旧タスクへキャンセルを投げる (終了は待たない)
	if s.hasTask() {
		s.cancelTask(false)
		s.logger.Info(fmt.Sprintf("%s[Segment %d] Previous Encoding Task Canceled before source position resolution.", s.LogPrefix(), sequence))
	}

	if err := s.ResolveSegmentSourcePosition(sequence); err != nil {
		return false, err
	}
	startSequence := sequence

	// QSVEncC は MPEG-TS の入力 DTS が 33bit ラップ直前にあると音ズレするため、少し手前から連続エンコードする
	video := s.Program.Video
	if s.encoderName == "QSVEncC" && video.ContainerFormat == "MPEG-TS" && !video.HasVideoStreamChanges {
		wrapAvoidanceTicks := int64(DTSWrapAvoidanceSeconds) * HZ
		iterations := 0
		for startSequence > 0 {
			iterations++
			if iterations > DTSWrapAvoidanceMaxBacktrackSegments {
				s.logger.Warn(fmt.Sprintf("%s[Segment %d] QSVEncC DTS wrap avoidance reached the backtrack limit. [encoding_start_sequence: %d]",
					s.LogPrefix(), sequence, startSequence))
				break
			}
			startSegment, segErr := s.segmentAt(startSequence)
			if segErr != nil {
				return false, segErr
			}
			if _, resolved := startSegment.SourceStartDTS(); !resolved {
				if err := s.ResolveSegmentSourcePosition(startSequence); err != nil {
					return false, err
				}
			}
			dts, _ := startSegment.SourceStartDTS()
			if PCRCycle-(dts%PCRCycle) > wrapAvoidanceTicks {
				break
			}
			startSequence--
		}
		if startSequence != sequence {
			s.logger.Info(fmt.Sprintf("%s[Segment %d] QSVEncC start adjusted to Segment %d to avoid DTS wrap.", s.LogPrefix(), sequence, startSequence))
		}
	}

	// 新しいタスクを起動した時点で既にエンコード済みのセグメントは使えなくなるので、すべてリセットする
	// (取得中断で空データのまま確定した Future も、再試行できるよう交換する)
	s.mu.Lock()
	if s.destroyed {
		s.mu.Unlock()
		return false, nil
	}
	for _, candidate := range s.segments {
		if candidate.status != SegmentPending || (candidate.future.resolved && len(candidate.future.data) == 0) {
			candidate.resetStateLocked()
		}
	}
	s.segments[startSequence].status = SegmentEncoding
	s.collectedKeyFrames = nil
	s.lastFlushedCount = 0

	taskContext, cancel := context.WithCancel(context.Background())
	task := &encodingTask{encoder: factory(s), cancel: cancel, done: make(chan struct{})}
	s.currentTask = task
	s.mu.Unlock()

	go s.runTask(taskContext, task, startSequence)
	s.logger.Info(fmt.Sprintf("%s[Segment %d] New Encoding Task Started.", s.LogPrefix(), startSequence))
	return false, nil
}

func (s *Session) hasTask() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentTask != nil
}

// runTask はエンコーダーを実行し、異常終了時に結果待ちのリクエストを失敗扱い (空データ) で解放する。
func (s *Session) runTask(ctx context.Context, task *encodingTask, startSequence int) {
	defer close(task.done)
	err := task.encoder.Run(ctx, startSequence)
	// 収集済みキーフレームの残りを保存する
	if ctx.Err() == nil {
		s.FlushKeyFrames(context.Background())
	}

	s.mu.Lock()
	if ctx.Err() == nil && s.currentTask == task {
		for _, segment := range s.segments {
			if segment.status == SegmentEncoding {
				// Future は交換せず空データで確定する (待機者は RuntimeError 相当になる) 。次回の起動時に Future を交換する
				segment.future.resolve([]byte{})
				segment.status = SegmentPending
			}
		}
	}
	if s.currentTask == task {
		s.currentTask = nil
	}
	destroyed := s.destroyed
	s.mu.Unlock()
	if err != nil && ctx.Err() == nil && !destroyed {
		s.logger.Error(s.LogPrefix()+" Encoding task failed: "+err.Error(), "error", err)
	}
}
