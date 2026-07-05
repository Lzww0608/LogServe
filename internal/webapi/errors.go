package webapi

// This file centralizes the JSON error envelope and maps gRPC/local errors
// onto HTTP status codes used by the console.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errorBody is the stable JSON envelope for API errors.
type errorBody struct {
	Error apiError `json:"error"`
}

// apiError carries machine-readable code, user-facing message, and optional
// structured details.
type apiError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// writeAPIError writes one error response using the webapi error envelope.
func writeAPIError(w http.ResponseWriter, httpStatus int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(errorBody{Error: apiError{Code: code, Message: message}})
}

// writeErr translates gRPC status errors and local validation failures into HTTP
// status codes while preserving the original message.
func writeErr(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	code := "INTERNAL"
	httpStatus := http.StatusInternalServerError
	message := err.Error()
	if st, ok := status.FromError(err); ok {
		// Preserve gRPC status text as the API code so frontend error handling can
		// distinguish backend failures from local validation failures.
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
		// Local validation helpers wrap errInvalidInput, but some older paths still
		// produce plain errors; the string checks keep those responses user-friendly.
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
