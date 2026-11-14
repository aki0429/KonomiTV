package api

import (
	"context"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/config"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

type logoFileClient interface {
	FileCopy2([]string) ([]reservations.FileData, bool)
}

var edcbLogoClientFactory = func(ctx context.Context, cfg *config.Config) (logoFileClient, error) {
	client, err := reservations.NewClientFromURLContext(ctx, cfg.General.EDCBURL)
	if err == nil {
		client.SetTimeout(5 * time.Second)
	}
	return client, err
}

// fetchEDCBChannelLogo は本家 GetFallbackLogoData と同じ順序で放送波のロゴを取得する。
func (s *Server) fetchEDCBChannelLogo(ctx context.Context, channel *database.Channel) ([]byte, string, bool) {
	client, err := edcbLogoClientFactory(ctx, s.config)
	if err != nil {
		return nil, "", false
	}
	files, ok := client.FileCopy2([]string{"LogoData.ini", `LogoData\*.*`})
	if !ok || len(files) != 2 {
		return nil, "", false
	}
	logoID := reservations.GetLogoID(reservations.ConvertEDCBBytesToString(files[0].Data), channel.NetworkID, channel.ServiceID)
	if logoID < 0 {
		return nil, "", false
	}
	index := reservations.ConvertEDCBBytesToString(files[1].Data)
	for _, kind := range []int{5, 2, 4, 1, 3, 0} {
		name := reservations.GetLogoFileName(index, channel.NetworkID, logoID, kind)
		if name == "" {
			continue
		}
		// 最優先ファイルの転送失敗でも別種へ進まない点は本家と同じ。
		files, ok = client.FileCopy2([]string{`LogoData\` + name})
		if !ok || len(files) != 1 || len(files[0].Data) == 0 {
			return nil, "", false
		}
		mediaType := "image/png"
		if strings.HasSuffix(strings.ToUpper(name), ".BMP") {
			mediaType = "image/bmp"
		}
		return files[0].Data, mediaType, true
	}
	return nil, "", false
}
