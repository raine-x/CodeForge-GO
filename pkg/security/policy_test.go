package security

import (
	"testing"

	"codeforge/config"
)

func TestPolicyDenyDangerousCommand(t *testing.T) {
	p := NewPolicy(config.SecurityConfig{DefaultDecision: "ask"})

	cases := []string{
		"rm -rf /",
		"rm -rf /*",
		"mkfs.ext4 /dev/sda1",
		"shutdown -h now",
		"dd if=/dev/zero of=/dev/sda",
	}
	for _, cmd := range cases {
		d, reason := p.Evaluate("run_command", cmd, `{"command":"`+cmd+`"}`)
		if d != Deny {
			t.Errorf("命令 %q 期望 Deny，实际 %s（%s）", cmd, d, reason)
		}
	}
}

func TestPolicyNormalCommandNotDenied(t *testing.T) {
	p := NewPolicy(config.SecurityConfig{DefaultDecision: "ask"})
	d, _ := p.Evaluate("run_command", "go build ./...", `{"command":"go build ./..."}`)
	if d != Ask {
		t.Errorf("普通命令期望 Ask，实际 %s", d)
	}
}

func TestPolicyReadOnlyAutoAllow(t *testing.T) {
	p := NewPolicy(config.SecurityConfig{DefaultDecision: "ask", AutoApproveReadOnly: true})
	d, _ := p.Evaluate("read_file", "/tmp/a.txt", `{"path":"/tmp/a.txt"}`)
	if d != Allow {
		t.Errorf("只读操作期望 Allow，实际 %s", d)
	}
}

func TestPolicyExplicitRule(t *testing.T) {
	p := NewPolicy(config.SecurityConfig{
		DefaultDecision: "allow",
		Rules: []config.SecurityRule{
			{Tools: []string{"write_file"}, Decision: "ask"},
		},
	})
	d, _ := p.Evaluate("write_file", "/tmp/a.txt", `{"path":"/tmp/a.txt"}`)
	if d != Ask {
		t.Errorf("命中规则期望 Ask，实际 %s", d)
	}
}

func TestPolicyPluginRequiresApproval(t *testing.T) {
	p := NewPolicy(config.SecurityConfig{DefaultDecision: "allow"})
	p.RequireApproval("git_assistant.git_status")

	d, _ := p.Evaluate("git_assistant.git_status", "git_status", `{}`)
	if d != Ask {
		t.Errorf("插件强制审批期望 Ask，实际 %s", d)
	}
}

func TestPolicyModeAutoOverridesAskRules(t *testing.T) {
	// 复现线上配置：default.yaml 把写类工具硬编码为 ask 规则。
	p := NewPolicy(config.SecurityConfig{
		DefaultDecision: "ask",
		Rules: []config.SecurityRule{
			{Tools: []string{"write_file", "edit_file", "delete_file", "run_command"}, Decision: "ask"},
		},
	})
	if d, _ := p.Evaluate("edit_file", "a.html", `{"path":"a.html"}`); d != Ask {
		t.Fatalf("ask 模式下写操作期望 Ask，实际 %s", d)
	}

	p.SetMode(ModeAuto)
	if d, _ := p.Evaluate("edit_file", "a.html", `{"path":"a.html"}`); d != Allow {
		t.Errorf("自主模式下 Ask 规则应升级为 Allow，实际 %s", d)
	}
	// 黑名单在自主模式下依然生效
	if d, _ := p.Evaluate("run_command", "shutdown -h now", `{"command":"shutdown -h now"}`); d != Deny {
		t.Errorf("自主模式下黑名单期望 Deny，实际 %s", d)
	}
}

func TestPolicyModeReadOnlyLocksDown(t *testing.T) {
	p := NewPolicy(config.SecurityConfig{
		DefaultDecision: "ask",
		Rules: []config.SecurityRule{
			{Tools: []string{"write_file"}, Decision: "allow"},
		},
	})
	p.SetMode(ModeReadOnly)
	if d, _ := p.Evaluate("write_file", "a.txt", `{"path":"a.txt"}`); d != Deny {
		t.Errorf("只读模式下 Allow 规则也应被锁定为 Deny，实际 %s", d)
	}
	if d, _ := p.Evaluate("read_file", "a.txt", `{"path":"a.txt"}`); d != Allow {
		t.Errorf("只读模式下读操作期望 Allow，实际 %s", d)
	}
}

func TestPolicySetModeAskRestoresConfigDefault(t *testing.T) {
	p := NewPolicy(config.SecurityConfig{DefaultDecision: "ask"})
	p.SetMode(ModeAuto)
	p.SetMode(ModeAsk)
	if d, _ := p.Evaluate("run_command", "go build", `{"command":"go build"}`); d != Ask {
		t.Errorf("切回 ask 模式后应恢复配置默认 Ask，实际 %s", d)
	}
}
