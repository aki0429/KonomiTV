package jikkyo

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Comment は DPlayer が受け付けるコメント形式 (schemas.JikkyoComment 互換) 。
type Comment struct {
	Time   float64 `json:"time"`
	Type   string  `json:"type"`
	Size   string  `json:"size"`
	Color  string  `json:"color"`
	Author string  `json:"author"`
	Text   string  `json:"text"`
}

// Comments は過去ログコメントの取得結果 (schemas.JikkyoComments 互換) 。
type Comments struct {
	IsSuccess bool      `json:"is_success"`
	Comments  []Comment `json:"comments"`
	Detail    string    `json:"detail"`
}

// colorCodeMap はニコニコの色指定と 16 進数カラーコードのマッピング。
// Python 版 JikkyoClient.COLOR_CODE_MAP と同じ内容を維持すること。
var colorCodeMap = map[string]string{
	"white":          "#FFEAEA",
	"red":            "#F02840",
	"pink":           "#FD7E80",
	"orange":         "#FDA708",
	"yellow":         "#FFE133",
	"green":          "#64DD17",
	"cyan":           "#00D4F5",
	"blue":           "#4763FF",
	"purple":         "#D500F9",
	"black":          "#1E1310",
	"white2":         "#CCCC99",
	"niconicowhite":  "#CCCC99",
	"red2":           "#CC0033",
	"truered":        "#CC0033",
	"pink2":          "#FF33CC",
	"orange2":        "#FF6600",
	"passionorange":  "#FF6600",
	"yellow2":        "#999900",
	"madyellow":      "#999900",
	"green2":         "#00CC66",
	"elementalgreen": "#00CC66",
	"cyan2":          "#00CCCC",
	"blue2":          "#3399FF",
	"marineblue":     "#3399FF",
	"purple2":        "#6633CC",
	"nobleviolet":    "#6633CC",
	"black2":         "#666666",
}

// specialCommandCommentPattern はニコ生の特殊コマンド付きコメントのフィルタ正規表現。
// Python 版 SPECIAL_COMMAND_COMMENT_PATTERN (r'^/[a-z][a-z0-9_-]*(?:\s|$)') と同じ。
var specialCommandCommentPattern = regexp.MustCompile(`^/[a-z][a-z0-9_-]*(?:\s|$)`)

// hexColorPattern は 16 進数カラーコードの正規表現。
var hexColorPattern = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// GetCommentColor はニコニコの色指定を 16 進数カラーコードに置換する。
// 移植元: server/app/utils/JikkyoClient.py の getCommentColor()
func GetCommentColor(color string) (string, bool) {
	// 16 進数カラーコードがそのまま入っている場合はそのまま返す
	if hexColorPattern.MatchString(color) {
		return color, true
	}
	code, ok := colorCodeMap[color]
	return code, ok
}

// GetCommentPosition はニコニコの位置指定を DPlayer の位置指定に置換する。
// 移植元: server/app/utils/JikkyoClient.py の getCommentPosition()
func GetCommentPosition(position string) (string, bool) {
	positions := map[string]string{"ue": "top", "naka": "right", "shita": "bottom"}
	converted, ok := positions[position]
	return converted, ok
}

// GetCommentSize はニコニコのサイズ指定を DPlayer のサイズ指定に置換する。
// 移植元: server/app/utils/JikkyoClient.py の getCommentSize()
func GetCommentSize(size string) (string, bool) {
	sizes := map[string]string{"big": "big", "medium": "medium", "small": "small"}
	converted, ok := sizes[size]
	return converted, ok
}

// IsSpecialCommandComment はコメントがニコ生の運営コマンド付きコメントかどうかを判定する。
// 移植元: server/app/utils/JikkyoClient.py の isSpecialCommandComment()
func IsSpecialCommandComment(comment string, premium *string) bool {
	if !specialCommandCommentPattern.MatchString(comment) {
		return false
	}
	// premium フラグが付与されている場合は、運営コメント (premium=3) のみを特殊コマンドとして扱う
	if premium != nil {
		return *premium == "3"
	}
	// premium フラグが欠落している場合、運営コメントかどうかを判定できないため特殊コマンドとして扱わない
	return false
}

// ParseCommentCommand はニコニコのコメントコマンドを解析し、色・位置・サイズを取得する。
// 移植元: server/app/utils/JikkyoClient.py の parseCommentCommand()
func ParseCommentCommand(commentMail *string) (string, string, string) {
	color := "#FFEAEA"  // 初期色
	position := "right" // 初期位置
	size := "medium"    // 初期サイズ

	if commentMail != nil {
		commands := strings.Split(strings.ReplaceAll(*commentMail, "184", ""), " ")
		for _, command := range commands {
			if parsedColor, ok := GetCommentColor(command); ok {
				color = parsedColor
			}
			if parsedPosition, ok := GetCommentPosition(command); ok {
				position = parsedPosition
			}
			if parsedSize, ok := GetCommentSize(command); ok {
				size = parsedSize
			}
		}
	}
	return color, position, size
}

// FetchJikkyoComments はニコニコ実況 過去ログ API から過去ログコメントを取得し、
// DPlayer が受け付けるコメント形式に変換して返す。
// 移植元: server/app/utils/JikkyoClient.py の fetchJikkyoComments()
func FetchJikkyoComments(
	httpClient *http.Client,
	jikkyoID string,
	recordingStartTime time.Time,
	recordingEndTime time.Time,
) (*Comments, error) {
	startTime := recordingStartTime.Unix()
	endTime := recordingEndTime.Unix()
	kakologAPIURL := fmt.Sprintf(
		"https://jikkyo.tsukumijima.net/api/kakolog/%s?starttime=%d&endtime=%d&format=json",
		jikkyoID, startTime, endTime,
	)

	// 30 秒応答がなかったらタイムアウト (レスポンスが結構重めなので場合によっては時間がかかることがある)
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	request, err := http.NewRequest(http.MethodGet, kakologAPIURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := httpClient.Do(request)
	if err != nil {
		// 接続エラー (サーバー再起動やタイムアウトなど)
		return &Comments{
			IsSuccess: false,
			Comments:  []Comment{},
			Detail:    "過去ログ API に接続できませんでした。過去ログ API で障害が発生している可能性があります。",
		}, nil
	}
	defer func() { _ = response.Body.Close() }()

	// ステータスコードが 200 以外
	if response.StatusCode != http.StatusOK {
		var detail string
		switch response.StatusCode {
		case http.StatusInternalServerError:
			detail = "過去ログ API でサーバーエラーが発生しました。過去ログ API に不具合がある可能性があります。(HTTP Error 500)"
		case http.StatusServiceUnavailable:
			detail = "現在、過去ログ API は一時的に利用できなくなっています。(HTTP Error 503)"
		default:
			detail = fmt.Sprintf("現在、過去ログ API でエラーが発生しています。(HTTP Error %d)", response.StatusCode)
		}
		return &Comments{IsSuccess: false, Comments: []Comment{}, Detail: detail}, nil
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	return parseKakologResponse(body, startTime)
}

// kakologResponse は過去ログ API のレスポンス。
type kakologResponse struct {
	Error  string `json:"error"`
	Packet []struct {
		Chat struct {
			Thread    string `json:"thread"`
			No        string `json:"no"`
			Vpos      string `json:"vpos"`
			Date      string `json:"date"`
			DateUsec  string `json:"date_usec"`
			UserID    string `json:"user_id"`
			Mail      string `json:"mail"`
			Premium   string `json:"premium"`
			Anonymity string `json:"anonymity"`
			Deleted   string `json:"deleted"`
			Content   string `json:"content"`
		} `json:"chat"`
	} `json:"packet"`
}

// parseKakologResponse は過去ログ API のレスポンスを schemas.JikkyoComments 互換の形に変換する。
func parseKakologResponse(body []byte, startTime int64) (*Comments, error) {
	parsed := kakologResponse{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse the kakolog API response: %w", err)
	}

	// エラーが入っていた場合はそのエラーを返す
	if parsed.Error != "" {
		return &Comments{IsSuccess: false, Comments: []Comment{}, Detail: parsed.Error}, nil
	}

	// 過去ログコメントが 1 つもない場合はエラーを返す
	if len(parsed.Packet) == 0 {
		return &Comments{
			IsSuccess: false,
			Comments:  []Comment{},
			Detail:    "この録画番組の過去ログコメントは存在しないか、現在取得中です。",
		}, nil
	}

	// 取得した過去ログコメントを随時整形
	comments := []Comment{}
	for _, packet := range parsed.Packet {
		chat := packet.Chat

		// コメントデータが不正な場合はスキップ
		if chat.Content == "" {
			continue
		}
		// 削除されているコメントを除外
		if chat.Deleted == "1" {
			continue
		}
		// 運営コメントは今のところ全て弾く
		var premium *string
		if chat.Premium != "" {
			value := chat.Premium
			premium = &value
		}
		if IsSpecialCommandComment(chat.Content, premium) {
			continue
		}

		// コメントコマンドをパース
		var mail *string
		if chat.Mail != "" {
			value := chat.Mail
			mail = &value
		}
		color, position, size := ParseCommentCommand(mail)

		// コメント投稿日時 (秒単位) を算出
		chatDate, err := strconv.ParseFloat(chat.Date, 64)
		if err != nil {
			continue
		}
		chatDateUsec := 0
		if chat.DateUsec != "" {
			if parsedUsec, err := strconv.Atoi(chat.DateUsec); err == nil {
				chatDateUsec = parsedUsec
			}
		}
		commentTime := float64(int64(chatDate)-startTime) + float64(chatDateUsec)/1000000

		comments = append(comments, Comment{
			Time:   commentTime,
			Type:   position,
			Size:   size,
			Color:  color,
			Author: chat.UserID,
			Text:   chat.Content,
		})
	}

	return &Comments{
		IsSuccess: true,
		Comments:  comments,
		Detail:    "過去ログコメントを取得しました。",
	}, nil
}
