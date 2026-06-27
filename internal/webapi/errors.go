package webapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type errorBody struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func writeAPIError(w http.ResponseWriter, httpStatus int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(errorBody{Error: apiError{Code: code, Message: message}})
}

func writeErr(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	code := "INTERNAL"
	httpStatus := http.StatusInternalServerError
	message := err.Error()
	if st, ok := status.FromError(err); ok {
		code = st.Code().String()
		message = st.Message()
		switch st.Code() {
		case codes.InvalidArgument:
			httpStatus = http.StatusBadRequest
		case codes.Unauthenticated:
			httpStatus = http.StatusUnauthorized
		case codes.NotFound:
			httpStatus = http.StatusNotFound
		case codes.AlreadyExists, codes.Aborted, codes.FailedPrecondition:
			httpStatus = http.StatusConflict
		case codes.DeadlineExceeded:
			httpStatus = http.StatusGatewayTimeout
		default:
			httpStatus = http.StatusInternalServerError
		}
	} else {
		lower := strings.ToLower(message)
		switch {
		case errors.Is(err, errInvalidInput), strings.Contains(lower, "required"), strings.Contains(lower, "invalid"):
			code = "INVALID_ARGUMENT"
			httpStatus = http.StatusBadRequest
		case strings.Contains(lower, "not found"):
			code = "NOT_FOUND"
			httpStatus = http.StatusNotFound
		case strings.Contains(lower, "idempotency"):
			code = "CONFLICT"
			httpStatus = http.StatusConflict
		}
	}
	writeAPIError(w, httpStatus, code, message)
}
