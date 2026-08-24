package command

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Context 是命令处理函数的上下文。
type Context struct {
	Platform       string   // 平台标识（qq/wechat/telegram/...）
	PlatformUserID string   // 平台用户 ID（平台唯一标识，如 QQ 号）
	GroupID        string   // 群组 ID
	IsGroup        bool     // 是否群消息
	CommandName    string   // 命令名（不含 / 前缀）
	CommandArgs    []string // 命令参数
	Message        string   // 原始消息
	Reply          func(string)
	// ReplySegments 回传 OneBot 原生消息段；主要用于插件命令经自然语言意图
	// 重入行为树后，将子消息上下文中的 at 等段传回原始消息。
	ReplySegments func([]map[string]any)
	// SuppressRequesterAt 禁止 Bot 为本次命令文本回复自动添加 @请求者。
	SuppressRequesterAt func()

	// ── 消息上下文（插件命令重入引擎时用于还原完整上下文）──
	SelfID    string   // 机器人自身平台 ID
	AtTargets []string // 消息 at 目标（平台 ID 列表）
	Nickname  string   // 发送者昵称
	MessageID string   // 消息 ID
	ConnID    string   // 来源连接 ID

	// IsSuperUser 当前消息发送者是否为超管（bot 层注入，命令 handler 据此做权限校验）。
	IsSuperUser bool

	// CommandReentry 标记本次 handler 调用来自插件命令重入（防止插件子树
	// 未匹配时"命令分支 → 重入 → 命令分支"的无限递归）。
	CommandReentry bool
}

// Command 定义一个斜杠命令。
type Command struct {
	Name        string
	Description string
	Handler     func(ctx *Context) error

	// Order 帮助列表排序权重，小的在前；0（未设置）排在末尾，
	// 同权重按命令名排序。动态安装的插件命令无需设置，自动排在最后。
	Order int
}

// System 并发安全地管理所有已注册命令。
type System struct {
	mu       sync.RWMutex
	commands map[string]Command
}

// New 创建命令系统。
func New() *System {
	return &System{commands: make(map[string]Command)}
}

// Register 注册命令。重复名称不会覆盖现有命令。
func (s *System) Register(cmd Command) error {
	if strings.TrimSpace(cmd.Name) == "" {
		return fmt.Errorf("命令名不能为空")
	}
	if cmd.Handler == nil {
		return fmt.Errorf("命令 %q 缺少 Handler", cmd.Name)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.commands[cmd.Name]; exists {
		return fmt.Errorf("命令 %q 已注册", cmd.Name)
	}
	s.commands[cmd.Name] = cmd
	return nil
}

// Unregister 注销命令。命令不存在时保持幂等。
func (s *System) Unregister(name string) {
	s.mu.Lock()
	delete(s.commands, name)
	s.mu.Unlock()
}

// Process 解析输入并分发到对应命令。
func (s *System) Process(input string, ctx *Context) error {
	name := strings.TrimPrefix(input, "/")
	parts := strings.Fields(name)
	if len(parts) == 0 {
		return fmt.Errorf("empty command")
	}

	cmdName := parts[0]
	s.mu.RLock()
	cmd, ok := s.commands[cmdName]
	s.mu.RUnlock()
	if !ok {
		ctx.Reply(fmt.Sprintf("未知命令: /%s\n输入 /帮助 查看可用命令", cmdName))
		return fmt.Errorf("unknown command: %s", cmdName)
	}

	if len(parts) > 1 {
		ctx.Message = "/" + cmdName + " " + strings.Join(parts[1:], " ")
	} else {
		ctx.Message = "/" + cmdName
	}
	return cmd.Handler(ctx)
}

// Lookup 查找已注册的命令（只读）
func (s *System) Lookup(name string) (Command, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cmd, ok := s.commands[name]
	return cmd, ok
}

// List 返回按名称排序的命令快照。
func (s *System) List() []Command {
	s.mu.RLock()
	commands := make([]Command, 0, len(s.commands))
	for _, cmd := range s.commands {
		commands = append(commands, cmd)
	}
	s.mu.RUnlock()

	sort.Slice(commands, func(i, j int) bool {
		return commands[i].Name < commands[j].Name
	})
	return commands
}

// HelpHandler 内置帮助命令：动态遍历全部注册命令，
// 按 Order 权重排序（0 排末尾，动态安装的插件命令自动在最后）。
func (s *System) HelpHandler(ctx *Context) error {
	commands := s.List()

	sort.SliceStable(commands, func(i, j int) bool {
		oi, oj := commands[i].Order, commands[j].Order
		if oi == 0 {
			oi = 1 << 30 // 未设置权重 → 排末尾
		}
		if oj == 0 {
			oj = 1 << 30
		}
		if oi != oj {
			return oi < oj
		}
		return commands[i].Name < commands[j].Name
	})

	var builder strings.Builder
	builder.WriteString("可用命令:\n")
	for _, cmd := range commands {
		builder.WriteString(fmt.Sprintf("  /%s — %s\n", cmd.Name, cmd.Description))
	}
	ctx.Reply(builder.String())
	return nil
}
