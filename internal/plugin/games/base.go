package games

// Game 定义蓝妹可以互动的小游戏接口
type Game interface {
	// Name 返回游戏名称
	Name() string
	// Description 返回游戏简介
	Description() string
	// Start 为某玩家开始一局游戏
	Start(playerID int64) error
	// Handle 处理玩家在游戏中的输入
	Handle(playerID int64, input string) (reply string, finished bool, err error)
	// End 结束某玩家的游戏
	End(playerID int64) error
}

// BaseGame 提供通用的基础实现，可被嵌入
type BaseGame struct {
	name string
	desc string
}

func NewBaseGame(name, desc string) BaseGame {
	return BaseGame{name: name, desc: desc}
}

func (g *BaseGame) Name() string        { return g.name }
func (g *BaseGame) Description() string { return g.desc }
