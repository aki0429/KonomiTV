package frameextract

import (
	"fmt"
	"math"
)

type areaWeight struct {
	index int
	value float32
}

// ResizeArea は RGB24 を OpenCV INTER_AREA と同じ重み・8 bit 丸めで縮小する。
// 片方の軸を拡大する場合も、OpenCV の area_mode の固定小数点補間へ切り替える。
func ResizeArea(input *RGB, width, height int) (*RGB, error) {
	if input == nil || input.Width <= 0 || input.Height <= 0 || width <= 0 || height <= 0 ||
		width > 16384 || height > 16384 || len(input.Pixels) != input.Width*input.Height*3 {
		return nil, fmt.Errorf("invalid RGB resize dimensions")
	}
	out := &RGB{Width: width, Height: height, Pixels: make([]byte, width*height*3), PTS: input.PTS}
	if input.Width == width && input.Height == height {
		copy(out.Pixels, input.Pixels)
		return out, nil
	}
	if input.Width < width || input.Height < height {
		resizeAreaLinear(input, out)
		return out, nil
	}
	sx, sy := input.Width/width, input.Height/height
	if input.Width%width == 0 && input.Height%height == 0 {
		factor := float32(1) / float32(sx*sy)
		for y := range height {
			for x := range width {
				for c := range 3 {
					sum := 0
					for yy := range sy {
						for xx := range sx {
							sum += int(input.Pixels[((y*sy+yy)*input.Width+x*sx+xx)*3+c])
						}
					}
					v := int(math.RoundToEven(float64(float32(sum) * factor)))
					if sx == 2 && sy == 2 {
						v = (sum + 2) >> 2
					}
					out.Pixels[(y*width+x)*3+c] = byte(v)
				}
			}
		}
		return out, nil
	}
	xweights, yweights := areaWeights(input.Width, width), areaWeights(input.Height, height)
	buffer, sums := make([]float32, width*3), make([]float32, width*3)
	for y := range height {
		clear(sums)
		for _, yw := range yweights[y] {
			clear(buffer)
			for x, weights := range xweights {
				for _, xw := range weights {
					p := (yw.index*input.Width + xw.index) * 3
					for c := range 3 {
						// 中間積を明示的に float32 に丸め、Go の積和融合を避ける。
						buffer[x*3+c] += float32(float32(input.Pixels[p+c]) * xw.value)
					}
				}
			}
			for i, v := range buffer {
				sums[i] += float32(v * yw.value)
			}
		}
		for i, v := range sums {
			out.Pixels[y*width*3+i] = byte(max(0, min(255, int(math.RoundToEven(float64(v))))))
		}
	}
	return out, nil
}

func areaWeights(source, destination int) [][]areaWeight {
	table := make([][]areaWeight, destination)
	scale := float64(source) / float64(destination)
	for d := range destination {
		left := float64(d) * scale
		right := left + scale
		cell := min(scale, float64(source)-left)
		first, last := int(math.Ceil(left)), min(int(math.Floor(right)), source-1)
		first = min(first, last)
		if float64(first)-left > 1e-3 {
			table[d] = append(table[d], areaWeight{first - 1, float32((float64(first) - left) / cell)})
		}
		for s := first; s < last; s++ {
			table[d] = append(table[d], areaWeight{s, float32(1 / cell)})
		}
		if right-float64(last) > 1e-3 {
			table[d] = append(table[d], areaWeight{last, float32(min(min(right-float64(last), 1), cell) / cell)})
		}
	}
	return table
}

type linearWeight struct{ index, first, second int }

func areaLinearWeights(source, destination int, horizontal bool) []linearWeight {
	table := make([]linearWeight, destination)
	scale, inverse := float64(source)/float64(destination), float64(destination)/float64(source)
	for d := range destination {
		s := int(math.Floor(float64(d) * scale))
		f := float32(float64(d+1) - float64(s+1)*inverse)
		if f <= 0 {
			f = 0
		} else {
			f -= float32(math.Floor(float64(f)))
		}
		if horizontal && s >= source-1 {
			f, s = 0, source-1
		}
		table[d] = linearWeight{s, int(math.RoundToEven(float64((1 - f) * 2048))), int(math.RoundToEven(float64(f * 2048)))}
	}
	return table
}

func resizeAreaLinear(input, out *RGB) {
	xs, ys := areaLinearWeights(input.Width, out.Width, true), areaLinearWeights(input.Height, out.Height, false)
	for y, yw := range ys {
		for x, xw := range xs {
			for c := range 3 {
				p := func(x, y int) int {
					return int(input.Pixels[(min(y, input.Height-1)*input.Width+min(x, input.Width-1))*3+c])
				}
				a := p(xw.index, yw.index)*xw.first + p(xw.index+1, yw.index)*xw.second
				b := p(xw.index, yw.index+1)*xw.first + p(xw.index+1, yw.index+1)*xw.second
				out.Pixels[(y*out.Width+x)*3+c] = byte((((yw.first * (a >> 4)) >> 16) + ((yw.second * (b >> 4)) >> 16) + 2) >> 2)
			}
		}
	}
}
