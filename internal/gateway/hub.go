package gateway

import (
	"fmt"
	"sync"

	"github.com/lxzan/gws"
)

// Connection 表示一个反向 WebSocket 连接（来自 Onebots 或 NapCat）
type Connection struct {
	ID       string    // 连接唯一标识
	Platform Platform  // 来源平台
	Protocol Protocol  // OneBot 协议版本
	SelfID   string    // 机器人自身 ID（并发写入，通过 mu 保护）
	Impl     string    // OneBot 实现名（并发写入，通过 mu 保护）
	Socket   *gws.Conn // 底层 gws 连接

	mu sync.Mutex // 保护 SelfID、Impl 的并发写入
}

// SetSelfID 安全设置 SelfID（仅首次设置生效）
func (c *Connection) SetSelfID(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.SelfID == "" {
		c.SelfID = id
	}
}

// SetImpl 安全设置 Impl（仅首次设置生效）
func (c *Connection) SetImpl(impl string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Impl == "" {
		c.Impl = impl
	}
}

// Hub 管理所有活跃的反向 WS 连接
type Hub struct {
	mu    sync.RWMutex
	conns map[string]*Connection // 连接ID → Connection
}

// NewHub 创建连接管理中心
func NewHub() *Hub {
	return &Hub{conns: make(map[string]*Connection)}
}

// Register 注册新连接
func (h *Hub) Register(conn *Connection) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[conn.ID] = conn
}

// Unregister 移除连接
func (h *Hub) Unregister(connID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, connID)
}

// Get 获取指定连接
func (h *Hub) Get(connID string) (*Connection, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.conns[connID]
	return c, ok
}

// SendTo 向指定连接发送动作请求
func (h *Hub) SendTo(connID string, req *ActionRequest) error {
	h.mu.RLock()
	conn, ok := h.conns[connID]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("gateway: 连接 %s 不存在", connID)
	}
	return writeJSON(conn.Socket, req)
}

// SendMessageTo 向指定连接发送文本消息（自动选择协议动作）
func (h *Hub) SendMessageTo(connID string, msg *NormalizedMessage, text string) error {
	return h.SendSegments(connID, msg, []NormalizedSegment{{Type: "text", Text: text}})
}

// SendSegments 向指定连接发送结构化消息段（text/image/at 组合，自动选择协议动作）。
func (h *Hub) SendSegments(connID string, msg *NormalizedMessage, segs []NormalizedSegment) error {
	h.mu.RLock()
	conn, ok := h.conns[connID]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("gateway: 连接 %s 不存在", connID)
	}

	var req *ActionRequest
	if conn.Protocol == ProtocolV11 {
		req = BuildSendMessageSegmentsV11(msg.IsGroup, msg.UserID, msg.GroupID, segs)
	} else {
		detailType := "private"
		if msg.IsGroup {
			detailType = "group"
		}
		req = BuildSendMessageSegmentsV12(detailType, msg.UserID, msg.GroupID, segs)
	}
	return writeJSON(conn.Socket, req)
}

// SendImage 向指定连接发送图片（file 支持 url / base64(data:image/...) / RustFS presign URL / 本地路径）。
func (h *Hub) SendImage(connID string, msg *NormalizedMessage, file string) error {
	return h.SendSegments(connID, msg, []NormalizedSegment{{
		Type: "image",
		Data: map[string]string{"file": file},
	}})
}

// SendAtText 向指定连接发送 "@用户 + 文本" 组合消息。
// atUserID 为空时退化为纯文本发送。
func (h *Hub) SendAtText(connID string, msg *NormalizedMessage, atUserID, text string) error {
	segs := make([]NormalizedSegment, 0, 2)
	if atUserID != "" {
		segs = append(segs, NormalizedSegment{Type: "at", Data: map[string]string{"user_id": atUserID}})
	}
	segs = append(segs, NormalizedSegment{Type: "text", Text: text})
	return h.SendSegments(connID, msg, segs)
}

// SendRichContent 向指定连接发送富文本消息（文本 + 可选 @ + 可选图片，自动选择协议动作）。
// content.At 为空时不 @，content.Image 为空时不发图。
func (h *Hub) SendRichContent(connID string, msg *NormalizedMessage, content RichContent) error {
	segs := make([]NormalizedSegment, 0, 3)
	if content.At != "" {
		segs = append(segs, NormalizedSegment{Type: "at", Data: map[string]string{"user_id": content.At}})
	}
	segs = append(segs, NormalizedSegment{Type: "text", Text: content.Text})
	if content.Image != "" {
		segs = append(segs, NormalizedSegment{Type: "image", Data: map[string]string{"file": content.Image}})
	}
	return h.SendSegments(connID, msg, segs)
}

// All 返回所有活跃连接的快照
func (h *Hub) All() []*Connection {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make([]*Connection, 0, len(h.conns))
	for _, c := range h.conns {
		result = append(result, c)
	}
	return result
}

// Count 返回活跃连接数
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}
