package kitexx

import (
	"fmt"
	"strings"

	"github.com/nacos-group/nacos-sdk-go/v2/clients/config_client"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"

	"github.com/sezznaw/devkit-common/nacosx"
	"github.com/sezznaw/devkit-common/zlog"
)

func watchLogLevelFromNacos(cfg Config) error {
	cc, err := nacosx.NewConfigClient(cfg.Nacos)
	if err != nil {
		return err
	}
	return watchLogLevel(cc, cfg.LogLevelDataID, cfg.Nacos.GroupName())
}

// watchLogLevel applies the level stored in a Nacos configuration now and
// whenever it changes. An empty configuration leaves the level alone, so the
// data id can exist before anybody needs it.
func watchLogLevel(cc config_client.IConfigClient, dataID, group string) error {
	apply := func(content string) {
		level := strings.ToLower(strings.TrimSpace(content))
		if level == "" {
			return
		}
		zlog.SetLevel(level)
		zlog.Warn("log level set from Nacos", zlog.Str("data_id", dataID), zlog.Str("to", level))
	}
	content, err := cc.GetConfig(vo.ConfigParam{DataId: dataID, Group: group})
	if err != nil {
		return fmt.Errorf("kitexx: read %s from Nacos: %w", dataID, err)
	}
	apply(content)
	err = cc.ListenConfig(vo.ConfigParam{
		DataId: dataID,
		Group:  group,
		OnChange: func(_, _, _, data string) {
			apply(data)
		},
	})
	if err != nil {
		return fmt.Errorf("kitexx: listen to %s in Nacos: %w", dataID, err)
	}
	return nil
}
