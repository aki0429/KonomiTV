package metadata

import (
	"strconv"
	"strings"
)

// marshalThumbnailInfo は thumbnail_info カラムに保存する JSON 文字列を返す。
// Python の ThumbnailInfo (TypedDict) は version → representative → tile の順で
// json.dumps(x, ensure_ascii=False) されるため、その書式に合わせる。
func marshalThumbnailInfo(info ThumbnailInfo) (string, error) {
	var builder strings.Builder
	builder.WriteString(`{"version": `)
	builder.WriteString(strconv.Itoa(info.Version))
	builder.WriteString(`, "representative": {"format": `)
	builder.WriteString(pyString(info.Representative.Format))
	builder.WriteString(`, "width": `)
	builder.WriteString(strconv.Itoa(info.Representative.Width))
	builder.WriteString(`, "height": `)
	builder.WriteString(strconv.Itoa(info.Representative.Height))
	builder.WriteString(`}, "tile": {"format": `)
	builder.WriteString(pyString(info.Tile.Format))
	builder.WriteString(`, "image_width": `)
	builder.WriteString(strconv.Itoa(info.Tile.ImageWidth))
	builder.WriteString(`, "image_height": `)
	builder.WriteString(strconv.Itoa(info.Tile.ImageHeight))
	builder.WriteString(`, "tile_width": `)
	builder.WriteString(strconv.Itoa(info.Tile.TileWidth))
	builder.WriteString(`, "tile_height": `)
	builder.WriteString(strconv.Itoa(info.Tile.TileHeight))
	builder.WriteString(`, "total_tiles": `)
	builder.WriteString(strconv.Itoa(info.Tile.TotalTiles))
	builder.WriteString(`, "column_count": `)
	builder.WriteString(strconv.Itoa(info.Tile.ColumnCount))
	builder.WriteString(`, "row_count": `)
	builder.WriteString(strconv.Itoa(info.Tile.RowCount))
	builder.WriteString(`, "interval_sec": `)
	builder.WriteString(pyFloat(info.Tile.IntervalSec))
	builder.WriteString(`}}`)
	return builder.String(), nil
}
