package cf

import (
	"strings"
	"testing"
)

func TestLoginTailKeepsWhatTheChildSaid(t *testing.T) {
	tail := newLoginTail(1024)
	tail.write([]byte("INF Starting webserver\n"))
	tail.write([]byte("ERR listen tcp 127.0.0.1:8976: bind: address already in use\n"))

	got := tail.lastLines(3)
	if !strings.Contains(got, "address already in use") {
		t.Fatalf("tail lost the reason: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("tail must join lines into one: %q", got)
	}
}

func TestLoginTailTrimsToLastLines(t *testing.T) {
	tail := newLoginTail(1024)
	tail.write([]byte("a\nb\nc\nd\n"))
	if got := tail.lastLines(2); got != "c；d" {
		t.Fatalf("lastLines(2) = %q, want c；d", got)
	}
}

// A child that prints megabytes must not grow the buffer without bound.
func TestLoginTailDropsOldBytes(t *testing.T) {
	tail := newLoginTail(8)
	tail.write([]byte("0123456789abcdef"))
	if got := tail.lastLines(1); !strings.HasSuffix(got, "cdef") {
		t.Fatalf("lastLines(1) = %q, want the newest bytes", got)
	}
}

func TestLoginFailureMessage(t *testing.T) {
	const bin = "/var/apps/cloudflared/target/bin/cloudflared"

	msg := loginFailureMessage(1, "bind: address already in use", bin)
	for _, want := range []string{"退出码 1", "address already in use", "tunnel login", bin} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message is missing %q: %s", want, msg)
		}
	}

	// No detail: the exit code still has to be reported, and the manual command
	// still has to be offered.
	bare := loginFailureMessage(2, "", bin)
	if !strings.Contains(bare, "退出码 2") || !strings.Contains(bare, "tunnel login") {
		t.Fatalf("message = %s", bare)
	}

	// Exit 0 with no certificate is a cancellation, not a failure.
	if got := loginFailureMessage(0, "whatever", bin); got != "登录已取消，未获取到 cert.pem" {
		t.Fatalf("cancellation message = %q", got)
	}
}
