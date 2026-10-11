package pagex

import "testing"

type req struct{ p, s int32 }

func (r *req) GetPage() int32     { return r.p }
func (r *req) GetPageSize() int32 { return r.s }

func TestNormalize(t *testing.T) {
	cases := []struct {
		in         *req
		page, size int32
		offset     int
	}{
		{nil, 1, 20, 0},
		{&req{0, 0}, 1, 20, 0},
		{&req{3, 10}, 3, 10, 20},
		{&req{2, 500}, 2, 100, 100},
		{&req{-1, -1}, 1, 20, 0},
	}
	for _, c := range cases {
		var p Pager
		if c.in != nil {
			p = c.in
		}
		page, size, off := Normalize(p)
		if page != c.page || size != c.size || off != c.offset {
			t.Errorf("Normalize(%+v) = %d %d %d", c.in, page, size, off)
		}
	}
}
