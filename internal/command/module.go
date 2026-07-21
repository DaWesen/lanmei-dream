package command

import (
	"fmt"
	"strings"
)

// Context 是命令处理函数的上下文
type Context struct {
	UserID  int64
	Message string
	Reply   func(string)
}

// Command 定义一个斜杠命令
type Command struct {
	Name        string
	Description string
	Handler     func(ctx *Context) error
}

// System 管理所有已注册的命令
type System struct {
	commands map[string]Command
}

// New 创建命令系统
func New() *System {
	return &System{commands: make(map[string]Command)}
}

// Register 注册一条命令
func (s *System) Register(cmd Command) {
	s.commands[cmd.Name] = cmd
}

// Process 解析输入并分发到对应命令
func (s *System) Process(input string, ctx *Context) error {
	name := strings.TrimPrefix(input, "/")
	parts := strings.Fields(name)
	if len(parts) == 0 {
		return fmt.Errorf("empty command")
	}

	cmdName := parts[0]
	cmd, ok := s.commands[cmdName]
	if !ok {
		ctx.Reply(fmt.Sprintf("未知命令: /%s\n输入 /帮助 查看可用命令", cmdName))
		return fmt.Errorf("unknown command: %s", cmdName)
	}

	// 把参数拼回传给 Handler
	if len(parts) > 1 {
		ctx.Message = "/" + cmdName + " " + strings.Join(parts[1:], " ")
	} else {
		ctx.Message = "/" + cmdName
	}

	return cmd.Handler(ctx)
}

// HelpHandler 是内置的帮助命令
func (s *System) HelpHandler(ctx *Context) error {
	var sb strings.Builder
	sb.WriteString("📋 可用命令:\n")
	for _, cmd := range s.commands {
		sb.WriteString(fmt.Sprintf("  /%s — %s\n", cmd.Name, cmd.Description))
	}
	ctx.Reply(sb.String())
	return nil
}
