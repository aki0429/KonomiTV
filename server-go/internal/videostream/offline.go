package videostream

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/stream"
)

// OfflineSegment は保存用セグメントの本文とプレイリスト上の長さを保持する。
type OfflineSegment struct {
	DurationSeconds float64
	Data            []byte
}

// OfflineParams は通常 HLS セッションと独立した保存処理の入力情報。
type OfflineParams struct {
	Program         *database.RecordedProgramDetail
	Quality         string
	EncodingOptions stream.StreamEncodingOptions
	Encoder         string
}

// OfflineEncoder は順序付きの有限ストリームと、その資源の終了責務を持つ。
// Next は ctx のキャンセルで解除され、Close は起動した全処理の終了を待つ必要がある。
type OfflineEncoder interface {
	Next(context.Context) (OfflineSegment, error)
	Close() error
}

// OfflineEncoderFactory は通常視聴のキャッシュを持たない保存専用エンコーダーを生成する。
type OfflineEncoderFactory func(context.Context, OfflineParams) (OfflineEncoder, error)

// OfflineMetadata は OfflineVideoStreamMetadata と同じ四つのフィールドを固定する。
type OfflineMetadata struct {
	VideoID         int64   `json:"video_id"`
	FileHash        string  `json:"file_hash"`
	Quality         string  `json:"quality"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// WriteOffline はクライアントが復元する big-endian KTVODLP 本文を順次出力する。
// エンコーダー所有権は呼出元にあり、成功の終端レコードは EOF のときだけ出力する。
func WriteOffline(ctx context.Context, w io.Writer, metadata OfflineMetadata, encoder OfflineEncoder) error {
	data, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("marshal offline metadata: %w", err)
	}
	// クライアントの割当上限を超えるメタデータは応答開始前に拒否する。
	if len(data) > 1<<20 {
		return fmt.Errorf("offline metadata exceeds 1 MiB")
	}
	if _, err = io.WriteString(w, "KTVODLP\n"); err != nil {
		return fmt.Errorf("write offline magic: %w", err)
	}
	if err = binary.Write(w, binary.BigEndian, uint32(len(data))); err != nil {
		return fmt.Errorf("write metadata length: %w", err)
	}
	if _, err = w.Write(data); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}
	// 最初のセグメントを待つ間もメタデータを受信でき、切断を検出する。
	if flusher, ok := w.(interface{ Flush() }); ok {
		flusher.Flush()
	}
	var count uint32
	for {
		segment, err := encoder.Next(ctx)
		if err == io.EOF {
			return binary.Write(w, binary.BigEndian, [2]uint32{math.MaxUint32, count})
		}
		if err != nil {
			return fmt.Errorf("generate offline segment: %w", err)
		}
		// Go の浮動小数点→整数変換で overflow を隠さない。
		milliseconds := max(1, math.RoundToEven(segment.DurationSeconds*1000))
		if math.IsNaN(segment.DurationSeconds) || math.IsInf(segment.DurationSeconds, 0) || milliseconds > math.MaxUint32 || len(segment.Data) == 0 || len(segment.Data) > 128<<20 || count == math.MaxUint32 {
			return fmt.Errorf("invalid offline segment")
		}
		duration := uint32(milliseconds)
		if err = binary.Write(w, binary.BigEndian, [3]uint32{count, duration, uint32(len(segment.Data))}); err != nil {
			return fmt.Errorf("write segment header: %w", err)
		}
		if _, err = w.Write(segment.Data); err != nil {
			return fmt.Errorf("write segment data: %w", err)
		}
		if flusher, ok := w.(interface{ Flush() }); ok {
			flusher.Flush()
		}
		count++
	}
}
