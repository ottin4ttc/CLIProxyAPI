package apikeylimit

import "testing"

func TestDefaultReturnsSameSharedInstance(t *testing.T) {
	first := Default()
	second := Default()
	if first == nil {
		t.Fatal("Default() = nil, want a usable Limiter")
	}
	if first != second {
		t.Fatal("Default() returned different instances; callers relying on a shared budget would each get their own counter")
	}
}

func TestDefaultBehavesLikeAnOrdinaryLimiter(t *testing.T) {
	l := Default()
	key := "sk-apikeylimit-default-test"
	if ok, _ := l.Allow(key, 1, base); !ok {
		t.Fatal("first request rejected")
	}
	if ok, _ := l.Allow(key, 1, base); ok {
		t.Fatal("second request within the window should be rejected")
	}
}
