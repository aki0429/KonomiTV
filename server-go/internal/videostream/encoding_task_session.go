package videostream

import (
	"context"

	"github.com/aki0429/KonomiTV/server-go/internal/stream"
)

// sessionEncodingAdapter は担当A の Session (session.go) を EncodingSession interface として使えるようにする。
// Session 型そのものには手を入れず、公開 API だけで接続する。
type sessionEncodingAdapter struct {
	session *Session
}

// NewSessionEncodingAdapter は *Session を EncodingSession として包む。
func NewSessionEncodingAdapter(session *Session) EncodingSession {
	return &sessionEncodingAdapter{session: session}
}

func (a *sessionEncodingAdapter) LogPrefix() string { return a.session.LogPrefix() }

func (a *sessionEncodingAdapter) Quality() string { return a.session.Quality() }

func (a *sessionEncodingAdapter) EncodingOptions() stream.StreamEncodingOptions {
	return a.session.EncodingOptions()
}

func (a *sessionEncodingAdapter) Encoder() string { return a.session.Encoder() }

// Video は録画ファイルの映像情報を EncodingVideoInfo へ詰め替える。
func (a *sessionEncodingAdapter) Video() EncodingVideoInfo {
	video := a.session.Program.Video
	return EncodingVideoInfo{
		FilePath:              video.FilePath,
		ContainerFormat:       video.ContainerFormat,
		VideoCodec:            video.VideoCodec,
		VideoScanType:         video.VideoScanType,
		VideoFrameRate:        video.VideoFrameRate,
		VideoResolutionWidth:  video.VideoResolutionWidth,
		VideoResolutionHeight: video.VideoResolutionHeight,
		HasVideoStreamChanges: video.HasVideoStreamChanges,
	}
}

// Channel は録画に紐づくチャンネルを EncodingChannel へ詰め替える (無ければ nil) 。
func (a *sessionEncodingAdapter) Channel() *EncodingChannel {
	channel := a.session.Program.Channel
	if channel == nil {
		return nil
	}
	return &EncodingChannel{
		NetworkID:         channel.NetworkID,
		TransportStreamID: channel.TransportStreamID,
		ServiceID:         channel.ServiceID,
	}
}

func (a *sessionEncodingAdapter) TSStreamInfo() *StreamInfo { return a.session.TSStreamInfo() }

func (a *sessionEncodingAdapter) EnsureTSKeyFrameContext() error {
	return a.session.EnsureTSKeyFrameContext()
}

// Segments は Session のセグメントを EncodingSegment へ包む。
func (a *sessionEncodingAdapter) Segments() []EncodingSegment {
	segments := a.session.Segments()
	result := make([]EncodingSegment, 0, len(segments))
	for _, segment := range segments {
		result = append(result, sessionSegmentAdapter{segment: segment})
	}
	return result
}

func (a *sessionEncodingAdapter) AddKeyFrames(ctx context.Context, keyFrames []KeyFrame) {
	a.session.AddKeyFrames(ctx, keyFrames)
}

func (a *sessionEncodingAdapter) FlushKeyFrames(ctx context.Context) {
	a.session.FlushKeyFrames(ctx)
}

// CompleteAndAdvance は完了と次セグメントの Encoding 化を原子的に行う (EncodingSession の任意 interface) 。
func (a *sessionEncodingAdapter) CompleteAndAdvance(sequence int, data []byte) {
	a.session.CompleteAndAdvance(sequence, data)
}

// sessionSegmentAdapter は *Segment を EncodingSegment interface として使えるようにする。
type sessionSegmentAdapter struct {
	segment *Segment
}

func (a sessionSegmentAdapter) Index() int { return a.segment.SequenceIndex }

func (a sessionSegmentAdapter) PlaylistStartSeconds() float64 { return a.segment.PlaylistStartSeconds }

func (a sessionSegmentAdapter) DurationSeconds() float64 { return a.segment.DurationSeconds }

func (a sessionSegmentAdapter) SourceStartDTS() (int64, bool) { return a.segment.SourceStartDTS() }

func (a sessionSegmentAdapter) SourceFilePosition() (int64, bool) {
	return a.segment.SourceFilePosition()
}

func (a sessionSegmentAdapter) MarkEncoding() { a.segment.MarkEncoding() }

func (a sessionSegmentAdapter) SetEncoded(data []byte) { a.segment.SetEncoded(data) }

// NewSessionSegmentEncoderFactory は Session から録画エンコードタスク (SegmentEncoder) を作る工場を返す。
// Manager.SetEncoderFactory() に渡して使う。
func NewSessionSegmentEncoderFactory(options EncodingTaskOptions) SegmentEncoderFactory {
	return func(session *Session) SegmentEncoder {
		return NewVideoEncodingTask(NewSessionEncodingAdapter(session), options)
	}
}
