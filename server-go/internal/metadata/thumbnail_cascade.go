package metadata

import (
	"context"
	_ "embed"
	"encoding/xml"
	"fmt"
	"image"
	"math"
	"strconv"
	"strings"
	"sync"
)

// モデルは本家と同じ XML を埋め込み、Python / OpenCV のインストールを不要にする。
// XML 内の Intel / nagadomi のライセンス表示を保持する。
//
//go:embed data/haarcascade_frontalface_default.xml
var humanFaceCascadeXML []byte

//go:embed data/lbpcascade_animeface.xml
var animeFaceCascadeXML []byte

// thumbnailCascade は同梱の HAAR / LBP stump モデルだけを扱う読み取り専用検出器。
// 評価・スケール走査・矩形統合は OpenCV xobjdetect の cascadedetect.{cpp,hpp} に対応する。
type thumbnailCascade struct {
	kind          string
	width, height int
	stages        []thumbnailCascadeStage
	features      []thumbnailCascadeFeature
}

type thumbnailCascadeStage struct {
	threshold float32
	stumps    []thumbnailCascadeStump
}

type thumbnailCascadeStump struct {
	feature                int
	threshold, left, right float32
	subset                 [8]uint32
}

type thumbnailCascadeFeature struct {
	rects []thumbnailHaarRectangle
	lbp   image.Rectangle
}

type thumbnailHaarRectangle struct {
	image.Rectangle
	weight float32
}

var thumbnailFaceCascades = sync.OnceValues(func() ([2]*thumbnailCascade, error) {
	human, err := parseThumbnailCascade(humanFaceCascadeXML)
	if err != nil {
		return [2]*thumbnailCascade{}, fmt.Errorf("load human face cascade: %w", err)
	}
	anime, err := parseThumbnailCascade(animeFaceCascadeXML)
	if err != nil {
		return [2]*thumbnailCascade{}, fmt.Errorf("load anime face cascade: %w", err)
	}
	return [2]*thumbnailCascade{human, anime}, nil
})

func loadThumbnailFaceCascades() (*thumbnailCascade, *thumbnailCascade, error) {
	models, err := thumbnailFaceCascades()
	return models[0], models[1], err
}

func parseThumbnailCascade(data []byte) (*thumbnailCascade, error) {
	var model struct {
		Cascade struct {
			StageType string `xml:"stageType"`
			Kind      string `xml:"featureType"`
			Width     int    `xml:"width"`
			Height    int    `xml:"height"`
			Stages    []struct {
				Threshold float32 `xml:"stageThreshold"`
				Weak      []struct {
					Nodes  string `xml:"internalNodes"`
					Leaves string `xml:"leafValues"`
				} `xml:"weakClassifiers>_"`
			} `xml:"stages>_"`
			Features []struct {
				Rects  []string `xml:"rects>_"`
				Rect   string   `xml:"rect"`
				Tilted int      `xml:"tilted"`
			} `xml:"features>_"`
		} `xml:"cascade"`
	}
	if err := xml.Unmarshal(data, &model); err != nil {
		return nil, fmt.Errorf("decode cascade XML: %w", err)
	}
	m := model.Cascade
	if m.StageType != "BOOST" || (m.Kind != "HAAR" && m.Kind != "LBP") || m.Width != 24 || m.Height != 24 || len(m.Stages) == 0 || len(m.Features) == 0 {
		return nil, fmt.Errorf("unsupported thumbnail cascade")
	}
	c := &thumbnailCascade{kind: m.Kind, width: m.Width, height: m.Height}
	parseNumbers := func(value string) ([]float64, error) {
		fields := strings.Fields(value)
		result := make([]float64, len(fields))
		for i, field := range fields {
			n, err := strconv.ParseFloat(field, 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
				return nil, fmt.Errorf("invalid cascade number %q", field)
			}
			result[i] = n
		}
		return result, nil
	}
	for _, feature := range m.Features {
		f := thumbnailCascadeFeature{}
		if feature.Tilted != 0 {
			return nil, fmt.Errorf("tilted cascade feature is not supported")
		}
		if c.kind == "LBP" {
			n, err := parseNumbers(feature.Rect)
			if err != nil || len(n) != 4 {
				return nil, fmt.Errorf("invalid LBP rectangle")
			}
			f.lbp = image.Rect(int(n[0]), int(n[1]), int(n[0]+n[2]), int(n[1]+n[3]))
			if f.lbp.Empty() || f.lbp.Min.X < 0 || f.lbp.Min.Y < 0 || f.lbp.Min.X+3*f.lbp.Dx() > c.width || f.lbp.Min.Y+3*f.lbp.Dy() > c.height {
				return nil, fmt.Errorf("LBP rectangle outside cascade window")
			}
		} else {
			if len(feature.Rects) < 2 || len(feature.Rects) > 3 {
				return nil, fmt.Errorf("invalid HAAR rectangle count")
			}
			for _, value := range feature.Rects {
				n, err := parseNumbers(value)
				if err != nil || len(n) != 5 {
					return nil, fmt.Errorf("invalid HAAR rectangle")
				}
				r := image.Rect(int(n[0]), int(n[1]), int(n[0]+n[2]), int(n[1]+n[3]))
				if r.Empty() || !r.In(image.Rect(0, 0, c.width, c.height)) {
					return nil, fmt.Errorf("HAAR rectangle outside cascade window")
				}
				f.rects = append(f.rects, thumbnailHaarRectangle{r, float32(n[4])})
			}
		}
		c.features = append(c.features, f)
	}
	for _, stage := range m.Stages {
		s := thumbnailCascadeStage{threshold: stage.Threshold - float32(1e-5)}
		for _, weak := range stage.Weak {
			n, err := parseNumbers(weak.Nodes)
			if err != nil {
				return nil, err
			}
			leaves, err := parseNumbers(weak.Leaves)
			want := 4
			if c.kind == "LBP" {
				want = 11
			}
			if err != nil || len(n) != want || len(leaves) != 2 || n[0] != 0 || n[1] != -1 || n[2] < 0 || n[2] >= float64(len(c.features)) || n[2] != math.Trunc(n[2]) {
				return nil, fmt.Errorf("unsupported cascade decision tree")
			}
			stump := thumbnailCascadeStump{feature: int(n[2]), left: float32(leaves[0]), right: float32(leaves[1])}
			if c.kind == "LBP" {
				for i := range stump.subset {
					stump.subset[i] = uint32(int32(n[3+i]))
				}
			} else {
				stump.threshold = float32(n[3])
			}
			s.stumps = append(s.stumps, stump)
		}
		if len(s.stumps) == 0 {
			return nil, fmt.Errorf("empty cascade stage")
		}
		c.stages = append(c.stages, s)
	}
	return c, nil
}

// thumbnailIntegral は縮小画像の積分画像。二乗和は 32 bit の桁あふれを避ける。
type thumbnailIntegral struct {
	stride      int
	sum, square []int64
}

func newThumbnailIntegral(gray []byte, w, h int, squared bool) thumbnailIntegral {
	i := thumbnailIntegral{stride: w + 1, sum: make([]int64, (w+1)*(h+1))}
	if squared {
		i.square = make([]int64, len(i.sum))
	}
	for y := range h {
		var sum, square int64
		for x := range w {
			v := int64(gray[y*w+x])
			sum += v
			square += v * v
			p := (y+1)*i.stride + x + 1
			i.sum[p] = i.sum[p-i.stride] + sum
			if squared {
				i.square[p] = i.square[p-i.stride] + square
			}
		}
	}
	return i
}

func (i thumbnailIntegral) rectangle(data []int64, x, y, w, h int) int64 {
	p := y*i.stride + x
	return data[p] + data[p+h*i.stride+w] - data[p+w] - data[p+h*i.stride]
}

type thumbnailCascadeOffsets struct {
	rects   [3][4]int
	weights [3]float32
	lbp     [16]int
}

func (c *thumbnailCascade) compile(stride int) []thumbnailCascadeOffsets {
	features := make([]thumbnailCascadeOffsets, len(c.features))
	for n, f := range c.features {
		for j, r := range f.rects {
			p := r.Min.Y*stride + r.Min.X
			features[n].rects[j] = [4]int{p, p + r.Dx(), p + r.Dy()*stride, p + r.Dy()*stride + r.Dx()}
			features[n].weights[j] = r.weight
		}
		for y := range 4 {
			for x := range 4 {
				features[n].lbp[y*4+x] = (f.lbp.Min.Y+y*f.lbp.Dy())*stride + f.lbp.Min.X + x*f.lbp.Dx()
			}
		}
	}
	return features
}

func (c *thumbnailCascade) runAt(i thumbnailIntegral, features []thumbnailCascadeOffsets, x, y int) int {
	norm := float32(1)
	if c.kind == "HAAR" {
		area := float64((c.width - 2) * (c.height - 2))
		s := float64(i.rectangle(i.sum, x+1, y+1, c.width-2, c.height-2))
		q := float64(i.rectangle(i.square, x+1, y+1, c.width-2, c.height-2))
		variance := area*q - s*s
		if variance <= 0 {
			return -1
		}
		norm = float32(1 / math.Sqrt(variance))
		if area*float64(norm) >= .1 {
			return -1
		}
	}
	p := y*i.stride + x
	data := i.sum[p:]
	for stageIndex, stage := range c.stages {
		score := float64(0)
		for _, stump := range stage.stumps {
			f := &features[stump.feature]
			left := false
			if c.kind == "HAAR" {
				var value float32
				for j, weight := range f.weights {
					if weight != 0 {
						r := f.rects[j]
						value += weight * float32(data[r[0]]-data[r[1]]-data[r[2]]+data[r[3]])
					}
				}
				left = value*norm < stump.threshold
			} else {
				sum := func(a, b, d, e int) int64 { return data[f.lbp[a]] - data[f.lbp[b]] - data[f.lbp[d]] + data[f.lbp[e]] }
				center := sum(5, 6, 9, 10)
				code := 0
				for j, pos := range [8]int{0, 1, 2, 6, 10, 9, 8, 4} {
					if sum(pos, pos+1, pos+4, pos+5) >= center {
						code |= 1 << (7 - j)
					}
				}
				left = stump.subset[code>>5]&(1<<(code&31)) != 0
			}
			if left {
				score += float64(stump.left)
			} else {
				score += float64(stump.right)
			}
		}
		if score < float64(stage.threshold) {
			return -stageIndex
		}
	}
	return 1
}

// detect は INTER_LINEAR_EXACT の画像ピラミッドを走査し、minNeighbors を超える群を返す。
func (c *thumbnailCascade) detect(ctx context.Context, gray []byte, w, h int, factor float64, neighbors int) ([]image.Rectangle, error) {
	var rectangles []image.Rectangle
	for scale := 1.0; int(math.RoundToEven(float64(c.width)*scale)) <= w && int(math.RoundToEven(float64(c.height)*scale)) <= h; scale *= factor {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sc := float32(scale)
		sw, sh := int(math.RoundToEven(float64(float32(w)/sc))), int(math.RoundToEven(float64(float32(h)/sc)))
		resized := resizeThumbnailGrayExact(gray, w, h, sw, sh)
		i := newThumbnailIntegral(resized, sw, sh, c.kind == "HAAR")
		features := c.compile(i.stride)
		step := 2
		if sc >= 2 {
			step = 1
		}
		winW, winH := int(math.RoundToEven(float64(float32(c.width)*sc))), int(math.RoundToEven(float64(float32(c.height)*sc)))
		for y := 0; y < sh+1-c.height; y += step {
			for x := 0; x < sw+1-c.width; x += step {
				result := c.runAt(i, features, x, y)
				if result > 0 {
					rx, ry := int(math.RoundToEven(float64(float32(x)*sc))), int(math.RoundToEven(float64(float32(y)*sc)))
					rectangles = append(rectangles, image.Rect(rx, ry, rx+winW, ry+winH))
				}
				if result == 0 {
					x += step
				}
			}
		}
	}
	grouped := groupThumbnailRectangles(rectangles, neighbors)
	for n, r := range grouped {
		grouped[n] = r.Intersect(image.Rect(0, 0, w, h))
	}
	return grouped, nil
}

func resizeThumbnailGrayExact(src []byte, w, h, dw, dh int) []byte {
	type coefficient struct{ p, q, weight int }
	axis := func(srcSize, dstSize int) []coefficient {
		out := make([]coefficient, dstSize)
		for n := range dstSize {
			v := (float64(n)+.5)*float64(srcSize)/float64(dstSize) - .5
			p := int(math.Floor(v))
			weight := int(math.RoundToEven((v - float64(p)) * 256))
			if p < 0 {
				p, weight = 0, 0
			}
			if p >= srcSize-1 {
				p, weight = srcSize-1, 0
			}
			out[n] = coefficient{p, min(p+1, srcSize-1), weight}
		}
		return out
	}
	xs, ys := axis(w, dw), axis(h, dh)
	out := make([]byte, dw*dh)
	for y, b := range ys {
		for x, a := range xs {
			top := int(src[b.p*w+a.p])*(256-a.weight) + int(src[b.p*w+a.q])*a.weight
			bottom := int(src[b.q*w+a.p])*(256-a.weight) + int(src[b.q*w+a.q])*a.weight
			out[y*dw+x] = byte((top*(256-b.weight) + bottom*b.weight + 32768) >> 16)
		}
	}
	return out
}

func groupThumbnailRectangles(rects []image.Rectangle, threshold int) []image.Rectangle {
	if threshold <= 0 {
		return rects
	}
	parent := make([]int, len(rects))
	for i := range parent {
		parent[i] = i
	}
	var root func(int) int
	root = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	for i, a := range rects {
		for j := 0; j < i; j++ {
			b := rects[j]
			delta := .2 * float64(min(a.Dx(), b.Dx())+min(a.Dy(), b.Dy())) * .5
			if math.Abs(float64(a.Min.X-b.Min.X)) <= delta && math.Abs(float64(a.Min.Y-b.Min.Y)) <= delta && math.Abs(float64(a.Max.X-b.Max.X)) <= delta && math.Abs(float64(a.Max.Y-b.Max.Y)) <= delta {
				parent[root(i)] = root(j)
			}
		}
	}
	type group struct{ x, y, w, h, count int }
	groups := map[int]int{}
	var values []group
	for i, r := range rects {
		key := root(i)
		n, ok := groups[key]
		if !ok {
			n = len(values)
			groups[key] = n
			values = append(values, group{})
		}
		g := &values[n]
		g.x += r.Min.X
		g.y += r.Min.Y
		g.w += r.Dx()
		g.h += r.Dy()
		g.count++
	}
	averages := make([]image.Rectangle, len(values))
	for n, g := range values {
		s := float32(1) / float32(g.count)
		x, y, w, h := int(math.RoundToEven(float64(float32(g.x)*s))), int(math.RoundToEven(float64(float32(g.y)*s))), int(math.RoundToEven(float64(float32(g.w)*s))), int(math.RoundToEven(float64(float32(g.h)*s)))
		averages[n] = image.Rect(x, y, x+w, y+h)
	}
	var result []image.Rectangle
	for i, r := range averages {
		if values[i].count <= threshold {
			continue
		}
		nested := false
		for j, other := range averages {
			if i == j || values[j].count <= threshold {
				continue
			}
			dx, dy := int(math.RoundToEven(float64(other.Dx())*.2)), int(math.RoundToEven(float64(other.Dy())*.2))
			if r.Min.X >= other.Min.X-dx && r.Min.Y >= other.Min.Y-dy && r.Max.X <= other.Max.X+dx && r.Max.Y <= other.Max.Y+dy && (values[j].count > max(3, values[i].count) || values[i].count < 3) {
				nested = true
				break
			}
		}
		if !nested {
			result = append(result, r)
		}
	}
	return result
}
