//go:build !confonly
// +build !confonly

package router

import (
	"context"

	core "github.com/v2fly/v2ray-core/v5"
	"github.com/v2fly/v2ray-core/v5/common"
	"github.com/v2fly/v2ray-core/v5/features"
	"github.com/v2fly/v2ray-core/v5/features/extension"
)

type LeastPingStrategy struct {
	ctx         context.Context
	observatory extension.Observatory

	config *StrategyLeastPingConfig
}

func (l *LeastPingStrategy) GetPrincipleTarget(strings []string) []string {
	return []string{l.PickOutbound(strings)}
}

func (l *LeastPingStrategy) InjectContext(ctx context.Context) {
	l.ctx = ctx
}

func (l *LeastPingStrategy) PickOutbound(strings []string) string {
	if l.observatory == nil {
		common.Must(core.RequireFeatures(l.ctx, func(observatory extension.Observatory) error {
			if l.config.ObserverTag != "" {
				l.observatory = common.Must2(observatory.(features.TaggedFeatures).GetFeaturesByTag(l.config.ObserverTag)).(extension.Observatory)
			} else {
				l.observatory = observatory
			}
			return nil
		}))
	}

	observeReport, err := l.observatory.GetObservation(l.ctx)
	if err != nil {
		newError("cannot get observe report").Base(err).WriteToLog()
		return ""
	}
	outboundsList := outboundList(strings)
	return ""
}

type outboundList []string

func (o outboundList) contains(name string) bool {
	for _, v := range o {
		if v == name {
			return true
		}
	}
	return false
}

func init() {
	common.Must(common.RegisterConfig((*StrategyLeastPingConfig)(nil), nil))
}
