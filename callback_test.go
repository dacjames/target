package main

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestCallbackHTTPStatus(t *testing.T) {
	cases := []struct {
		name string
		res  callbackResult
		want int
	}{
		{"ok", callbackResult{OK: true}, 200},
		{"failure", callbackResult{OK: false}, 502},
		{"timeout", callbackResult{OK: false, timedOut: true}, 504},
		// A completed HTTP callback whose upstream answered 5xx is still ok.
		{"upstream 5xx still ok", callbackResult{OK: true, Status: 503}, 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.res.httpStatus(); got != c.want {
				t.Fatalf("httpStatus() = %d, want %d", got, c.want)
			}
		})
	}
}

type fakeTimeout struct{}

func (fakeTimeout) Error() string   { return "i/o timeout" }
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return false }

func TestIsTimeout(t *testing.T) {
	if !isTimeout(context.DeadlineExceeded) {
		t.Error("context.DeadlineExceeded should be a timeout")
	}
	if !isTimeout(net.Error(fakeTimeout{})) {
		t.Error("net.Error with Timeout()=true should be a timeout")
	}
	if isTimeout(errors.New("connection refused")) {
		t.Error("plain error should not be a timeout")
	}
	if isTimeout(nil) {
		t.Error("nil should not be a timeout")
	}
}
