package twitter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"
)

type composeNetworkResult struct {
	response *RawResponse
	err      error
}

// composeNetworkMonitor は投稿中だけ Network イベントを監視し、アップロードと送信結果を別々に通知する。
type composeNetworkMonitor struct {
	cancel        context.CancelFunc
	subscription  *cdpSubscription
	done          chan struct{}
	uploadSuccess chan struct{}
	uploadFailure chan struct{}
	result        chan composeNetworkResult
}

func startComposeNetworkMonitor(ctx context.Context, c *cdpConnection, session string, expected int) (*composeNetworkMonitor, error) {
	ctx, cancel := context.WithCancel(ctx)
	sub := c.subscribe(session, "Network.responseReceived", "Network.loadingFinished", "Network.loadingFailed")
	if err := c.call(ctx, session, "Network.enable", map[string]any{}, nil); err != nil {
		cancel()
		sub.Close()
		return nil, err
	}
	m := &composeNetworkMonitor{cancel: cancel, subscription: sub, done: make(chan struct{}), uploadSuccess: make(chan struct{}),
		uploadFailure: make(chan struct{}), result: make(chan composeNetworkResult, 1)}
	go func() {
		defer close(m.done)
		responses := map[string]*RawResponse{}
		count, uploaded, uploadFailed, completed := 0, false, false, false
		for {
			event, err := sub.next(ctx)
			if err != nil {
				if !completed {
					m.result <- composeNetworkResult{err: err}
				}
				return
			}
			var params struct {
				RequestID string `json:"requestId"`
				Type      string `json:"type"`
				ErrorText string `json:"errorText"`
				Response  struct {
					URL     string            `json:"url"`
					Status  float64           `json:"status"`
					Headers map[string]string `json:"headers"`
				} `json:"response"`
			}
			if json.Unmarshal(event.Params, &params) != nil {
				continue
			}
			switch event.Method {
			case "Network.responseReceived":
				if params.Type == "Preflight" {
					continue
				}
				status := int(params.Response.Status)
				if strings.Contains(params.Response.URL, "CreateTweet") {
					responses[params.RequestID] = &RawResponse{StatusCode: &status, Headers: params.Response.Headers}
				}
				if expected > 0 && strings.Contains(params.Response.URL, "upload.x.com") && strings.Contains(params.Response.URL, "FINALIZE") {
					if status >= 200 && status < 300 {
						count++
						if count >= expected && !uploaded {
							close(m.uploadSuccess)
							uploaded = true
						}
					} else if !uploadFailed {
						close(m.uploadFailure)
						uploadFailed = true
					}
				}
			case "Network.loadingFailed":
				if responses[params.RequestID] != nil && !completed {
					m.result <- composeNetworkResult{err: fmt.Errorf("CreateTweet request failed: %s", params.ErrorText)}
					completed = true
				}
			case "Network.loadingFinished":
				if response := responses[params.RequestID]; response != nil && !completed {
					var body struct {
						Body          string `json:"body"`
						Base64Encoded bool   `json:"base64Encoded"`
					}
					err := c.call(ctx, session, "Network.getResponseBody", map[string]any{"requestId": params.RequestID}, &body)
					text := body.Body
					if err == nil && body.Base64Encoded {
						var data []byte
						data, err = base64.StdEncoding.DecodeString(text)
						text = string(data)
					}
					if err == nil {
						response.ResponseText = &text
						err = json.Unmarshal([]byte(text), &response.ParsedResponse)
					}
					m.result <- composeNetworkResult{response: response, err: err}
					completed = true
				}
			}
		}
	}()
	return m, nil
}

func dispatchBrowserKey(ctx context.Context, c *cdpConnection, session, kind, key, code string, virtualKey, modifiers int, text string) error {
	params := map[string]any{"type": kind, "key": key, "code": code, "windowsVirtualKeyCode": virtualKey, "nativeVirtualKeyCode": virtualKey}
	if modifiers != 0 {
		params["modifiers"] = modifiers
	}
	if text != "" {
		params["text"] = text
		if key == "Enter" {
			params["unmodifiedText"] = text
		}
	}
	return c.call(ctx, session, "Input.dispatchKeyEvent", params, nil)
}

func evaluateBrowserScript(ctx context.Context, c *cdpConnection, session, name string, await bool, result any, arguments ...any) error {
	script, err := renderBrowserScript(name, arguments...)
	if err != nil {
		return err
	}
	return c.evaluate(ctx, session, script, await, result)
}

// PostTweetViaComposeUI は本家と同じモーダル・paste・fileInput・Ctrl+Enter の順で投稿する。
func (b *BrowserBackend) PostTweetViaComposeUI(ctx context.Context, request ComposeRequest) (*ComposeResult, error) {
	c, session, err := b.page()
	if err != nil {
		return nil, err
	}
	started := time.Now()
	var submitted *float64
	var monitor *composeNetworkMonitor
	defer func() {
		if monitor != nil {
			monitor.cancel()
			monitor.subscription.Close()
			<-monitor.done
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.call(cleanup, session, "Network.disable", map[string]any{}, nil)
	}()
	failure := func(message string) (*ComposeResult, error) {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		runes := []rune(message)
		_, _ = b.CaptureDebugScreenshot(cleanup, "compose_error_"+string(runes[:min(len(runes), 50)]))
		_ = dispatchBrowserKey(cleanup, c, session, "keyDown", "Escape", "Escape", 27, 0, "")
		_ = browserWait(cleanup, 500*time.Millisecond)
		var discarded bool
		if evaluateBrowserScript(cleanup, c, session, "discard", false, &discarded) == nil && discarded {
			_ = browserWait(cleanup, 500*time.Millisecond)
		}
		return &ComposeResult{ErrorMessage: &message, ComposeSubmittedAt: submitted}, nil
	}
	unexpected := func(err error) (*ComposeResult, error) {
		return failure(fmt.Sprintf("ツイート送信中に予期しないエラーが発生しました。(%s)", err))
	}
	enableNetwork := func() error {
		if monitor != nil {
			return nil
		}
		var err error
		monitor, err = startComposeNetworkMonitor(ctx, c, session, len(request.Images))
		return err
	}
	if err := c.call(ctx, session, "Page.bringToFront", map[string]any{}, nil); err != nil {
		return unexpected(err)
	}
	if request.InReplyToStatusID == nil {
		if err := c.evaluate(ctx, session, "document.body.focus()", false, nil); err != nil {
			return unexpected(err)
		}
		if err := browserWait(ctx, 100*time.Millisecond); err != nil {
			return unexpected(err)
		}
		if err := dispatchBrowserKey(ctx, c, session, "keyDown", "n", "KeyN", 78, 0, "n"); err != nil {
			return unexpected(err)
		}
		if err := dispatchBrowserKey(ctx, c, session, "keyUp", "n", "KeyN", 78, 0, ""); err != nil {
			return unexpected(err)
		}
	} else {
		id := *request.InReplyToStatusID
		if !regexp.MustCompile(`^[0-9]+$`).MatchString(id) {
			return failure("リプライ先のツイート ID が不正です。")
		}
		alphabet, key := "abcdefghijklmnopqrstuvwxyz0123456789", make([]byte, 6)
		for index := range key {
			key[index] = alphabet[rand.IntN(len(alphabet))]
		}
		state := NewOrderedMap().Set("state", NewOrderedMap().Set("inReplyToStatusId", id)).Set("key", string(key))
		if err := evaluateBrowserScript(ctx, c, session, "reply", false, nil, state, id); err != nil {
			return unexpected(err)
		}
	}
	if err := evaluateBrowserScript(ctx, c, session, "modal_ready", true, nil); err != nil {
		return failure("ツイート送信モーダルが開きませんでした。")
	}
	remaining := max(0, request.ThrottleRemainingSec-time.Since(started).Seconds())
	thinking := math.Round((1+rand.Float64())*1000) / 1000
	if err := browserWait(ctx, time.Duration(max(remaining, thinking)*float64(time.Second))); err != nil {
		return unexpected(err)
	}
	if request.TweetText != "" {
		var pasted bool
		if err := evaluateBrowserScript(ctx, c, session, "paste", false, &pasted, request.TweetText); err != nil || !pasted {
			return failure("テキストの入力に失敗しました。")
		}
	}
	if len(request.Images) > 0 {
		preloaded := make(chan error, 1)
		go func() {
			images := make([]any, 0, len(request.Images))
			for _, image := range request.Images {
				mimeType, name := image.ContentType, image.Filename
				if mimeType == "" {
					mimeType = "image/jpeg"
				}
				if name == "" {
					name = "capture.jpg"
				}
				images = append(images, NewOrderedMap().Set("base64", base64.StdEncoding.EncodeToString(image.Data)).Set("mimeType", mimeType).Set("fileName", name))
			}
			preloaded <- evaluateBrowserScript(ctx, c, session, "preload", false, nil, images)
		}()
		waitErr := browserWait(ctx, time.Duration((0.2+0.2*rand.Float64())*float64(time.Second)))
		if err := <-preloaded; err != nil {
			return unexpected(err)
		}
		if waitErr != nil {
			return unexpected(waitErr)
		}
		if err := enableNetwork(); err != nil {
			return unexpected(err)
		}
		var attached any
		if err := evaluateBrowserScript(ctx, c, session, "attach", false, &attached); err != nil || attached != true {
			detail := fmt.Sprint(attached)
			if err != nil {
				detail = err.Error()
			}
			return failure(fmt.Sprintf("画像の設定に失敗しました。(%s)", detail))
		}
		timer := time.NewTimer(60 * time.Second)
		defer timer.Stop()
		select {
		case <-monitor.uploadFailure:
			return failure("画像のアップロードに失敗しました。")
		case <-monitor.uploadSuccess:
			// 成功と失敗が同時に通知された場合も失敗を優先する。
			select {
			case <-monitor.uploadFailure:
				return failure("画像のアップロードに失敗しました。")
			default:
			}
		case <-timer.C:
			return failure("画像アップロードがタイムアウトしました。")
		case <-ctx.Done():
			return unexpected(ctx.Err())
		case <-c.done:
			return unexpected(ErrBrowserConnectionLost)
		}
	}
	if err := evaluateBrowserScript(ctx, c, session, "button_ready", true, nil); err != nil {
		return failure("ツイートボタンが有効になりませんでした。テキストや画像の入力に問題がある可能性があります。")
	}
	if err := enableNetwork(); err != nil {
		return unexpected(err)
	}
	var focused bool
	if err := evaluateBrowserScript(ctx, c, session, "focus", false, &focused); err != nil || !focused {
		return failure("ツイートテキストエリアへのフォーカスに失敗しました。")
	}
	for _, key := range []struct {
		kind, key, code string
		virtual, mods   int
		text            string
	}{{"keyDown", "Control", "ControlLeft", 17, 2, ""}, {"keyDown", "Enter", "Enter", 13, 2, "\r"},
		{"keyUp", "Enter", "Enter", 13, 2, ""}, {"keyUp", "Control", "ControlLeft", 17, 0, ""}} {
		if err := dispatchBrowserKey(ctx, c, session, key.kind, key.key, key.code, key.virtual, key.mods, key.text); err != nil {
			return unexpected(err)
		}
	}
	now := float64(time.Now().UnixNano()) / 1e9
	submitted = &now
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case result := <-monitor.result:
		if result.err != nil {
			return failure(fmt.Sprintf("CreateTweet のレスポンスを取得できませんでした。(%s)", result.err))
		}
		return &ComposeResult{IsSuccess: true, ComposeSubmittedAt: submitted, GraphQLAPIResult: result.response}, nil
	case <-timer.C:
		return failure("CreateTweet のレスポンスがタイムアウトしました。")
	case <-ctx.Done():
		return unexpected(ctx.Err())
	}
}
