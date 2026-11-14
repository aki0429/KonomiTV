package metadata

import (
	"image"
	"image/color"
	"math"
)

// thumbnailScoreImage は採点中の画像。RGB を詰め、切り出しで元画像の境界を参照しない。
type thumbnailScoreImage struct {
	pixels []byte
	w, h   int
}

func newThumbnailScoreImage(img image.Image) thumbnailScoreImage {
	b := img.Bounds()
	out := thumbnailScoreImage{pixels: make([]byte, b.Dx()*b.Dy()*3), w: b.Dx(), h: b.Dy()}
	for y := range out.h {
		for x := range out.w {
			c := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			p := (y*out.w + x) * 3
			out.pixels[p], out.pixels[p+1], out.pixels[p+2] = c.R, c.G, c.B
		}
	}
	return out
}

func (img thumbnailScoreImage) crop(r image.Rectangle) thumbnailScoreImage {
	out := thumbnailScoreImage{pixels: make([]byte, r.Dx()*r.Dy()*3), w: r.Dx(), h: r.Dy()}
	for y := range out.h {
		copy(out.pixels[y*out.w*3:(y+1)*out.w*3], img.pixels[((y+r.Min.Y)*img.w+r.Min.X)*3:((y+r.Min.Y)*img.w+r.Max.X)*3])
	}
	return out
}

func (img thumbnailScoreImage) gray() []byte {
	out := make([]byte, img.w*img.h)
	// OpenCV の 8 bit RGB2GRAY と同じ BT.601 固定小数点係数。
	for n := range out {
		p := n * 3
		out[n] = byte((int(img.pixels[p])*9798 + int(img.pixels[p+1])*19235 + int(img.pixels[p+2])*3735 + 16384) >> 15)
	}
	return out
}

// thumbnailCanny は aperture=3 / L2gradient=false の Canny。
// Sobel の境界は REPLICATE、非極大抑制とヒステリシスは OpenCV imgproc/canny.cpp に対応する。
func thumbnailCanny(gray []byte, w, h, low, high int) []byte {
	stride := w + 2
	magnitudes := make([]int, (w+2)*(h+2))
	dxs, dys := make([]int, w*h), make([]int, w*h)
	get := func(x, y int) int { return int(gray[max(0, min(h-1, y))*w+max(0, min(w-1, x))]) }
	for y := range h {
		for x := range w {
			dx := get(x+1, y-1) + 2*get(x+1, y) + get(x+1, y+1) - get(x-1, y-1) - 2*get(x-1, y) - get(x-1, y+1)
			dy := get(x-1, y+1) + 2*get(x, y+1) + get(x+1, y+1) - get(x-1, y-1) - 2*get(x, y-1) - get(x+1, y-1)
			dxs[y*w+x], dys[y*w+x] = dx, dy
			magnitudes[(y+1)*stride+x+1] = thumbnailAbs(dx) + thumbnailAbs(dy)
		}
	}
	state := make([]byte, len(magnitudes))
	var stack []int
	for y := range h {
		for x := range w {
			p := (y+1)*stride + x + 1
			m := magnitudes[p]
			if m <= low {
				continue
			}
			dx, dy := dxs[y*w+x], dys[y*w+x]
			tgx, tgy := thumbnailAbs(dx)*13573, thumbnailAbs(dy)<<15
			keep := false
			if tgy < tgx {
				keep = m > magnitudes[p-1] && m >= magnitudes[p+1]
			} else if tgy > tgx+(thumbnailAbs(dx)<<16) {
				keep = m > magnitudes[p-stride] && m >= magnitudes[p+stride]
			} else {
				s := 1
				if (dx ^ dy) < 0 {
					s = -1
				}
				keep = m > magnitudes[p-stride-s] && m > magnitudes[p+stride+s]
			}
			if keep {
				state[p] = 1
				if m > high {
					state[p] = 2
					stack = append(stack, p)
				}
			}
		}
	}
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, d := range [8]int{-stride - 1, -stride, -stride + 1, -1, 1, stride - 1, stride, stride + 1} {
			q := p + d
			if state[q] == 1 {
				state[q] = 2
				stack = append(stack, q)
			}
		}
	}
	out := make([]byte, w*h)
	for y := range h {
		for x := range w {
			if state[(y+1)*stride+x+1] == 2 {
				out[y*w+x] = 255
			}
		}
	}
	return out
}

func thumbnailAbs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// detectThumbnailLetterbox は本家の再帰的な帯幅判定をそのまま維持する。
// 右・下でも部分画像の先頭を調べ、左右の最大幅も画像の高さから求める仕様を含む。
func detectThumbnailLetterbox(img thumbnailScoreImage) (image.Rectangle, bool) {
	minHeight, maxHeight := int(float64(img.h)*.05), int(float64(img.h)*.25)
	if maxHeight == 0 {
		return image.Rect(0, 0, img.w, img.h), true
	}
	check := func(part thumbnailScoreImage, vertical bool) int {
		gray := part.gray()
		edges := thumbnailCanny(gray, part.w, part.h, 50, 150)
		for width := maxHeight; ; width /= 2 {
			dark, totalEdges := 0, 0
			lines, lineSize := min(width, part.h), part.w
			if !vertical {
				lines, lineSize = min(width, part.w), part.h
			}
			for n := range lines {
				lum := 0
				for v := range lineSize {
					p := n*part.w + v
					if !vertical {
						p = v*part.w + n
					}
					lum += int(gray[p])
					totalEdges += int(edges[p])
				}
				if float32(lum)/float32(lineSize) <= 45 {
					dark++
				}
			}
			if float64(dark)/float64(lines) >= .7 && float64(totalEdges)/float64(width*lineSize) <= .05 {
				return width
			}
			if width <= minHeight*2 {
				return 0
			}
		}
	}
	top := check(img.crop(image.Rect(0, 0, img.w, maxHeight)), true)
	bottom := check(img.crop(image.Rect(0, img.h-maxHeight, img.w, img.h)), true)
	left := check(img.crop(image.Rect(0, 0, min(img.w, maxHeight), img.h)), false)
	right := check(img.crop(image.Rect(max(0, img.w-maxHeight), 0, img.w, img.h)), false)
	r := image.Rect(left, top, img.w-right, img.h-bottom)
	return r, 1-float64(r.Dx()*r.Dy())/float64(img.w*img.h) <= .4
}

func thumbnailEdgePenalty(img thumbnailScoreImage) float64 {
	mh, mw := int(float64(img.h)*.08), int(float64(img.w)*.08)
	// -0: は NumPy では全体を意味する。採点入力は通常 480x270。
	bottom, right := img.h-mh, img.w-mw
	if mh == 0 {
		bottom = 0
	}
	if mw == 0 {
		right = 0
	}
	regions := []struct {
		r          image.Rectangle
		horizontal bool
	}{
		{image.Rect(0, 0, img.w, mh), true}, {image.Rect(0, bottom, img.w, img.h), true},
		{image.Rect(0, 0, mw, img.h), false}, {image.Rect(right, 0, img.w, img.h), false},
	}
	penalty := 0.0
	edgeCount := 0
	for _, region := range regions {
		part := img.crop(region.r)
		if part.w == 0 || part.h == 0 {
			continue
		}
		gray := part.gray()
		lines, size := part.h, part.w
		if !region.horizontal {
			lines, size = part.w, part.h
		}
		dark, bright := 0, 0
		for n := range lines {
			d, b := 0, 0
			for v := range size {
				p := n*part.w + v
				if !region.horizontal {
					p = v*part.w + n
				}
				if gray[p] <= 30 {
					d++
				}
				if gray[p] >= 225 {
					b++
				}
			}
			if float64(d)/float64(size) >= .8 {
				dark++
			}
			if float64(b)/float64(size) >= .8 {
				bright++
			}
		}
		ratio := float64(dark) / float64(lines)
		if ratio < .85 {
			ratio = float64(bright) / float64(lines)
		}
		if ratio >= .85 {
			edgeCount++
			penalty = math.Min(penalty+90*ratio/4, 90)
		}
	}
	if edgeCount < 2 {
		return 0
	}
	return penalty
}
