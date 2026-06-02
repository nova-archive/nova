package httputil

import (
	"fmt"
	"net/http"
	"strconv"
)

// Pagination defaults and caps (the openapi PageParam/PerPageParam contract).
const (
	defaultPerPage = 50
	maxPerPage     = 100
)

// Page is parsed pagination input, mapped to SQL LIMIT/OFFSET.
type Page struct {
	Page    int
	PerPage int
	Limit   int
	Offset  int
}

// Pagination is the response metadata block (openapi #/components/schemas/Pagination).
type Pagination struct {
	Page    int `json:"page"`
	PerPage int `json:"per_page"`
	Total   int `json:"total"`
}

// ParsePage reads page (default 1, minimum 1) and per_page (default 50, minimum
// 1, capped at 100) from the request query, returning the LIMIT/OFFSET mapping.
// Non-numeric or out-of-range (< 1) values are rejected with an error.
func ParsePage(r *http.Request) (Page, error) {
	page := 1
	perPage := defaultPerPage
	q := r.URL.Query()
	if v := q.Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return Page{}, fmt.Errorf("invalid page %q", v)
		}
		page = n
	}
	if v := q.Get("per_page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return Page{}, fmt.Errorf("invalid per_page %q", v)
		}
		if n > maxPerPage {
			n = maxPerPage
		}
		perPage = n
	}
	return Page{Page: page, PerPage: perPage, Limit: perPage, Offset: (page - 1) * perPage}, nil
}
