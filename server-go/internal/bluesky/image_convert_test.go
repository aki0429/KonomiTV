package bluesky

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type imageConversionReference struct {
	Commit   string `json:"upstream_commit"`
	Attempts []struct {
		Name          string
		Width, Height int
		Calls         []webPAttempt
		Error         bool
	}
	Images []struct {
		Name, Data string
		Expected   struct {
			Width, Height int
			Pixels        [][]int
		}
	}
}

func imageReference(t *testing.T) imageConversionReference {
	t.Helper()
	data, err := os.ReadFile("testdata/image_conversion_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var ref imageConversionReference
	if err := json.Unmarshal(data, &ref); err != nil {
		t.Fatal(err)
	}
	if ref.Commit != "687b904bb0a2cbb329596fd7b0368c09afe19a6c" || len(ref.Attempts) != 7 || len(ref.Images) != 6 {
		t.Fatal("unexpected conversion reference")
	}
	return ref
}

func TestImageConversionAttemptsMatchUpstream(t *testing.T) {
	for _, test := range imageReference(t).Attempts {
		t.Run(test.Name, func(t *testing.T) {
			attempts := webPAttempts(test.Width, test.Height)
			if len(attempts) < len(test.Calls) || !reflect.DeepEqual(attempts[:len(test.Calls)], test.Calls) {
				t.Fatalf("attempts=%+v want=%+v", attempts, test.Calls)
			}
			if test.Error && len(attempts) != len(test.Calls) {
				t.Fatal("full attempt list differs from upstream")
			}
		})
	}
}

func TestImageConversionNormalizedPixelsMatchUpstream(t *testing.T) {
	for _, test := range imageReference(t).Images {
		t.Run(test.Name, func(t *testing.T) {
			data, err := base64.StdEncoding.DecodeString(test.Data)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			converter := &FFMpegImageConverter{encode: func(ctx context.Context, input []byte, attempt webPAttempt) ([]byte, error) {
				calls++
				img, err := png.Decode(bytes.NewReader(input))
				if err != nil {
					return nil, err
				}
				if img.Bounds().Dx() != test.Expected.Width || img.Bounds().Dy() != test.Expected.Height || attempt.Width != test.Expected.Width || attempt.Height != test.Expected.Height || attempt.Quality != 85 {
					return nil, fmt.Errorf("image geometry or first quality differs")
				}
				for y := 0; y < test.Expected.Height; y++ {
					for x := 0; x < test.Expected.Width; x++ {
						got := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
						pixels := []int{int(got.R), int(got.G), int(got.B), int(got.A)}
						for channel, value := range pixels {
							tolerance := 0
							if test.Name == "CMYK" && channel < 3 {
								tolerance = 2
							}
							want := test.Expected.Pixels[y*test.Expected.Width+x][channel]
							if value < want-tolerance || value > want+tolerance {
								return nil, fmt.Errorf("pixel %d,%d channel%d=%d want=%d", x, y, channel, value, want)
							}
						}
					}
				}
				return []byte("encoded"), nil
			}}
			if _, err := converter.Convert(context.Background(), data); err != nil || calls != 1 {
				t.Fatalf("conversion failed: calls=%d err=%v", calls, err)
			}
		})
	}
}

func imagePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 60, G: 100, B: 200, A: 120})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, img); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestImageConversionSizeBoundariesAndCancellation(t *testing.T) {
	data := imagePNG(t, 4, 2)
	large := make([]byte, maxImageBytes+1)
	for _, success := range []int{1, 5, 6, 30, -1} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			calls := 0
			converter := &FFMpegImageConverter{encode: func(ctx context.Context, input []byte, attempt webPAttempt) ([]byte, error) {
				calls++
				if calls == success {
					return large[:maxImageBytes], nil
				}
				return large, nil
			}}
			converted, err := converter.Convert(context.Background(), data)
			if success > 0 {
				if err != nil || len(converted) != maxImageBytes || calls != success {
					t.Fatalf("bytes=%d calls=%d err=%v", len(converted), calls, err)
				}
			} else if err == nil || calls != 30 {
				t.Fatalf("oversize result: calls=%d err=%v", calls, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	converter := &FFMpegImageConverter{encode: func(context.Context, []byte, webPAttempt) ([]byte, error) { calls++; cancel(); return large, nil }}
	if _, err := converter.Convert(ctx, data); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancellation=%v calls=%d", err, calls)
	}
	if _, err := converter.Convert(ctx, data); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("started after cancellation: %v", err)
	}
	failed := errors.New("encoder error")
	converter.encode = func(context.Context, []byte, webPAttempt) ([]byte, error) { return nil, failed }
	if _, err := converter.Convert(context.Background(), data); !errors.Is(err, failed) {
		t.Fatalf("encoder error lost: %v", err)
	}
	if _, err := converter.Convert(context.Background(), []byte("invalid image")); err == nil {
		t.Fatal("invalid input accepted")
	}
}

func TestPrepareImageUsesOriginalBelowLimitAndFinalAspectRatio(t *testing.T) {
	original := imagePNG(t, 4, 2)
	converted := imagePNG(t, 2, 1)
	for _, size := range []int{maxImageBytes - 1, maxImageBytes, maxImageBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			input := append(append([]byte{}, original...), make([]byte, size-len(original))...)
			received := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/xrpc/com.atproto.repo.uploadBlob" {
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				received <- body
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"blob":{"$type":"blob","ref":{"$link":"fixture"},"mimeType":"image/png","size":100}}`)
			}))
			defer server.Close()
			calls := 0
			manager := NewManager(ManagerOptions{ImageConverter: func(ctx context.Context, data []byte) ([]byte, error) { calls++; return converted, nil }})
			service := &Service{manager: manager, client: NewClient(ClientOptions{BaseURL: server.URL + "/xrpc", HTTPClient: server.Client()})}
			result, err := service.prepareAndUploadImage(context.Background(), ImageInput{Data: input, ContentType: "image/png"})
			if err != nil {
				t.Fatal(err)
			}
			payload := <-received
			want := input
			w, h := 4, 2
			if size > maxImageBytes {
				want = converted
				w, h = 2, 1
				if calls != 1 {
					t.Fatal("large image not converted")
				}
			} else if calls != 0 {
				t.Fatal("small image was recompressed")
			}
			if !bytes.Equal(payload, want) || result.AspectRatio.Width != w || result.AspectRatio.Height != h {
				t.Fatalf("uploaded bytes or aspect ratio changed: %+v", result.AspectRatio)
			}
		})
	}
}

func nativeImageConverter(t *testing.T) *FFMpegImageConverter {
	t.Helper()
	ffmpeg := os.Getenv("KONOMITV_TEST_FFMPEG")
	if ffmpeg == "" {
		t.Skip("KONOMITV_TEST_FFMPEG is not set")
	}
	return &FFMpegImageConverter{FFmpegPath: ffmpeg, LibraryPath: filepath.Dir(ffmpeg)}
}

func TestImageConversionWithRealFFmpegAndLocalPost(t *testing.T) {
	converter := nativeImageConverter(t)
	input := imagePNG(t, 16, 8)
	input = append(input, make([]byte, maxImageBytes+1-len(input))...)
	requests := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/xrpc/com.atproto.repo.uploadBlob":
			decoded, _, err := image.Decode(bytes.NewReader(body))
			if err != nil || decoded.Bounds().Dx() != 16 || decoded.Bounds().Dy() != 8 || len(body) > maxImageBytes {
				t.Errorf("invalid converted image: %v", err)
			}
			if len(body) < 12 || string(body[8:12]) != "WEBP" {
				t.Error("upload was not WebP")
			}
			value := color.NRGBAModel.Convert(decoded.At(8, 4)).(color.NRGBA)
			if value.A != 120 {
				t.Errorf("alpha lost: %d", value.A)
			}
			requests <- "blob"
			io.WriteString(w, `{"blob":{"$type":"blob","ref":{"$link":"fixture"},"mimeType":"image/webp","size":100}}`)
		case "/xrpc/com.atproto.repo.createRecord":
			var record struct {
				Record PostRecord `json:"record"`
			}
			if err := json.Unmarshal(body, &record); err != nil {
				t.Error(err)
			}
			if record.Record.Embed == nil || len(record.Record.Embed.Images) != 1 || record.Record.Embed.Images[0].AspectRatio.Width != 16 || record.Record.Embed.Images[0].AspectRatio.Height != 8 {
				t.Errorf("invalid post embed: %s", body)
			}
			requests <- "post"
			io.WriteString(w, `{"uri":"at://did:plc:fixture/app.bsky.feed.post/fixture","cid":"fixture-cid"}`)
		default:
			t.Errorf("unexpected local API %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	manager := NewManager(ManagerOptions{ImageConverter: converter.Convert})
	client := NewClient(ClientOptions{BaseURL: server.URL + "/xrpc", HTTPClient: server.Client()})
	// ローカルの受け口専用。署名も外部の認証情報も持たないダミー JWT。
	client.session = &Session{DID: "did:plc:fixture", AccessJWT: "fixture." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":4102444800}`)) + ".fixture"}
	service := &Service{manager: manager, client: client, account: &Account{Handle: "fixture.test"}, isSessionRestored: true}
	result, err := service.CreatePost(context.Background(), "local fixture", []ImageInput{{Data: input, ContentType: "image/png"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	post, ok := result.(*PostTweetResult)
	if !ok || !post.IsSuccess {
		t.Fatalf("local post failed: %+v", result)
	}
	if <-requests != "blob" || <-requests != "post" {
		t.Fatal("invalid local upload order")
	}
}

func TestImageConversionResizesWithRealFFmpeg(t *testing.T) {
	converter := nativeImageConverter(t)
	img := image.NewNRGBA(image.Rect(0, 0, 3072, 2048))
	random := rand.New(rand.NewSource(1))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i] = byte(random.Intn(256))
		img.Pix[i+1] = byte(random.Intn(256))
		img.Pix[i+2] = byte(random.Intn(256))
		img.Pix[i+3] = 255
	}
	var input bytes.Buffer
	if err := png.Encode(&input, img); err != nil {
		t.Fatal(err)
	}
	if input.Len() <= maxImageBytes {
		t.Fatal("test image did not exceed limit")
	}
	calls := []webPAttempt{}
	converter.encode = func(ctx context.Context, data []byte, attempt webPAttempt) ([]byte, error) {
		calls = append(calls, attempt)
		return converter.encodeWebP(ctx, data, attempt)
	}
	output, err := converter.Convert(context.Background(), input.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	w, h, err := getImageSize(output)
	if err != nil || len(output) > maxImageBytes || w != 1920 || h != 1280 || len(calls) < 6 {
		t.Fatalf("converted %d bytes %dx%d attempts=%v err=%v", len(output), w, h, calls, err)
	}
	if strings.TrimSpace(string(output[:4])) != "RIFF" {
		t.Fatal("invalid WebP")
	}
}
