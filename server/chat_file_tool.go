package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

func (s *Server) chatFileTools() []actool.CoreTool {
	return []actool.CoreTool{actool.Build(actool.Spec{
		Name:        "send_file",
		Description: "将已经生成的本地文件作为可下载附件发送给当前会话用户。文件先复制保存，源文件后续修改或工作树删除不影响下载。支持任意普通文件，单个不超过 128 MiB；目录请先打包。仅在主对话调用，子任务将文件路径交给主代理审查后发送。",
		Schema:      objSchema(map[string]any{"path": strParam("要发送的已有文件路径；相对当前工作目录解析，也支持绝对路径"), "name": strParam("可选：用户看到的文件名，默认源文件名，不含目录"), "caption": strParam("可选：附件旁的简短说明")}, "path"),
		Concurrent:  func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: s.sendChatFile,
	})}
}

func (s *Server) sendChatFile(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if s.m.pg == nil {
		return actool.Errorf("会话数据库未就绪"), nil
	}
	session := agent.RunInfoFrom(ctx).SessionID
	id, err := conversationSessionID(session)
	if err != nil {
		return actool.Errorf("send_file 需要当前主对话；子任务请报告文件路径，由主代理发送"), nil
	}
	conv, err := s.m.pg.GetConversation(id)
	if err != nil || conv == nil {
		return actool.Errorf("当前会话不存在或暂时不可用"), nil
	}
	var request struct {
		Path    string `json:"path"`
		Name    string `json:"name"`
		Caption string `json:"caption"`
	}
	if err = json.Unmarshal(input, &request); err != nil {
		return actool.Errorf("附件参数无效"), nil
	}
	if strings.TrimSpace(request.Path) == "" {
		return actool.Errorf("path 不能为空"), nil
	}
	if len(request.Caption) > 8000 {
		return actool.Errorf("附件说明过长"), nil
	}
	source := request.Path
	if !filepath.IsAbs(source) {
		base := filepath.Join(s.m.dir, "sessions", session)
		if tc != nil && tc.WorkingDir != "" {
			base = tc.WorkingDir
		}
		source = filepath.Join(base, source)
	}
	file, err := s.copyChatFile(ctx, id, source, request.Name)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	remove := func() { _ = os.RemoveAll(filepath.Join(s.chatFileDir(id), file.ID)); _ = os.Remove(s.chatFileDir(id)) }
	if err = ctx.Err(); err != nil {
		remove()
		return actool.Errorf("发送已取消"), nil
	}
	body, _ := json.Marshal(chatFileMessage{Text: request.Caption, Attachments: []chatFile{file}})
	if _, err = s.m.pg.AppendConvActivity(id, db.Activity{Worker: conv.AgentKey, Kind: "file", Tool: "send_file", Summary: fmt.Sprintf("%s（%s）", file.Name, humanBytes(file.Size)), Detail: string(body)}); err != nil {
		remove()
		return actool.Errorf("保存附件消息失败，请重试"), nil
	}
	_ = s.m.pg.TouchConversation(id)
	return jsonResult(map[string]any{"status": "sent", "file": file, "message": "文件已发送，用户可以在当前会话的附件卡片下载。"})
}
