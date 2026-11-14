package metadata

import (
	"context"
	"fmt"
	"image"
	"math"
	"sort"
)

type thumbnailFrameScore struct {
	score     float64
	foundFace bool
}

// scoreThumbnailFrame は本家 ThumbnailGenerator.__computeImageScore の評価項目と重みを使う。
// 候補の解像度は 480x270。顔検出を含めて Go 内で完結させる。
func scoreThumbnailFrame(ctx context.Context, img thumbnailScoreImage, mode string) (thumbnailFrameScore, error) {
	if img.w <= 0 || img.h <= 0 {
		return thumbnailFrameScore{}, fmt.Errorf("cannot score empty thumbnail frame")
	}
	if err := ctx.Err(); err != nil {
		return thumbnailFrameScore{}, err
	}
	region, ok := detectThumbnailLetterbox(img)
	if !ok {
		return thumbnailFrameScore{score: -1000}, nil
	}
	letterboxPenalty := 0.0
	if region != image.Rect(0, 0, img.w, img.h) {
		letterboxPenalty = 50
		img = img.crop(region)
	}
	gray := img.gray()
	count := img.w * img.h
	var means, variances [3]float64
	for p, v := range img.pixels {
		means[p%3] += float64(v)
	}
	for c := range means {
		means[c] /= float64(count)
	}
	for p, v := range img.pixels {
		d := float64(v) - means[p%3]
		variances[p%3] += d * d
	}
	solid := true
	for _, v := range variances {
		if v/float64(count) >= 10 {
			solid = false
		}
	}
	solidPenalty := 0.0
	if solid {
		solidPenalty = 100
	}
	result := thumbnailFrameScore{}
	faceScore := 0.0
	if mode == "Human" || mode == "Anime" {
		human, anime, err := loadThumbnailFaceCascades()
		if err != nil {
			return result, err
		}
		detector, factor, neighbors := human, 1.05, 3
		if mode == "Anime" {
			detector, factor, neighbors = anime, 1.01, 2
		}
		faces, err := detector.detect(ctx, gray, img.w, img.h, factor, neighbors)
		if err != nil {
			return result, err
		}
		var auxiliary []image.Rectangle
		if len(faces) > 0 && mode == "Anime" {
			auxiliary, err = human.detect(ctx, gray, img.w, img.h, 1.10, 5)
			if err != nil {
				return result, err
			}
		}
		faceScore, result.foundFace = thumbnailFaceBoost(mode, count, faces, auxiliary)
	}
	// NumPy と同じ float32 輝度を作り、5/95 percentile は線形補間する。
	luminances := make([]float64, count)
	mean := 0.0
	for n := range count {
		p := n * 3
		y := float32(.2126)*float32(img.pixels[p]) + float32(.7152)*float32(img.pixels[p+1]) + float32(.0722)*float32(img.pixels[p+2])
		luminances[n] = float64(y)
		mean += float64(y)
	}
	mean /= float64(count)
	variance := 0.0
	for _, v := range luminances {
		d := v - mean
		variance += d * d
	}
	std := math.Sqrt(variance / float64(count))
	sort.Float64s(luminances)
	percentile := func(q float64) float64 {
		v := float64(count-1) * q
		p := int(v)
		return luminances[p] + (luminances[min(p+1, count-1)]-luminances[p])*(v-float64(p))
	}
	contrast := percentile(.95) - percentile(.05)
	contrast += math.Max(0, 1-math.Abs(mean-127.5)/127.5) * contrast * .1
	// Laplacian は 3 チャンネルそれぞれの ksize=1、境界は REFLECT_101。
	reflect := func(v, size int) int {
		if size <= 1 {
			return 0
		}
		if v < 0 {
			return -v
		}
		if v >= size {
			return 2*size - v - 2
		}
		return v
	}
	get := func(x, y, c int) float64 { return float64(img.pixels[(reflect(y, img.h)*img.w+reflect(x, img.w))*3+c]) }
	lapSum, lapSquares := 0.0, 0.0
	for y := range img.h {
		for x := range img.w {
			for c := range 3 {
				v := get(x-1, y, c) + get(x+1, y, c) + get(x, y-1, c) + get(x, y+1, c) - 4*get(x, y, c)
				lapSum += v
				lapSquares += v * v
			}
		}
	}
	lapMean := lapSum / float64(count*3)
	sharpness := (lapSquares/float64(count*3) - lapMean*lapMean) / 2000
	edges := thumbnailCanny(gray, img.w, img.h, 100, 200)
	nonzero := 0
	for _, v := range edges {
		if v != 0 {
			nonzero++
		}
	}
	edgeDensity := float64(nonzero) / float64(count)
	edgeScore := math.Max(0, 1-math.Abs(edgeDensity-.15)/.1)
	if edgeDensity > .25 {
		edgeScore *= .7
	}
	var histogram [256]int
	for _, v := range gray {
		histogram[v]++
	}
	entropy := 0.0
	for _, n := range histogram {
		if n > 0 {
			p := float64(n) / float64(count)
			entropy -= p * math.Log2(p)
		}
	}
	entropyScore := math.Max(0, 1-math.Abs(entropy-5)/2)
	if entropy > 7 {
		entropyScore *= .7
	}
	weights := [5]float64{.6, .4, .5, .4, .3}
	colorScore := 0.0
	if mode == "Anime" {
		weights = [5]float64{.4, .3, .15, .2, .4}
		colorScore = thumbnailColorBalance(img) * .5
	}
	result.score = std/50*weights[0]*100 + contrast/200*weights[1]*100 + math.Min(sharpness*weights[2]*100, 100) + edgeScore*weights[3]*100 + entropyScore*weights[4]*100 + faceScore + colorScore - solidPenalty - letterboxPenalty - thumbnailEdgePenalty(img)
	return result, nil
}

func thumbnailFaceBoost(mode string, area int, faces, auxiliary []image.Rectangle) (float64, bool) {
	if len(faces) == 0 || mode == "Anime" && len(auxiliary) > 0 {
		return 0, false
	}
	maxArea := 0
	for _, r := range faces {
		maxArea = max(maxArea, r.Dx()*r.Dy())
	}
	ratio := float64(maxArea) / float64(area)
	if mode == "Anime" {
		if ratio > .23 {
			reduction := math.Max(.5, 1-(ratio-.23)/.23*.6)
			ratio = .23 + (ratio-.23)*reduction
		}
		return 20 * ratio * 8, true
	}
	return 20 * ratio * 1.5, true
}

// selectThumbnailFrame は顔ありの候補を優先し、同点では先に現れた候補を保持する。
func selectThumbnailFrame(scores []thumbnailFrameScore, mode string) int {
	best := -1
	for i, value := range scores {
		if best < 0 || (mode != "" && value.foundFace && !scores[best].foundFace) || (value.foundFace == scores[best].foundFace || mode == "") && value.score > scores[best].score {
			best = i
		}
	}
	return best
}
