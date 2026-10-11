package account

import (
	"context"
	"errors"
	"strings"
)

// ErrMediaDisabled 表示没有可写的公开 bucket（auth.avatars 未配置，或它的密钥不在环境里），PublishMedia 无处可写。
var ErrMediaDisabled = errors.New("media storage disabled")

// PublishMedia 把 body 写进头像所用的公开 R2 bucket 的 key 下，返回公开地址（auth.avatars.public_base_url 加 key）。key 由调用方生成且不得落在头像的 avatars/ 前缀下；contentType 必须是调用方已经按文件内容核实过的类型，因为对象就以这个类型从公开域名提供。对象只写一次、长期缓存，与头像相同。
func (a *Service) PublishMedia(ctx context.Context, key string, body []byte, contentType string) (string, error) {
	if a == nil || a.avatars == nil || a.config.Avatars.PublicBaseURL == "" {
		return "", ErrMediaDisabled
	}
	if key == "" || strings.HasPrefix(key, "avatars/") || strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
		return "", errors.New("invalid media key")
	}
	if err := a.avatars.Put(ctx, key, body, contentType); err != nil {
		return "", err
	}
	return strings.TrimSuffix(a.config.Avatars.PublicBaseURL, "/") + "/" + key, nil
}
