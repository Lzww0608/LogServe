package webapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const (
	defaultListLimit = 50
	maxListLimit     = 100
)

type paginationParams struct {
	Limit  int
	Offset int
}

type paginationResult struct {
	Start         int
	End           int
	Limit         int
	TotalCount    int
	NextPageToken string
}

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
