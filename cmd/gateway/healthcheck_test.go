package main

import "testing"

// A listen address is not always a destination: ":8080" means every interface,
// which cannot be dialled. Getting this wrong makes the container HEALTHCHECK
// fail against a perfectly healthy gateway.
func TestHealthcheckHostPort(t *testing.T) {
	tests := []struct {
		addr string
		want string
	}{
		{":8080", "127.0.0.1:8080"},
		{"0.0.0.0:8080", "127.0.0.1:8080"},
		{"[::]:8080", "127.0.0.1:8080"},
		{"127.0.0.1:9000", "127.0.0.1:9000"},
		{"192.168.1.10:8080", "192.168.1.10:8080"},
		{"localhost:8080", "localhost:8080"},
	}
	for _, tc := range tests {
		if got := healthcheckHostPort(tc.addr); got != tc.want {
			t.Errorf("healthcheckHostPort(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}
