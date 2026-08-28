package singleinst

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func TestNormalizeAddr(t *testing.T) {
	cases := []struct {
		in      string
		probe   string
		listen  string
		wantErr bool
	}{
		{":8080", "127.0.0.1:8080", ":8080", false},
		{"0.0.0.0:8080", "127.0.0.1:8080", "0.0.0.0:8080", false},
		{"127.0.0.1:8080", "127.0.0.1:8080", "127.0.0.1:8080", false},
		{"8080", "127.0.0.1:8080", "8080", false},
		{"[::]:8080", "127.0.0.1:8080", "[::]:8080", false},
		{"not-a-addr", "", "", true},
	}
	for _, c := range cases {
		probe, listen, err := normalizeAddr(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("normalizeAddr(%q): expected error, got nil", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeAddr(%q): unexpected error %v", c.in, err)
			continue
		}
		if probe != c.probe || listen != c.listen {
			t.Errorf("normalizeAddr(%q) = (%q,%q), want (%q,%q)", c.in, probe, listen, c.probe, c.listen)
		}
	}
}

func TestAcquireNoPeer(t *testing.T) {
	// 连一个未监听的端口，应判定无运行实例、返回 nil（正常接管）。
	logger := zap.NewNop().Sugar()
	// 取一个几乎不可能被监听的高端端口
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	yielded := false
	err = Acquire(context.Background(), addr, logger, func() { yielded = true })
	if err != nil {
		t.Fatalf("Acquire with no peer: expected nil, got %v", err)
	}
	if yielded {
		t.Fatal("Acquire with no peer should not yield")
	}
}

func TestAcquirePeerYieldsWhenNewer(t *testing.T) {
	// 起一个 health server，报告比本实例更新的 buildTimeUnix，Acquire 应让位。
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"buildTimeUnix":9999999999,"pid":12345}}`))
	}))
	defer peer.Close()

	logger := zap.NewNop().Sugar()
	yielded := false
	// peer 的 buildTimeUnix(2286 年) 远大于本实例，应触发让位。
	// 传入 peer 的真实地址（127.0.0.1:port），normalizeAddr 原样保留。
	err := Acquire(context.Background(), peer.URL[len("http://"):], logger, func() { yielded = true })
	if err != ErrYield {
		t.Fatalf("Acquire with newer peer: expected ErrYield, got %v", err)
	}
	if !yielded {
		t.Fatal("Acquire with newer peer should call selfExit")
	}
}
