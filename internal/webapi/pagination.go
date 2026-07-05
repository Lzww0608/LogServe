package webapi

// This file contains the shared offset-token pagination contract used by list
// endpoints.

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Shared list limits keep dashboard-derived list endpoints small and predictable.
const (
	// defaultListLimit is used when callers omit limit.
	defaultListLimit = 50
	// maxListLimit bounds in-memory slices returned by list endpoints.
	maxListLimit = 100
)

// paginationParams stores the bounded list limit and integer offset token parsed
// from query parameters.
type paginationParams struct {
	Limit  int
	Offset int
}

// paginationResult describes the slice window and next offset token returned to
// list endpoints.
type paginationResult struct {
	Start         int
	End           int
	Limit         int
	TotalCount    int
	NextPageToken string
}

// parsePaginationParams validates limit and page_token against the shared list
// pagination contract.
func parsePaginationParams(r *http.Request, totalCount int) (paginationParams, error) {
	if totalCount < 0 {
		totalCount = 0
	}
	limit := defaultListLimit
	rawLimit := strings.TrimSpace(r.URL.Query().Get("limit"))
	if rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed <= 0 {
			return paginationParams{}, fmt.Errorf("%w: limit must be a positive integer", errInvalidInput)
		}
		if parsed > maxListLimit {
			return paginationParams{}, fmt.Errorf("%w: limit must be between 1 and %d", errInvalidInput, maxListLimit)
		}
		limit = parsed
	}
	offset := 0
	// Page tokens are plain integer offsets because every list endpoint first
	// materializes an in-memory dashboard-derived slice before pagination.
	rawToken := strings.TrimSpace(r.URL.Query().Get("page_token"))
	if rawToken != "" {
		parsed, err := strconv.Atoi(rawToken)
		if err != nil || parsed < 0 {
			return paginationParams{}, fmt.Errorf("%w: page_token must be a non-negative integer offset", errInvalidInput)
		}
		offset = parsed
	}
	return paginationParams{Limit: limit, Offset: offset}, nil
}

// paginate clamps the requested offset to the collection size and computes the
// next page token when more items remain.
func paginate(totalCount int, params paginationParams) paginationResult {
	start := params.Offset
	if start > totalCount {
		start = totalCount
	}
	end := start + params.Limit
	if end > totalCount {
		end = totalCount
	}
	nextPageToken := ""
	if end < totalCount {
		nextPageToken = strconv.Itoa(end)
	}
	return paginationResult{
		Start:         start,
		End:           end,
		Limit:         params.Limit,
		TotalCount:    totalCount,
		NextPageToken: nextPageToken,
	}
}
