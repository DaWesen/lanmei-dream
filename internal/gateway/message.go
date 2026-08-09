package gateway

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// ── OneBot 12 消息段 ──

// MessageSegmentV12 表示 OneBot 12 消息段
type MessageSegmentV12 struct {
	Type string         `json:"type"` // text / image / at / reply / ...
	Data map[string]any `json:"data"`
}

// TextContent 提取消息段的纯文本内容
func (m MessageSegmentV12) TextContent() string {
	switch m.Type {
	case "text":
		if s, ok := m.Data["text"].(string); ok {
			return s
		}
	case "at":
		if uid, ok := m.Data["user_id"].(string); ok {
			return "@" + uid
		}
	}
	return ""
}

// ExtractPlainText 从 OneBot 12 消息段列表提取纯文本
func ExtractPlainTextV12(segments []MessageSegmentV12) string {
	var parts []string
	for _, seg := range segments {
		if t := seg.TextContent(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "")
}

// ── OneBot 11 消息段 ──

// MessageSegmentV11 表示 OneBot 11 消息段（NapCat 方言）
type MessageSegmentV11 struct {
	Type string         `json:"type"` // text / image / at / face / reply / ...
	Data map[string]any `json:"data"`
}

// TextContent 提取消息段的纯文本内容
func (m MessageSegmentV11) TextContent() string {
	switch m.Type {
	case "text":
		if s, ok := m.Data["text"].(string); ok {
			return s
		}
	case "at":
		// NapCat 的 at 消息段，qq 字段可能是 number 或 string
		if qq := m.Data["qq"]; qq != nil {
			return "@" + formatIntOrString(qq)
		}
	}
	return ""
}

// ExtractPlainTextV11 从 OneBot 11 消息段列表提取纯文本
func ExtractPlainTextV11(segments []MessageSegmentV11) string {
	var parts []string
	for _, seg := range segments {
		if t := seg.TextContent(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "")
}

// ── 事件类型与通知子类型 ──

// MessageType 事件类型
type MessageType string

const (
	MessageTypeMessage MessageType = "message" // 普通消息
	MessageTypeNotice  MessageType = "notice"  // 系统通知
	MessageTypeRequest MessageType = "request" // 请求（好友/加群）
)

// NormalizedSegment 跨协议标准化消息段。
// 保留 type / mime type / data，供需要结构化信息的消费方（MediaPass、Topic 提及检测等）使用。
type NormalizedSegment struct {
	Type     string            // text / image / at / face / reply / file / audio / video / record / ...
	MimeType string            // 多媒体 MIME 类型（image/png、audio/ogg...），text/at 段为空
	Data     map[string]string // 平台原始 data（file_id / file / url / path / text / qq / name / ...）
	Text     string            // 该段的纯文本表示（text 内容 / @昵称 / 空）
}

// ── 标准化消息 ──

// NormalizedMessage 是跨平台标准化后的消息，供 bot 层消费
type NormalizedMessage struct {
	Platform   Platform // 来源平台
	Protocol   Protocol // 协议版本
	SelfID     string   // 机器人自身 ID
	UserID     string   // 平台用户 ID（字符串，跨平台）；notice 事件为被操作者
	GroupID    string   // 平台群 ID（空字符串 = 私聊）
	IsGroup    bool     // 是否群消息
	Content    string   // 纯文本内容（所有 text 段拼接，at 段转 "@昵称"）
	SenderName string   // 发送者昵称
	MessageID  string   // 消息 ID
	ConnID     string   // 来源连接 ID（用于回复路由）

	// ── 多模态 / 事件扩展字段 ──
	// 多模态段（message 事件填充）；事件字段（notice/request 事件填充，普通消息为空）
	Segments     []NormalizedSegment // 完整段列表
	AtTargets    []string            // at 目标 user_id 列表
	MimeTypes    []string            // 去重后的 MIME 类型列表
	MessageType  MessageType         // message / notice / request
	EventType    string              // 规范化事件类型（见 notice.go；普通消息为空）
	EventSubType string              // 事件子类型（透传原始 sub_type，可为空）
	EventData    map[string]any      // 事件全字段（普通消息为 nil）
}

// NormalizeV12 将 OneBot 12 事件标准化为 NormalizedMessage。
// 支持 message / notice / request 三类事件；meta 事件返回 nil。
func NormalizeV12(connID string, evt *EventV12, platform Platform) *NormalizedMessage {
	if evt.Type == "" || evt.Type == "meta" {
		return nil
	}

	// 如果事件平台字段有效，优先使用事件中的平台标识
	p := platform
	if evt.Platform != "" {
		p = Platform(evt.Platform)
	}

	msg := &NormalizedMessage{
		Platform:  p,
		Protocol:  ProtocolV12,
		SelfID:    evt.ResolveSelfID(),
		UserID:    evt.UserID,
		GroupID:   evt.GroupID,
		IsGroup:   evt.GroupID != "",
		MessageID: evt.ID,
		ConnID:    connID,
	}

	switch evt.Type {
	case "message":
		msg.MessageType = MessageTypeMessage
		msg.Segments = ParseSegmentsV12(evt.Message)
		msg.Content = evt.AltMessage
		if msg.Content == "" {
			msg.Content = ExtractPlainTextV12(evt.Message)
		}
		collectSegmentMeta(msg, msg.Segments)
		msg.SenderName = "" // OB12 消息事件不直接包含 sender nickname，需从 sender 子对象获取（如有）

	case "notice":
		// 通知事件：仅接收白名单内的事件类型（见 notice.go），白名单外返回 nil
		eventType, subType, data, ok := normalizeNoticeV12(evt)
		if !ok {
			return nil
		}
		msg.MessageType = MessageTypeNotice
		msg.EventType = eventType
		msg.EventSubType = subType
		msg.EventData = data

	case "request":
		msg.MessageType = MessageTypeRequest
		msg.EventType = evt.DetailType
		msg.EventSubType = evt.SubType
		msg.EventData = map[string]any{
			"user_id":     evt.UserID,
			"group_id":    evt.GroupID,
			"sub_type":    evt.SubType,
			"detail_type": evt.DetailType,
		}

	default:
		return nil
	}

	return msg
}

// NormalizeV11 将 OneBot 11 事件标准化为 NormalizedMessage。
// 支持 message / notice / request 三类事件；meta_event 返回 nil。
func NormalizeV11(connID string, evt *EventV11, platform Platform) *NormalizedMessage {
	if evt.PostType == "" || evt.PostType == "meta_event" {
		return nil
	}

	msg := &NormalizedMessage{
		Platform: platform,
		Protocol: ProtocolV11,
		SelfID:   strconv.FormatInt(evt.SelfID, 10),
		ConnID:   connID,
	}

	switch evt.PostType {
	case "message":
		msg.MessageType = MessageTypeMessage
		msg.UserID = strconv.FormatInt(evt.UserID, 10)
		if evt.MessageType == "group" {
			msg.GroupID = strconv.FormatInt(evt.GroupID, 10)
			msg.IsGroup = true
		}
		// 群聊兜底判定：message_type 缺失时 group_id 非 0 也判为群聊
		//（防御 message_type 缺失的异常报文，notice 事件不经过此分支）
		if !msg.IsGroup && evt.GroupID != 0 {
			msg.GroupID = strconv.FormatInt(evt.GroupID, 10)
			msg.IsGroup = true
		}
		msg.Segments = ParseSegmentsV11(evt.ParseMessageSegments())
		msg.Content = evt.RawMessage
		if msg.Content == "" {
			msg.Content = ExtractPlainTextV11(evt.ParseMessageSegments())
		}
		collectSegmentMeta(msg, msg.Segments)

		senderName := evt.Sender.Nickname
		if msg.IsGroup && evt.Sender.Card != "" {
			senderName = evt.Sender.Card
		}
		msg.SenderName = senderName
		msg.MessageID = strconv.FormatInt(evt.MessageID, 10)

	case "notice":
		// 通知事件：仅接收白名单内的事件类型（见 notice.go），白名单外返回 nil
		eventType, subType, data, ok := normalizeNoticeV11(evt)
		if !ok {
			return nil
		}
		msg.MessageType = MessageTypeNotice
		msg.UserID = strconv.FormatInt(evt.UserID, 10)
		msg.GroupID = strconv.FormatInt(evt.GroupID, 10)
		msg.IsGroup = evt.GroupID != 0
		msg.EventType = eventType
		msg.EventSubType = subType
		msg.EventData = data

	case "request":
		msg.MessageType = MessageTypeRequest
		msg.UserID = strconv.FormatInt(evt.UserID, 10)
		msg.GroupID = strconv.FormatInt(evt.GroupID, 10)
		msg.IsGroup = evt.GroupID != 0
		msg.EventType = evt.RequestType
		msg.EventSubType = evt.SubType
		msg.EventData = map[string]any{
			"user_id":      strconv.FormatInt(evt.UserID, 10),
			"group_id":     strconv.FormatInt(evt.GroupID, 10),
			"sub_type":     evt.SubType,
			"request_type": evt.RequestType,
		}

	default:
		return nil
	}

	return msg
}

// ── 消息段解析与辅助 ──

// ParseSegmentsV12 将 OneBot 12 消息段列表解析为标准化段。
// 多媒体段的 MIME 类型按文件扩展名推断；at 段的 Text 为 "@昵称"（无昵称时用 user_id）。
func ParseSegmentsV12(segs []MessageSegmentV12) []NormalizedSegment {
	result := make([]NormalizedSegment, 0, len(segs))
	for _, seg := range segs {
		data := toStringMap(seg.Data)
		ns := NormalizedSegment{Type: seg.Type, Data: data}
		switch seg.Type {
		case "text":
			ns.Text = data["text"]
		case "image", "audio", "video", "file", "record", "voice":
			ns.MimeType = DetectMimeByExt(seg.Type, data["file"], data["url"])
		case "at":
			ns.Text = "@" + atDisplayName(data["user_id"], data["name"])
		case "face":
			ns.Text = "[表情]"
		}
		result = append(result, ns)
	}
	return result
}

// ParseSegmentsV11 将 OneBot 11 消息段列表解析为标准化段。
// at 段的 user_id 同时兼容 JSON number 与 string 两种编码。
func ParseSegmentsV11(segs []MessageSegmentV11) []NormalizedSegment {
	result := make([]NormalizedSegment, 0, len(segs))
	for _, seg := range segs {
		data := toStringMap(seg.Data)
		ns := NormalizedSegment{Type: seg.Type, Data: data}
		switch seg.Type {
		case "text":
			ns.Text = data["text"]
		case "image", "audio", "video", "file", "record", "voice":
			ns.MimeType = DetectMimeByExt(seg.Type, data["file"], data["url"])
		case "at":
			uid := data["qq"]
			if uid == "" {
				uid = data["user_id"]
			}
			data["user_id"] = uid
			ns.Text = "@" + atDisplayName(uid, data["name"])
		case "face":
			ns.Text = "[表情]"
		}
		result = append(result, ns)
	}
	return result
}

// collectSegmentMeta 收集 at 目标列表与去重 MIME 类型列表写入 msg。
func collectSegmentMeta(msg *NormalizedMessage, segs []NormalizedSegment) {
	seen := make(map[string]struct{})
	for _, s := range segs {
		if s.Type == "at" {
			if uid := s.Data["user_id"]; uid != "" {
				if _, ok := seen["at:"+uid]; !ok {
					seen["at:"+uid] = struct{}{}
					msg.AtTargets = append(msg.AtTargets, uid)
				}
			}
		}
		if s.MimeType != "" {
			if _, ok := seen["mime:"+s.MimeType]; !ok {
				seen["mime:"+s.MimeType] = struct{}{}
				msg.MimeTypes = append(msg.MimeTypes, s.MimeType)
			}
		}
	}
}

// toStringMap 将任意值 map 转为字符串 map，兼容 JSON number/string/bool。
func toStringMap(data map[string]any) map[string]string {
	if len(data) == 0 {
		return map[string]string{}
	}
	result := make(map[string]string, len(data))
	for k, v := range data {
		result[k] = formatAny(v)
	}
	return result
}

// formatAny 将 JSON 值格式化为字符串（处理 number/string/bool）。
func formatAny(v any) string {
	switch n := v.(type) {
	case nil:
		return ""
	case string:
		return n
	case bool:
		return strconv.FormatBool(n)
	case json.Number:
		return n.String()
	case float64:
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(n, 10)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// atDisplayName 返回 at 段的显示名（昵称优先，其次 user_id）。
func atDisplayName(userID, name string) string {
	if name != "" {
		return name
	}
	return userID
}

// DetectMimeByExt 根据段类型与 file/url 扩展名推断 MIME 类型。
// 无法推断时返回 ""。
func DetectMimeByExt(segType, file, url string) string {
	ext := strings.ToLower(path.Ext(file))
	if ext == "" {
		ext = strings.ToLower(path.Ext(url))
	}
	switch segType {
	case "image":
		switch ext {
		case ".png":
			return "image/png"
		case ".jpg", ".jpeg":
			return "image/jpeg"
		case ".gif":
			return "image/gif"
		case ".webp":
			return "image/webp"
		case ".bmp":
			return "image/bmp"
		}
	case "audio", "record", "voice":
		switch ext {
		case ".mp3":
			return "audio/mpeg"
		case ".ogg":
			return "audio/ogg"
		case ".wav":
			return "audio/wav"
		case ".flac":
			return "audio/flac"
		case ".m4a":
			return "audio/mp4"
		}
	case "video":
		switch ext {
		case ".mp4":
			return "video/mp4"
		case ".webm":
			return "video/webm"
		}
	}
	return ""
}

// ── 动作请求/响应 ──

// ActionRequest 表示要发送给 OneBot 实现的动作请求
type ActionRequest struct {
	Action string         `json:"action"`         // 动作名，如 send_message / send_group_msg
	Params map[string]any `json:"params"`         // 动作参数
	Echo   string         `json:"echo,omitempty"` // 请求标识（用于匹配响应）
}

// ActionResponse 表示 OneBot 实现返回的动作响应
type ActionResponse struct {
	Status  string          `json:"status"`            // ok / failed
	RetCode int64           `json:"retcode"`           // 返回码
	Data    json.RawMessage `json:"data"`              // 响应数据
	Message string          `json:"message,omitempty"` // 错误信息
	Echo    string          `json:"echo,omitempty"`    // 请求标识
}

// ── 构建动作辅助 ──

// BuildSendMessageV12 构建 OneBot 12 send_message 动作
func BuildSendMessageV12(detailType, userID, groupID, text string) *ActionRequest {
	params := map[string]any{
		"detail_type": detailType, // private / group
		"message": []MessageSegmentV12{
			{Type: "text", Data: map[string]any{"text": text}},
		},
	}
	if detailType == "private" {
		params["user_id"] = userID
	} else {
		params["group_id"] = groupID
	}
	return &ActionRequest{
		Action: "send_message",
		Params: params,
	}
}

// BuildSendMessageV11 构建 OneBot 11 send_private_msg / send_group_msg 动作
func BuildSendMessageV11(isGroup bool, userID, groupID, text string) *ActionRequest {
	if isGroup {
		gid, _ := strconv.ParseInt(groupID, 10, 64)
		return &ActionRequest{
			Action: "send_group_msg",
			Params: map[string]any{
				"group_id": gid,
				"message": []MessageSegmentV11{
					{Type: "text", Data: map[string]any{"text": text}},
				},
			},
		}
	}
	uid, _ := strconv.ParseInt(userID, 10, 64)
	return &ActionRequest{
		Action: "send_private_msg",
		Params: map[string]any{
			"user_id": uid,
			"message": []MessageSegmentV11{
				{Type: "text", Data: map[string]any{"text": text}},
			},
		},
	}
}

// ── 富媒体发送构建 ──

// BuildSendMessageSegmentsV12 构建 OneBot 12 send_message 动作（支持富媒体段）。
func BuildSendMessageSegmentsV12(detailType, userID, groupID string, segs []NormalizedSegment) *ActionRequest {
	params := map[string]any{
		"detail_type": detailType,
		"message":     ToMessageSegmentV12(segs),
	}
	if detailType == "private" {
		params["user_id"] = userID
	} else {
		params["group_id"] = groupID
	}
	return &ActionRequest{Action: "send_message", Params: params}
}

// BuildSendMessageSegmentsV11 构建 OneBot 11 富媒体动作（send_group_msg / send_private_msg）。
func BuildSendMessageSegmentsV11(isGroup bool, userID, groupID string, segs []NormalizedSegment) *ActionRequest {
	if isGroup {
		gid, _ := strconv.ParseInt(groupID, 10, 64)
		return &ActionRequest{
			Action: "send_group_msg",
			Params: map[string]any{
				"group_id": gid,
				"message":  ToMessageSegmentV11(segs),
			},
		}
	}
	uid, _ := strconv.ParseInt(userID, 10, 64)
	return &ActionRequest{
		Action: "send_private_msg",
		Params: map[string]any{
			"user_id": uid,
			"message": ToMessageSegmentV11(segs),
		},
	}
}

// ToMessageSegmentV12 将标准化段转为 OneBot 12 消息段列表。
func ToMessageSegmentV12(segs []NormalizedSegment) []MessageSegmentV12 {
	result := make([]MessageSegmentV12, 0, len(segs))
	for _, s := range segs {
		switch s.Type {
		case "text":
			result = append(result, MessageSegmentV12{Type: "text", Data: map[string]any{"text": s.Text}})
		case "image":
			data := map[string]any{}
			if v := s.Data["file_id"]; v != "" {
				data["file_id"] = v
			} else if v := s.Data["file"]; v != "" {
				data["file"] = v
			} else if v := s.Data["url"]; v != "" {
				data["url"] = v
			}
			result = append(result, MessageSegmentV12{Type: "image", Data: data})
		case "at":
			uid := s.Data["user_id"]
			if uid == "" {
				uid = s.Data["qq"]
			}
			result = append(result, MessageSegmentV12{Type: "at", Data: map[string]any{"user_id": uid}})
		default:
			// 其余类型透传原始 data（转为 string 值，兼容 number 场景）
			data := make(map[string]any, len(s.Data))
			for k, v := range s.Data {
				data[k] = v
			}
			result = append(result, MessageSegmentV12{Type: s.Type, Data: data})
		}
	}
	return result
}

// ToMessageSegmentV11 将标准化段转为 OneBot 11 消息段列表。
func ToMessageSegmentV11(segs []NormalizedSegment) []MessageSegmentV11 {
	result := make([]MessageSegmentV11, 0, len(segs))
	for _, s := range segs {
		switch s.Type {
		case "text":
			result = append(result, MessageSegmentV11{Type: "text", Data: map[string]any{"text": s.Text}})
		case "image":
			// OneBot 11 file 支持 url / base64(data:image/...) / 本地路径
			file := s.Data["file"]
			if file == "" {
				file = s.Data["url"]
			}
			result = append(result, MessageSegmentV11{Type: "image", Data: map[string]any{"file": file}})
		case "at":
			uid := s.Data["user_id"]
			if uid == "" {
				uid = s.Data["qq"]
			}
			result = append(result, MessageSegmentV11{Type: "at", Data: map[string]any{"qq": uid}})
		default:
			data := make(map[string]any, len(s.Data))
			for k, v := range s.Data {
				data[k] = v
			}
			result = append(result, MessageSegmentV11{Type: s.Type, Data: data})
		}
	}
	return result
}

// ── 富文本发送内容 ──

// RichContent 富文本发送内容：一段文本 + 可选 @ 用户 + 可选图片。
// At / Image 为空时对应消息段不生成（空 At = 不 @，空 Image = 不发图）。
type RichContent struct {
	Text  string // 文本内容
	At    string // @ 目标用户 ID（空 = 不 @）
	Image string // 图片 file（URL / 本地路径 / base64，空 = 不发图）
}

// ── 辅助函数 ──

// formatIntOrString 将值格式化为字符串（处理 JSON number 或 string）
func formatIntOrString(v any) string {
	switch n := v.(type) {
	case json.Number:
		return n.String()
	case float64:
		return strconv.FormatInt(int64(n), 10)
	case int64:
		return strconv.FormatInt(n, 10)
	case string:
		return n
	default:
		return fmt.Sprintf("%v", v)
	}
}
