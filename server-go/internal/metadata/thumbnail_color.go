package metadata

import (
	"math"
	"math/rand/v2"
)

// thumbnailLabColor は OpenCV の 8 bit Lab 表現 (L*255/100,a+128,b+128)。
type thumbnailLabColor [3]float64

func thumbnailRGBToLab(r, g, b byte) thumbnailLabColor {
	gamma := func(v byte) float64 {
		x := float64(v) / 255
		if x <= .04045 {
			return x / 12.92
		}
		return math.Pow((x+.055)/1.055, 2.4)
	}
	rr, gg, bb := gamma(r), gamma(g), gamma(b)
	f := func(x float64) float64 {
		if x > 216.0/24389 {
			return math.Cbrt(x)
		}
		return (24389.0/27*x + 16) / 116
	}
	x := f((.412453*rr + .357580*gg + .180423*bb) / .950456)
	y := f(.212671*rr + .715160*gg + .072169*bb)
	z := f((.019334*rr + .119193*gg + .950227*bb) / 1.088754)
	return thumbnailLabColor{math.RoundToEven((116*y - 16) * 255 / 100), math.RoundToEven(500*(x-y) + 128), math.RoundToEven(200*(y-z) + 128)}
}

func thumbnailColorDistance(a, b thumbnailLabColor) float64 {
	d := 0.0
	for n := range 3 {
		v := a[n] - b[n]
		d += v * v
	}
	return d
}

// thumbnailColorBalance は 128² 以下へ縮小し、k-means++ / 3 色 / 10 試行で分布を評価する。
// 本家は OpenCV の共有乱数状態を使用するが、Go は候補ごとに固定 seed とし並行処理でも再現可能にする。
func thumbnailColorBalance(img thumbnailScoreImage) float64 {
	scale := math.Min(1, math.Sqrt(128*128/float64(img.w*img.h)))
	w, h := int(math.RoundToEven(float64(img.w)*scale)), int(math.RoundToEven(float64(img.h)*scale))
	pixels := make([]thumbnailLabColor, w*h)
	if len(pixels) < 3 {
		return .2
	}
	for y := range h {
		for x := range w {
			sx, sy := (float64(x)+.5)/scale-.5, (float64(y)+.5)/scale-.5
			ix, iy := int(math.Floor(sx)), int(math.Floor(sy))
			fx, fy := sx-float64(ix), sy-float64(iy)
			get := func(x, y, c int) float64 {
				return float64(img.pixels[(max(0, min(img.h-1, y))*img.w+max(0, min(img.w-1, x)))*3+c])
			}
			var rgb [3]byte
			for c := range 3 {
				rgb[c] = byte(math.RoundToEven((get(ix, iy, c)*(1-fx)+get(ix+1, iy, c)*fx)*(1-fy) + (get(ix, iy+1, c)*(1-fx)+get(ix+1, iy+1, c)*fx)*fy))
			}
			pixels[y*w+x] = thumbnailRGBToLab(rgb[0], rgb[1], rgb[2])
		}
	}
	rng := rand.New(rand.NewPCG(7281, 5179))
	var bestCenters [3]thumbnailLabColor
	var bestCounts [3]int
	bestCost := math.Inf(1)
	for range 10 {
		centers := [3]thumbnailLabColor{pixels[rng.IntN(len(pixels))]}
		distances := make([]float64, len(pixels))
		for n := 1; n < 3; n++ {
			total := 0.0
			for i, p := range pixels {
				d := thumbnailColorDistance(p, centers[n-1])
				if n == 1 || d < distances[i] {
					distances[i] = d
				}
				total += distances[i]
			}
			// OpenCV の k-means++ と同じく 3 回試して、距離和が最小の追加中心を選ぶ。
			candidateCost := math.Inf(1)
			for range 3 {
				v := rng.Float64() * total
				index := len(pixels) - 1
				for i, d := range distances {
					v -= d
					if v <= 0 {
						index = i
						break
					}
				}
				cost := 0.0
				for i, p := range pixels {
					cost += math.Min(distances[i], thumbnailColorDistance(p, pixels[index]))
				}
				if cost < candidateCost {
					candidateCost = cost
					centers[n] = pixels[index]
				}
			}
		}
		labels := make([]int, len(pixels))
		var counts [3]int
		for iteration := 0; iteration < 10; iteration++ {
			var sums [3]thumbnailLabColor
			counts = [3]int{}
			for i, p := range pixels {
				best, dist := 0, thumbnailColorDistance(p, centers[0])
				for c := 1; c < 3; c++ {
					d := thumbnailColorDistance(p, centers[c])
					if d < dist {
						best, dist = c, d
					}
				}
				labels[i] = best
				counts[best]++
				for j := range 3 {
					sums[best][j] += p[j]
				}
			}
			// 空のクラスタには最大クラスタの最遠点を移す。
			for c := range 3 {
				if counts[c] == 0 {
					largest := 0
					for n := 1; n < 3; n++ {
						if counts[n] > counts[largest] {
							largest = n
						}
					}
					var center thumbnailLabColor
					for j := range 3 {
						center[j] = sums[largest][j] / float64(counts[largest])
					}
					farthest, distance := 0, -1.0
					for i, p := range pixels {
						if labels[i] == largest {
							d := thumbnailColorDistance(p, center)
							if d >= distance {
								farthest, distance = i, d
							}
						}
					}
					labels[farthest] = c
					counts[largest]--
					counts[c]++
					for j := range 3 {
						sums[largest][j] -= pixels[farthest][j]
						sums[c][j] += pixels[farthest][j]
					}
				}
			}
			shift := 0.0
			for c := range 3 {
				for j := range 3 {
					sums[c][j] /= float64(counts[c])
				}
				shift = math.Max(shift, thumbnailColorDistance(sums[c], centers[c]))
				centers[c] = sums[c]
			}
			if iteration > 0 && shift <= 1 {
				break
			}
		}
		cost := 0.0
		for i, p := range pixels {
			cost += thumbnailColorDistance(p, centers[labels[i]])
		}
		if cost < bestCost {
			bestCost, bestCenters, bestCounts = cost, centers, counts
		}
	}
	var ratios [3]float64
	significant := 0
	for n, c := range bestCounts {
		ratios[n] = float64(c) / float64(len(pixels))
		if ratios[n] > .7 {
			return .2
		}
		if ratios[n] >= .15 {
			significant++
		}
	}
	if significant < 2 {
		return .3
	}
	distance := math.Inf(1)
	for i := range 3 {
		for j := i + 1; j < 3; j++ {
			if ratios[i] >= .15 && ratios[j] >= .15 {
				distance = math.Min(distance, math.Sqrt(thumbnailColorDistance(bestCenters[i], bestCenters[j])))
			}
		}
	}
	if distance < 30 {
		return .4
	}
	variance := 0.0
	for _, p := range ratios {
		variance += (p - 1.0/3) * (p - 1.0/3)
	}
	return float64(significant)/3*.4 + math.Min(distance/100, 1)*.3 + (1-math.Abs(math.Sqrt(variance/3)-.2))*.3
}
