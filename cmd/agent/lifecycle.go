// lifecycle.go 管理 CodeForge 单实例生命周期：运行信息文件、stop/restart 支持。
//
// 启动时将 {pid, port, token} 写入 <config>/codeforge.run；stop 命令读取该文件
// 通过内部关闭接口（/api/shutdown + 令牌请求头）优雅停止实例；文件缺失或
// 优雅通道不可用时，按端口定位进程强制结束兜底。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// runInfo 是运行实例的标识信息，写入 codeforge.run。
type runInfo struct {
	PID   int    `json:"pid"`
	Port  int    `json:"port"`
	Token string `json:"token"`
	URL   string `json:"url"`
}

// runFilePath 返回运行信息文件路径（位于配置目录下）。
func runFilePath(configDir string) string {
	return filepath.Join(configDir, "codeforge.run")
}

// readRunInfo 读取运行信息；文件缺失或损坏时返回 nil。
func readRunInfo(configDir string) *runInfo {
	data, err := os.ReadFile(runFilePath(configDir))
	if err != nil {
		return nil
	}
	var info runInfo
	if err := json.Unmarshal(data, &info); err != nil || info.Port <= 0 {
		return nil
	}
	return &info
}

// writeRunInfo 原子写入运行信息文件。
func writeRunInfo(configDir string, info *runInfo) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	tmp := runFilePath(configDir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, runFilePath(configDir))
}

// removeRunInfo 删除运行信息文件（不存在时忽略）。
func removeRunInfo(configDir string) {
	_ = os.Remove(runFilePath(configDir))
}

// baseURL 返回本机回环地址上的服务根 URL（管理通道不依赖配置的监听地址）。
func baseURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// healthOK 探测端口上是否运行着可响应的 CodeForge 服务。
func healthOK(port int) bool {
	if port <= 0 {
		return false
	}
	client := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get(baseURL(port) + "/api/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// requestShutdown 向实例发送带令牌的优雅关闭请求。
func requestShutdown(port int, token string) error {
	req, err := http.NewRequest(http.MethodPost, baseURL(port)+"/api/shutdown", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-CodeForge-Token", token)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("状态码 %d", resp.StatusCode)
	}
	return nil
}

// waitForDown 轮询等待服务下线。
func waitForDown(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !healthOK(port) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return !healthOK(port)
}

// stopInstance 停止运行中的实例：优雅关闭优先，必要时按 PID/端口强制结束。
// 返回是否实际停止了进程。
func stopInstance(configDir string, fallbackPort int) bool {
	info := readRunInfo(configDir)
	port := fallbackPort
	if info != nil && info.Port > 0 {
		port = info.Port
	}
	if !healthOK(port) {
		if info != nil {
			removeRunInfo(configDir) // 清理残留的过期运行信息
		}
		return false
	}

	// 1) 优雅关闭（需要 run 文件中的内部令牌）
	if info != nil && info.Token != "" {
		if err := requestShutdown(port, info.Token); err == nil && waitForDown(port, 8*time.Second) {
			removeRunInfo(configDir)
			return true
		}
		log.Println("优雅关闭未完成，尝试强制结束…")
	}

	// 2) 强制结束：优先 run 文件中的 PID，失效则按端口定位
	pid := 0
	if info != nil {
		pid = info.PID
	}
	if pid <= 0 || !forceKillPID(pid) {
		pid = findPIDByPort(port)
		forceKillPID(pid)
	}
	ok := waitForDown(port, 8*time.Second)
	if ok {
		removeRunInfo(configDir)
	}
	return ok
}

// forceKillPID 强制结束进程；进程不存在时返回 false。
func forceKillPID(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Kill(); err != nil {
		return false
	}
	_, _ = proc.Wait()
	return true
}

// findPIDByPort 定位监听指定端口的进程 PID（找不到返回 0）。
func findPIDByPort(port int) int {
	if runtime.GOOS == "windows" {
		return findPIDByPortWindows(port)
	}
	// POSIX（含 Linux / Termux）：优先 lsof，缺失时解析 ss。
	if pid := findPIDByPortLsof(port); pid > 0 {
		return pid
	}
	return findPIDByPortSS(port)
}

// findPIDByPortLsof 通过 lsof 定位监听进程。
func findPIDByPortLsof(port int) int {
	out, err := exec.Command("lsof", "-ti", fmt.Sprintf("tcp:%d", port)).Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if pid, _ := strconv.Atoi(strings.TrimSpace(line)); pid > 0 {
			return pid
		}
	}
	return 0
}

// findPIDByPortSS 解析 ss -ltnp 输出定位监听进程（lsof 缺失时的回退，
// 适用于 Termux / 精简发行版）。
func findPIDByPortSS(port int) int {
	out, err := exec.Command("ss", "-ltnp").Output()
	if err != nil {
		return 0
	}
	suffix := fmt.Sprintf(":%d", port)
	pidRe := regexp.MustCompile(`pid=(\d+)`)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		// 行样例：LISTEN 0 4096 127.0.0.1:8420 0.0.0.0:* users:(("codeforge",pid=1234,fd=6))
		if len(fields) < 4 || !strings.EqualFold(fields[0], "LISTEN") {
			continue
		}
		if !strings.HasSuffix(fields[3], suffix) {
			continue
		}
		if m := pidRe.FindStringSubmatch(strings.Join(fields[4:], " ")); m != nil {
			if pid, _ := strconv.Atoi(m[1]); pid > 0 {
				return pid
			}
		}
	}
	return 0
}

// findPIDByPortWindows 解析 netstat -ano 输出定位监听进程。
func findPIDByPortWindows(port int) int {
	out, err := exec.Command("netstat", "-ano", "-p", "tcp").Output()
	if err != nil {
		return 0
	}
	suffix := fmt.Sprintf(":%d", port)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.EqualFold(fields[3], "LISTENING") {
			continue
		}
		if strings.HasSuffix(fields[1], suffix) {
			if pid, _ := strconv.Atoi(fields[4]); pid > 0 {
				return pid
			}
		}
	}
	return 0
}
