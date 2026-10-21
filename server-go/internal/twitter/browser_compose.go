package twitter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

// PostTweetViaComposeUI は Web App のモーダルを操作し、実際の CreateTweet 応答を返す。
// 送信後に結果が不明になっても自動再送しない。
func (b *BrowserBackend) PostTweetViaComposeUI(parent context.Context, request ComposeRequest) (*ComposeResult, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	if err := b.acquire(ctx); err != nil {
		return nil, err
	}
	defer b.release()
	browser, err := b.activeBrowser()
	if err != nil {
		return nil, err
	}
	if len(request.Images) > MaxTweetImages {
		return nil, errors.New("too many tweet images")
	}
	if math.IsNaN(request.ThrottleRemainingSec) || math.IsInf(request.ThrottleRemainingSec, 0) {
		return nil, errors.New("invalid tweet throttle")
	}
	if request.InReplyToStatusID != nil {
		if *request.InReplyToStatusID == "" {
			return nil, errors.New("invalid reply tweet ID")
		}
		for _, digit := range *request.InReplyToStatusID {
			if digit < '0' || digit > '9' {
				return nil, errors.New("invalid reply tweet ID")
			}
		}
	}
	result := &ComposeResult{}
	failure := func(message string) (*ComposeResult, error) { result.ErrorMessage = &message; return result, nil }
	start := time.Now()
	events, unsubscribe := browser.Subscribe("Network.responseReceived", "Network.loadingFinished", "Network.loadingFailed")
	defer unsubscribe()
	if err := browser.Call(ctx, "Network.enable", nil, nil); err != nil {
		return nil, err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		_ = browser.Call(cleanup, "Network.disable", nil, nil)
	}()
	// 失敗時だけ残ったモーダルを閉じる。ブラウザ接続喪失時は Shutdown に任せる。
	defer func() {
		if !result.IsSuccess {
			cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
			defer stop()
			_, _ = browserEvaluate(cleanup, browser, `(() => { const dialog=document.querySelector('[role="dialog"]'); if (!dialog) return; const close=dialog.querySelector('[data-testid="app-bar-close"]'); if(close) close.click(); const discard=document.querySelector('[data-testid="confirmationSheetConfirm"]'); if(discard) discard.click(); })()`, false)
		}
	}()
	if err := browser.Call(ctx, "Page.bringToFront", nil, nil); err != nil {
		return nil, err
	}
	if request.InReplyToStatusID == nil {
		if _, err := browserEvaluate(ctx, browser, "document.body.focus()", false); err != nil {
			return nil, err
		}
		if err := dispatchBrowserKey(ctx, browser, "keyDown", "n", "KeyN", 78, 0, "n"); err != nil {
			return nil, err
		}
		if err := dispatchBrowserKey(ctx, browser, "keyUp", "n", "KeyN", 78, 0, ""); err != nil {
			return nil, err
		}
	} else {
		reply, _ := json.Marshal(*request.InReplyToStatusID)
		script := fmt.Sprintf(`(() => { const id=%s; const state={state:{inReplyToStatusId:id},key:Math.random().toString(36).slice(2,8)}; history.pushState(state,'','/compose/post?in_reply_to='+encodeURIComponent(id)); dispatchEvent(new PopStateEvent('popstate',{state})); })()`, reply)
		if _, err := browserEvaluate(ctx, browser, script, false); err != nil {
			return nil, err
		}
	}
	if err := waitBrowserCondition(ctx, browser, `location.pathname === '/compose/post' && !!document.querySelector('[role="dialog"] [data-testid="tweetTextarea_0"]')`, 15*time.Second); err != nil {
		return failure("ツイート送信モーダルを開けませんでした。")
	}
	remaining := time.Duration(request.ThrottleRemainingSec*float64(time.Second)) - time.Since(start)
	// Python と同じくモーダル表示後の待機へスロットルを吸収する。
	if remaining < time.Second {
		remaining = time.Second
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-browser.Done():
		return nil, ErrBrowserConnectionLost
	case <-timer.C:
	}
	if request.TweetText != "" {
		text, _ := json.Marshal(request.TweetText)
		value, err := browserEvaluate(ctx, browser, fmt.Sprintf(`(() => { const textarea=document.querySelector('[role="dialog"] [data-testid="tweetTextarea_0"]'); if(!textarea)return false; textarea.focus(); const dt=new DataTransfer(); dt.setData('text/plain',%s); textarea.dispatchEvent(new ClipboardEvent('paste',{bubbles:true,cancelable:true,clipboardData:dt})); return true; })()`, text), false)
		if err != nil {
			return nil, err
		}
		if string(value) != "true" {
			return failure("テキストの入力に失敗しました。")
		}
	}
	// 状態を無制限に保持しない。投稿一回の CreateTweet と FINALIZE だけを追跡する。
	monitor := composeNetworkMonitor{responses: map[string]*RawResponse{}, uploads: map[string]bool{}}
	if len(request.Images) > 0 {
		images := make([]map[string]string, 0, len(request.Images))
		for _, image := range request.Images {
			images = append(images, map[string]string{"fileName": image.Filename, "mimeType": image.ContentType, "data": base64.StdEncoding.EncodeToString(image.Data)})
		}
		encoded, _ := json.Marshal(images)
		script := fmt.Sprintf(`(() => { const images=%s; const input=document.querySelector('[role="dialog"] [data-testid="fileInput"]'); if(!input)return false; const dt=new DataTransfer(); for(const image of images){const bytes=Uint8Array.from(atob(image.data),c=>c.charCodeAt(0)); dt.items.add(new File([bytes],image.fileName,{type:image.mimeType}));} input.files=dt.files; input.dispatchEvent(new Event('change',{bubbles:true})); return true; })()`, encoded)
		value, err := browserEvaluate(ctx, browser, script, false)
		if err != nil {
			return nil, err
		}
		if string(value) != "true" {
			return failure("画像の添付に失敗しました。")
		}
		uploadCtx, stop := context.WithTimeout(ctx, 60*time.Second)
		for monitor.uploadCount < len(request.Images) {
			if _, err = monitor.next(uploadCtx, browser, events); err != nil {
				break
			}
		}
		stop()
		if err != nil {
			return failure("画像のアップロードに失敗しました。")
		}
	}
	if err := waitBrowserCondition(ctx, browser, `(() => { const button=document.querySelector('[role="dialog"] [data-testid="tweetButton"]'); return !!button && !button.disabled && button.getAttribute('aria-disabled') !== 'true'; })()`, 30*time.Second); err != nil {
		return failure("ツイートの送信ボタンが有効になりませんでした。")
	}
	if _, err := browserEvaluate(ctx, browser, `document.querySelector('[role="dialog"] [data-testid="tweetTextarea_0"]').focus()`, false); err != nil {
		return nil, err
	}
	// 最初の送信イベント前に時刻を記録し、途中失敗でも再送制限を保つ。
	submitted := float64(time.Now().UnixNano()) / 1e9
	result.ComposeSubmittedAt = &submitted
	for _, key := range []struct {
		kind, key, code string
		vk, mod         int
		text            string
	}{
		{"keyDown", "Control", "ControlLeft", 17, 2, ""}, {"keyDown", "Enter", "Enter", 13, 2, "\r"},
		{"keyUp", "Enter", "Enter", 13, 2, ""}, {"keyUp", "Control", "ControlLeft", 17, 0, ""},
	} {
		if err := dispatchBrowserKey(ctx, browser, key.kind, key.key, key.code, key.vk, key.mod, key.text); err != nil {
			return failure("ツイート送信結果を確認できませんでした。タイムラインを確認してください。")
		}
	}
	responseCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	for {
		response, err := monitor.next(responseCtx, browser, events)
		if err != nil {
			return failure("CreateTweet のレスポンスを確認できませんでした。タイムラインを確認してください。")
		}
		if response != nil {
			result.IsSuccess = true
			result.GraphQLAPIResult = response
			return result, nil
		}
	}
}

func dispatchBrowserKey(ctx context.Context, browser BrowserSession, kind, key, code string, vk, mod int, text string) error {
	params := map[string]any{"type": kind, "key": key, "code": code, "windowsVirtualKeyCode": vk, "nativeVirtualKeyCode": vk, "modifiers": mod}
	if text != "" {
		params["text"] = text
	}
	return browser.Call(ctx, "Input.dispatchKeyEvent", params, nil)
}

func waitBrowserCondition(parent context.Context, browser BrowserSession, expression string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		value, err := browserEvaluate(ctx, browser, expression, false)
		if err != nil {
			return err
		}
		if string(value) == "true" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-browser.Done():
			return ErrBrowserConnectionLost
		case <-ticker.C:
		}
	}
}

type composeNetworkMonitor struct {
	responses   map[string]*RawResponse
	uploads     map[string]bool
	uploadCount int
}

func (m *composeNetworkMonitor) next(ctx context.Context, browser BrowserSession, events <-chan BrowserEvent) (*RawResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-browser.Done():
		return nil, ErrBrowserConnectionLost
	case event := <-events:
		var message struct {
			ID       string `json:"requestId"`
			Type     string `json:"type"`
			Response struct {
				URL     string         `json:"url"`
				Status  int            `json:"status"`
				Headers map[string]any `json:"headers"`
			} `json:"response"`
		}
		if err := json.Unmarshal(event.Params, &message); err != nil {
			return nil, err
		}
		switch event.Method {
		case "Network.responseReceived":
			if message.Type == "Preflight" {
				return nil, nil
			}
			address, err := url.Parse(message.Response.URL)
			if err != nil {
				return nil, err
			}
			host := address.Hostname()
			if (host == "x.com" || host == "twitter.com" || host == "api.x.com" || host == "api.twitter.com") && strings.HasSuffix(address.Path, "/CreateTweet") {
				if len(m.responses) >= 8 {
					return nil, errors.New("too many compose responses")
				}
				headers := map[string]string{}
				for key, value := range message.Response.Headers {
					headers[strings.ToLower(key)] = fmt.Sprint(value)
				}
				status := message.Response.Status
				m.responses[message.ID] = &RawResponse{StatusCode: &status, Headers: headers}
			}
			if (host == "upload.x.com" || host == "upload.twitter.com") && strings.EqualFold(address.Query().Get("command"), "FINALIZE") && !m.uploads[message.ID] {
				if message.Response.Status < 200 || message.Response.Status >= 300 {
					return nil, errors.New("image upload failed")
				}
				if len(m.uploads) >= MaxTweetImages {
					return nil, errors.New("too many upload responses")
				}
				m.uploads[message.ID] = true
				m.uploadCount++
			}
		case "Network.loadingFailed":
			if m.responses[message.ID] != nil {
				return nil, errors.New("tweet response connection failed")
			}
		case "Network.loadingFinished":
			response := m.responses[message.ID]
			if response == nil {
				return nil, nil
			}
			delete(m.responses, message.ID)
			var body struct {
				Body   string `json:"body"`
				Base64 bool   `json:"base64Encoded"`
			}
			if err := browser.Call(ctx, "Network.getResponseBody", map[string]any{"requestId": message.ID}, &body); err != nil {
				return nil, err
			}
			if body.Base64 {
				data, err := base64.StdEncoding.DecodeString(body.Body)
				if err != nil {
					return nil, err
				}
				body.Body = string(data)
			}
			if err := json.Unmarshal([]byte(body.Body), &response.ParsedResponse); err != nil {
				return nil, errors.New("invalid tweet response body")
			}
			response.ResponseText = &body.Body
			return response, nil
		}
		return nil, nil
	}
}
