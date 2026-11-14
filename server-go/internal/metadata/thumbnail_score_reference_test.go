package metadata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
)

// TestThumbnailScoringReference は固定した本家の実メソッド・OpenCV 5.0 の出力と比較する。
// 明暗・黒帯・単色・画面端・ノイズに加え、パブリックドメインの NASA 人物写真を含む。
func TestThumbnailScoringReference(t *testing.T) {
	var fixture struct {
		Models map[string]string `json:"models"`
		Images []struct {
			Name      string `json:"name"`
			Gray      string `json:"gray_sha256"`
			Canny50   string `json:"canny50_sha256"`
			Canny100  string `json:"canny100_sha256"`
			Letterbox []int  `json:"letterbox"`
			Scores    map[string]struct {
				Min, Max  float64
				FoundFace bool `json:"found_face"`
			} `json:"scores"`
			Faces map[string][][4]int `json:"faces"`
		} `json:"images"`
	}
	base := filepath.Join("testdata", "thumbnail_scoring")
	data, err := os.ReadFile(filepath.Join(base, "reference.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	human, anime, err := loadThumbnailFaceCascades()
	if err != nil {
		t.Fatal(err)
	}
	for name, bytes := range map[string][]byte{"haarcascade_frontalface_default.xml": humanFaceCascadeXML, "lbpcascade_animeface.xml": animeFaceCascadeXML} {
		hash := sha256.Sum256(bytes)
		if hex.EncodeToString(hash[:]) != fixture.Models[name] {
			t.Fatalf("model %s changed", name)
		}
	}
	for _, entry := range fixture.Images {
		t.Run(entry.Name, func(t *testing.T) {
			f, err := os.Open(filepath.Join(base, entry.Name+".png"))
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := png.Decode(f)
			_ = f.Close()
			if err != nil {
				t.Fatal(err)
			}
			img := newThumbnailScoreImage(decoded)
			gray := img.gray()
			for _, check := range []struct {
				name     string
				data     []byte
				expected string
			}{
				{"gray", gray, entry.Gray}, {"canny50", thumbnailCanny(gray, img.w, img.h, 50, 150), entry.Canny50}, {"canny100", thumbnailCanny(gray, img.w, img.h, 100, 200), entry.Canny100},
			} {
				h := sha256.Sum256(check.data)
				if got := hex.EncodeToString(h[:]); got != check.expected {
					t.Errorf("%s hash=%s want=%s", check.name, got, check.expected)
				}
			}
			r, ok := detectThumbnailLetterbox(img)
			if entry.Letterbox == nil {
				if ok {
					t.Errorf("letterbox=%v expected rejected", r)
				}
			} else if !ok || r != image.Rect(entry.Letterbox[0], entry.Letterbox[1], entry.Letterbox[2], entry.Letterbox[3]) {
				t.Errorf("letterbox=%v/%v want=%v", r, ok, entry.Letterbox)
			}
			for _, mode := range []string{"None", "Human", "Anime"} {
				m := mode
				if m == "None" {
					m = ""
				}
				got, err := scoreThumbnailFrame(context.Background(), img, m)
				if err != nil {
					t.Fatal(err)
				}
				want := entry.Scores[mode]
				// NumPy float32 の集約と Go float64 の差、および色クラスタの乱数・Lab 丸め差を許容する。
				tolerance := .003
				if mode == "Anime" {
					tolerance = .08
				}
				if got.foundFace != want.FoundFace || got.score < want.Min-tolerance || got.score > want.Max+tolerance {
					t.Errorf("%s score=%.9f face=%v want=[%.9f,%.9f] face=%v", mode, got.score, got.foundFace, want.Min, want.Max, want.FoundFace)
				}
			}
			for _, check := range []struct {
				name      string
				c         *thumbnailCascade
				factor    float64
				neighbors int
			}{
				{"Human", human, 1.05, 3}, {"Anime", anime, 1.01, 2}, {"Auxiliary", human, 1.10, 5},
			} {
				got, err := check.c.detect(context.Background(), gray, img.w, img.h, check.factor, check.neighbors)
				if err != nil {
					t.Fatal(err)
				}
				values := make([][4]int, 0, len(got))
				for _, r := range got {
					values = append(values, [4]int{r.Min.X, r.Min.Y, r.Dx(), r.Dy()})
				}
				sort.Slice(values, func(i, j int) bool {
					for n := range 4 {
						if values[i][n] != values[j][n] {
							return values[i][n] < values[j][n]
						}
					}
					return false
				})
				if !reflect.DeepEqual(values, entry.Faces[check.name]) {
					t.Errorf("%s faces=%v want=%v", check.name, values, entry.Faces[check.name])
				}
			}
		})
	}
}

func TestThumbnailFrameSelection(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		scores     []thumbnailFrameScore
		want       int
	}{
		{"empty", "", nil, -1}, {"highest", "", []thumbnailFrameScore{{score: 20}, {score: 40}, {score: 10}}, 1},
		{"first tie", "Human", []thumbnailFrameScore{{score: 40}, {score: 40}}, 0},
		{"face priority", "Anime", []thumbnailFrameScore{{score: 200}, {score: -20, foundFace: true}, {score: 100}}, 1},
		{"best face", "Human", []thumbnailFrameScore{{score: 50, foundFace: true}, {score: 100}, {score: 60, foundFace: true}}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectThumbnailFrame(tc.scores, tc.mode); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := scoreThumbnailFrame(ctx, thumbnailScoreImage{w: 1, h: 1, pixels: []byte{0, 0, 0}}, "Human"); err == nil {
		t.Fatal("canceled scoring succeeded")
	}
	if _, err := parseThumbnailCascade([]byte("<opencv_storage/>")); err == nil {
		t.Fatal("invalid cascade accepted")
	}
	if math.IsNaN(thumbnailColorBalance(thumbnailScoreImage{w: 1, h: 1, pixels: []byte{127, 127, 127}})) {
		t.Fatal("invalid color balance")
	}
}

// TestThumbnailFaceBoostReference は本家に固定した検出矩形を渡して生成した面積加点の境界値。
func TestThumbnailFaceBoostReference(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Mode             string `json:"mode"`
			Area             int    `json:"image_area"`
			Faces, Auxiliary [][4]int
			Boost            float64 `json:"boost"`
			Found            bool    `json:"found_face"`
		} `json:"face_boost_cases"`
	}
	data, err := os.ReadFile(filepath.Join("testdata", "thumbnail_scoring", "reference.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 24 {
		t.Fatalf("face boost fixture count=%d", len(fixture.Cases))
	}
	convert := func(values [][4]int) []image.Rectangle {
		var rects []image.Rectangle
		for _, v := range values {
			rects = append(rects, image.Rect(v[0], v[1], v[0]+v[2], v[1]+v[3]))
		}
		return rects
	}
	for _, tc := range fixture.Cases {
		boost, found := thumbnailFaceBoost(tc.Mode, tc.Area, convert(tc.Faces), convert(tc.Auxiliary))
		if found != tc.Found || math.Abs(boost-tc.Boost) > .0001 {
			t.Errorf("mode=%s faces=%v auxiliary=%v boost=%v/%v want=%v/%v", tc.Mode, tc.Faces, tc.Auxiliary, boost, found, tc.Boost, tc.Found)
		}
	}
}

func TestThumbnailFaceCascadeConcurrent(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "thumbnail_scoring", "face-60.png"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	frame := newThumbnailScoreImage(img)
	gray := frame.gray()
	human, anime, err := loadThumbnailFaceCascades()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for n := range 8 {
		wg.Go(func() {
			model, factor, neighbors := human, 1.05, 3
			if n%2 != 0 {
				model, factor, neighbors = anime, 1.01, 2
			}
			faces, err := model.detect(context.Background(), gray, frame.w, frame.h, factor, neighbors)
			if err != nil || len(faces) != 1 {
				t.Errorf("concurrent detector faces=%v error=%v", faces, err)
			}
		})
	}
	wg.Wait()
}
