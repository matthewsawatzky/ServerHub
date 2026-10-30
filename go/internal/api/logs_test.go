package api

import (
	"net/http/httptest"
	"testing"
)

func TestLogTail(t *testing.T) {
	cases := map[string]int{
		"":             defaultContainerLogTail,
		"?tail=50":     50,
		"?tail=0":      1,
		"?tail=-5":     1,
		"?tail=999999": 10000,
		"?tail=abc":    defaultContainerLogTail,
	}
	for query, want := range cases {
		r := httptest.NewRequest("GET", "/logs"+query, nil)
		if got := logTail(r); got != want {
			t.Errorf("logTail(%q) = %d, want %d", query, got, want)
		}
	}
}
