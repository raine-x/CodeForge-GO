package server

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
)

// 端口只由全局配置文件决定：被占用时必须直接失败，绝不自动换端口。

func TestStartFailsWhenPortOccupied(t *testing.T) {
	d := newTestDeps(t)

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占用端口失败: %v", err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port

	d.cfg.Server.Host = "127.0.0.1"
	d.cfg.Server.Port = busyPort

	srv := d.newServer()
	err = srv.Start()
	if err == nil {
		_ = srv.Shutdown(context.Background())
		t.Fatal("端口被占用时应直接返回错误，而不是自动换端口")
	}

	msg := err.Error()
	if !strings.Contains(msg, "已被占用") {
		t.Errorf("错误信息应说明端口被占用，实际: %v", err)
	}
	if !strings.Contains(msg, "default.yaml") {
		t.Errorf("错误信息应指引修改全局配置文件，实际: %v", err)
	}
	if !strings.Contains(msg, strconv.Itoa(busyPort)) {
		t.Errorf("错误信息应包含冲突端口 %d，实际: %v", busyPort, err)
	}
	t.Logf("预期报错: %v", err)
}

func TestPortComesFromConfigOnly(t *testing.T) {
	d := newTestDeps(t)

	// 取一个空闲端口并释放，写入配置
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("探测空闲端口失败: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	d.cfg.Server.Host = "127.0.0.1"
	d.cfg.Server.Port = port

	srv := d.newServer()
	if err := srv.Start(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	if srv.Port() != port {
		t.Errorf("端口应严格等于配置值 %d，实际 %d", port, srv.Port())
	}
	if !strings.Contains(srv.Addr(), strconv.Itoa(port)) {
		t.Errorf("Addr 应包含配置端口，实际: %s", srv.Addr())
	}
	if !strings.Contains(srv.URL(), strconv.Itoa(port)) {
		t.Errorf("URL 应包含配置端口，实际: %s", srv.URL())
	}
}
