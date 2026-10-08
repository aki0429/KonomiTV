package reservations

// SearchPg は過去番組を含めず検索する通常コマンド (CMD_EPG_SRV_SEARCH_PG=1025) 。
// バージョン付き自動予約と異なり、検索キーに chk_rec_end/day は付けない。
func (c *Client) SearchPg(keys []SearchKeyInfo) ([]EventInfo, bool) {
	response, ok := c.sendCmd(1025, func(writer *wireWriter) {
		writer.writeVector(len(keys), func() {
			for _, key := range keys {
				writeSearchKeyInfo(writer, key, false)
			}
		})
	})
	if !ok || response.Code != cmdSuccess {
		return nil, false
	}
	return decodeResponse(func() []EventInfo {
		reader := &wireReader{buf: response.Data}
		return readVector(reader, len(response.Data), readEventInfo)
	})
}
