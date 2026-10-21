package api

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

type edcbLogoClient interface {
	FileCopy2([]string) ([]reservations.FileData, bool)
}

var edcbLogoClientFactory = func(ctx context.Context, address string) (edcbLogoClient, error) {
	return reservations.NewClientFromURLContext(ctx, address)
}
var edcbLogoFilename = regexp.MustCompile(`(?i)^[0-9a-f]{4}_[0-9a-f]{3}_[0-9a-f]{3}_[0-9]{2}\.(png|bmp)$`)

// EDCB の LogoData.ini とディレクトリ index を読む。チューナー/録画操作は送信しない。
func (s *Server) fetchEDCBChannelLogo(parent context.Context, channel *database.Channel) ([]byte, string, bool) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	client, err := edcbLogoClientFactory(ctx, s.config.General.EDCBURL)
	if err != nil {
		return nil, "", false
	}
	files, ok := client.FileCopy2([]string{"LogoData.ini", `LogoData\*.*`})
	if !ok || len(files) != 2 {
		return nil, "", false
	}
	logoID := -1
	target := fmt.Sprintf("%04X%04X", channel.NetworkID, channel.ServiceID)
	for _, line := range strings.Split(reservations.ConvertEDCBBytesToString(files[0].Data), "\n") {
		pair := strings.SplitN(line, "=", 2)
		if len(pair) == 2 && strings.EqualFold(strings.TrimSpace(pair[0]), target) {
			value, err := strconv.Atoi(strings.TrimSpace(pair[1]))
			if err == nil && value >= 0 && value <= 4095 {
				logoID = value
			}
			break
		}
	}
	if logoID < 0 {
		return nil, "", false
	}
	index := reservations.ConvertEDCBBytesToString(files[1].Data)
	prefix := fmt.Sprintf("%04X_%03X_", channel.NetworkID, logoID)
	for _, kind := range []int{5, 2, 4, 1, 3, 0} {
		suffix := fmt.Sprintf("_%02d.", kind)
		for _, line := range strings.Split(index, "\n") {
			// EDCB index は日時・サイズ・basename の4フィールド。
			fields := strings.SplitN(strings.TrimSuffix(line, "\r"), " ", 4)
			if len(fields) != 4 {
				continue
			}
			name := fields[3]
			if !edcbLogoFilename.MatchString(name) || !strings.EqualFold(name[:9], prefix) || name[12:16] != suffix {
				continue
			}
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
	}
	return nil, "", false
}
