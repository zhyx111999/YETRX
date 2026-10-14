package server

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	settingChatFileMaxMiB       = "chat_file_max_mib"
	defaultChatFileMaxMiB int64 = 128
	// Keep the byte conversion and the one-byte overflow probe within int64.
	maxChatFileMaxMiB int64 = (1<<63 - 1) >> 20
)

func validateChatFileMaxMiB(value int64) error {
	if value < 0 || value > maxChatFileMaxMiB {
		return fmt.Errorf("文件大小上限需为 0 到 %d 之间的整数（MiB），0 表示不限制", maxChatFileMaxMiB)
	}
	return nil
}

// Read once per send, so settings changes apply to the next copy and never
// change the limit midway through a file. Zero explicitly disables the cap.
func (s *Server) chatFileMaxMiB() (int64, error) {
	if s.m.pg == nil {
		return defaultChatFileMaxMiB, nil
	}
	value, exists, err := s.m.pg.GetSetting(settingChatFileMaxMiB)
	if err != nil {
		return 0, fmt.Errorf("无法读取文件大小配置，请稍后重试")
	}
	if !exists {
		return defaultChatFileMaxMiB, nil
	}
	limit, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || validateChatFileMaxMiB(limit) != nil {
		return 0, fmt.Errorf("文件大小配置无效，请在系统配置中重新保存")
	}
	return limit, nil
}

// Only replace the previous built-in text. Custom descriptions, schemas,
// defaults and bindings remain owned by the user.
func (s *Server) upgradeChatFileDescription() error {
	const previous = "将已经生成的本地文件作为可下载附件发送给当前会话用户。文件先复制保存，源文件后续修改或工作树删除不影响下载。支持任意普通文件，单个不超过 128 MiB；目录请先打包。仅在主对话调用，子任务将文件路径交给主代理审查后发送。"
	_, err := s.m.pg.Exec(`UPDATE tools SET description=$1 WHERE key='send_file' AND system AND description=$2`, chatFileDescription, previous)
	return err
}
