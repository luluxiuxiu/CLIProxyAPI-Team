package auth

import (
	"net/http"
	"testing"
)

func TestErrorStatusCode_DefaultAvailabilityMappings(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		err  *Error
		want int
	}{
		{
			name: "auth unavailable defaults to 503",
			err:  &Error{Code: "auth_unavailable", Message: "no auth available"},
			want: http.StatusServiceUnavailable,
		},
		{
			name: "auth not found defaults to 503",
			err:  &Error{Code: "auth_not_found", Message: "no auth available"},
			want: http.StatusServiceUnavailable,
		},
		{
			name: "provider not found defaults to 503",
			err:  &Error{Code: "provider_not_found", Message: "no provider supplied"},
			want: http.StatusServiceUnavailable,
		},
		{
			name: "explicit http status wins",
			err:  &Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusTooManyRequests},
			want: http.StatusTooManyRequests,
		},
		{
			name: "unknown code keeps zero",
			err:  &Error{Code: "custom_error", Message: "boom"},
			want: 0,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.err.StatusCode(); got != tc.want {
				t.Fatalf("StatusCode() = %d, want %d", got, tc.want)
			}
		})
	}
}
