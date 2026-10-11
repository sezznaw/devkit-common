// Package pagex is the one place the paging defaults live. A list request
// carries common.PageReq {page, page_size} (idl/common/common.thrift) and a
// list response carries common.PageInfo {page, page_size, total}; Normalize
// turns the request into what a query needs, with the defaults applied once
// for every service: page 1 when missing, size 20 when missing, size at most
// 100.
//
// pagex 是分页默认值唯一的出处：请求带 common.PageReq，响应带 common.PageInfo，Normalize
// 把请求变成查询要的 offset/limit，默认第 1 页、每页 20、最多 100。
package pagex

// Pager is what the generated common.PageReq satisfies (its getters are
// nil-safe, so a request without a page block is fine).
type Pager interface {
	GetPage() int32
	GetPageSize() int32
}

const (
	DefaultSize int32 = 20
	MaxSize     int32 = 100
)

// Normalize returns the page (from 1), the page size (1..MaxSize, default
// DefaultSize) and the offset a query uses. nil means page 1.
func Normalize(p Pager) (page, size int32, offset int) {
	if p != nil {
		page, size = p.GetPage(), p.GetPageSize()
	}
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = DefaultSize
	}
	if size > MaxSize {
		size = MaxSize
	}
	return page, size, int(page-1) * int(size)
}
