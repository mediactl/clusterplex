package pipeline

import (
	"errors"

	"github.com/obinnaokechukwu/ffgo"
)

// composite draws an image subtitle's rectangles onto f, a yuv420p frame,
// each scaled from the canvas (canvasW x canvasH, the source video's size)
// to the frame's, bilinear, and blended by its palette's alpha in BT.709
// limited range.
func composite(f ffgo.Frame, rects []ffgo.SubtitleRect, canvasW, canvasH int) error {
	if len(rects) == 0 {
		return nil
	}
	w := ffgo.WrapFrame(f, ffgo.MediaTypeVideo)
	fw, fh := w.Width(), w.Height()
	if fw <= 0 || fh <= 0 || canvasW <= 0 || canvasH <= 0 {
		return errors.New("composite: no frame or canvas size")
	}
	yp, up, vp := w.Data(0), w.Data(1), w.Data(2)
	ys, us, vs := w.Linesize(0), w.Linesize(1), w.Linesize(2)
	sx, sy := float64(canvasW)/float64(fw), float64(canvasH)/float64(fh)
	for _, r := range rects {
		if r.Width <= 0 || r.Height <= 0 || len(r.Data) < r.LineSize*r.Height || len(r.Palette) == 0 {
			continue
		}
		x0, y0 := int(float64(r.X)/sx), int(float64(r.Y)/sy)
		x1, y1 := min(int(float64(r.X+r.Width)/sx+0.999), fw), min(int(float64(r.Y+r.Height)/sy+0.999), fh)
		for oy := max(y0, 0); oy < y1; oy++ {
			for ox := max(x0, 0); ox < x1; ox++ {
				cr, cg, cb, ca := sample(r, (float64(ox)+0.5)*sx-float64(r.X)-0.5, (float64(oy)+0.5)*sy-float64(r.Y)-0.5)
				if ca <= 0 {
					continue
				}
				y, u, v := yuv709(cr, cg, cb)
				blend(yp, oy*ys+ox, y, ca)
				if ox%2 == 0 && oy%2 == 0 {
					blend(up, (oy/2)*us+ox/2, u, ca)
					blend(vp, (oy/2)*vs+ox/2, v, ca)
				}
			}
		}
	}
	return nil
}

// sample is the rectangle's colour at (x, y) in its own pixels, bilinear
// over its palette's BGRA entries, premultiplied-free: colour and alpha
// each interpolated.
func sample(r ffgo.SubtitleRect, x, y float64) (cr, cg, cb, ca float64) {
	at := func(px, py int) (float64, float64, float64, float64) {
		if px < 0 || py < 0 || px >= r.Width || py >= r.Height {
			return 0, 0, 0, 0
		}
		i := 4 * int(r.Data[py*r.LineSize+px])
		if i+3 >= len(r.Palette) {
			return 0, 0, 0, 0
		}
		p := r.Palette[i:]
		return float64(p[2]), float64(p[1]), float64(p[0]), float64(p[3]) / 255
	}
	ix, iy := floor(x), floor(y)
	fx, fy := x-float64(ix), y-float64(iy)
	for _, c := range [4]struct {
		dx, dy int
		w      float64
	}{{0, 0, (1 - fx) * (1 - fy)}, {1, 0, fx * (1 - fy)}, {0, 1, (1 - fx) * fy}, {1, 1, fx * fy}} {
		r0, g0, b0, a0 := at(ix+c.dx, iy+c.dy)
		cr += r0 * a0 * c.w
		cg += g0 * a0 * c.w
		cb += b0 * a0 * c.w
		ca += a0 * c.w
	}
	if ca > 0 {
		cr, cg, cb = cr/ca, cg/ca, cb/ca
	}
	return cr, cg, cb, ca
}

func floor(v float64) int {
	i := int(v)
	if v < 0 && float64(i) != v {
		i--
	}
	return i
}

// yuv709 is an RGB colour in BT.709 limited-range Y'CbCr.
func yuv709(r, g, b float64) (y, u, v float64) {
	y = 16 + 0.1826*r + 0.6142*g + 0.0620*b
	u = 128 - 0.1006*r - 0.3386*g + 0.4392*b
	v = 128 + 0.4392*r - 0.3989*g - 0.0403*b
	return y, u, v
}

func blend(plane []byte, i int, c, a float64) {
	if i < 0 || i >= len(plane) {
		return
	}
	plane[i] = byte(float64(plane[i])*(1-a) + c*a + 0.5)
}
