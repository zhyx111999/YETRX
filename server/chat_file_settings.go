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
