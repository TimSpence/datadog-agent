// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build otlp

package run

import (
	"context"

	agentConfig "github.com/DataDog/datadog-agent/cmd/otel-agent/config"
	"github.com/DataDog/datadog-agent/cmd/otel-agent/subcommands"
	"github.com/DataDog/datadog-agent/comp/api/authtoken/fetchonlyimpl"
	coreconfig "github.com/DataDog/datadog-agent/comp/core/config"
	"github.com/DataDog/datadog-agent/comp/core/hostname"
	"github.com/DataDog/datadog-agent/comp/core/hostname/hostnameimpl"
	corelog "github.com/DataDog/datadog-agent/comp/core/log"
	corelogimpl "github.com/DataDog/datadog-agent/comp/core/log/logimpl"
	"github.com/DataDog/datadog-agent/comp/core/log/tracelogimpl"
	"github.com/DataDog/datadog-agent/comp/core/secrets"
	"github.com/DataDog/datadog-agent/comp/core/sysprobeconfig"
	"github.com/DataDog/datadog-agent/comp/core/tagger"
	"github.com/DataDog/datadog-agent/comp/core/tagger/taggerimpl"
	"github.com/DataDog/datadog-agent/comp/core/workloadmeta"
	"github.com/DataDog/datadog-agent/comp/dogstatsd/statsd"
	"github.com/DataDog/datadog-agent/comp/forwarder"
	"github.com/DataDog/datadog-agent/comp/forwarder/defaultforwarder"
	"github.com/DataDog/datadog-agent/comp/forwarder/orchestrator/orchestratorinterface"
	"github.com/DataDog/datadog-agent/comp/metadata/inventoryagent/inventoryagentimpl"
	collectorcontribFx "github.com/DataDog/datadog-agent/comp/otelcol/collector-contrib/fx"
	collectordef "github.com/DataDog/datadog-agent/comp/otelcol/collector/def"
	collectorfx "github.com/DataDog/datadog-agent/comp/otelcol/collector/fx"
	"github.com/DataDog/datadog-agent/comp/otelcol/logsagentpipeline"
	"github.com/DataDog/datadog-agent/comp/otelcol/logsagentpipeline/logsagentpipelineimpl"
	"go.opentelemetry.io/collector/confmap"

	provider "github.com/DataDog/datadog-agent/comp/otelcol/provider/def"
	providerfx "github.com/DataDog/datadog-agent/comp/otelcol/provider/fx"
	"github.com/DataDog/datadog-agent/comp/serializer/compression"
	"github.com/DataDog/datadog-agent/comp/serializer/compression/compressionimpl/strategy"
	tracecomp "github.com/DataDog/datadog-agent/comp/trace"
	traceagentcomp "github.com/DataDog/datadog-agent/comp/trace/agent"
	traceconfig "github.com/DataDog/datadog-agent/comp/trace/config"
	"github.com/DataDog/datadog-agent/pkg/config/env"
	"github.com/DataDog/datadog-agent/pkg/serializer"
	"github.com/DataDog/datadog-agent/pkg/trace/telemetry"
	"github.com/DataDog/datadog-agent/pkg/util/fxutil"
	"github.com/DataDog/datadog-agent/pkg/util/optional"
	"github.com/spf13/cobra"

	"go.uber.org/fx"
)

// MakeCommand creates the `run` command
func MakeCommand(globalConfGetter func() *subcommands.GlobalParams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Starting OpenTelemetry Collector",
		RunE: func(cmd *cobra.Command, args []string) error {
			globalParams := globalConfGetter()
			return runOTelAgentCommand(context.Background(), globalParams)
		},
	}
	return cmd
}

type orchestratorinterfaceimpl struct {
	f defaultforwarder.Forwarder
}

func newOrchestratorinterfaceimpl(f defaultforwarder.Forwarder) orchestratorinterface.Component {
	return &orchestratorinterfaceimpl{
		f: f,
	}
}

func (o *orchestratorinterfaceimpl) Get() (defaultforwarder.Forwarder, bool) {
	return o.f, true
}

func (o *orchestratorinterfaceimpl) Reset() {
	o.f = nil
}

func runOTelAgentCommand(ctx context.Context, params *subcommands.GlobalParams, opts ...fx.Option) error {
	err := fxutil.Run(
		forwarder.Bundle(),
		tracelogimpl.Module(), // cannot have corelogimpl and tracelogimpl at the same time
		inventoryagentimpl.Module(),
		workloadmeta.Module(),
		hostnameimpl.Module(),
		statsd.Module(),
		sysprobeconfig.NoneModule(),
		fetchonlyimpl.Module(),
		collectorfx.Module(),
		collectorcontribFx.Module(),
		providerfx.Module(),
		fx.Provide(func(cp provider.Component) confmap.Converter {
			return cp
		}),
		fx.Provide(func() (coreconfig.Component, error) {
			c, err := agentConfig.NewConfigComponent(context.Background(), params.ConfPaths)
			if err != nil {
				return nil, err
			}
			env.DetectFeatures(c)
			return c, nil
		}),
		fx.Provide(func() workloadmeta.Params {
			return workloadmeta.NewParams()
		}),
		fx.Provide(func() []string {
			return append(params.ConfPaths, params.Sets...)
		}),

		fx.Supply(optional.NewNoneOption[secrets.Component]()),
		fx.Provide(func(c coreconfig.Component) corelogimpl.Params {
			return corelogimpl.ForOneShot(params.LoggerName, c.GetString("log_level"), true)
		}),
		logsagentpipelineimpl.Module(),
		// We create strategy.ZlibStrategy directly to avoid build tags
		fx.Provide(strategy.NewZlibStrategy),
		fx.Provide(func(s *strategy.ZlibStrategy) compression.Component {
			return s
		}),
		fx.Provide(serializer.NewSerializer),
		// For FX to provide the serializer.MetricSerializer from the serializer.Serializer
		fx.Provide(func(s *serializer.Serializer) serializer.MetricSerializer {
			return s
		}),
		fx.Provide(func(h hostname.Component) string {
			hn, _ := h.Get(context.Background())
			return hn
		}),

		fx.Provide(newForwarderParams),
		fx.Provide(func(c defaultforwarder.Component) (defaultforwarder.Forwarder, error) {
			return defaultforwarder.Forwarder(c), nil
		}),
		fx.Provide(newOrchestratorinterfaceimpl),
		fx.Options(opts...),
		fx.Invoke(func(_ collectordef.Component, _ defaultforwarder.Forwarder, _ optional.Option[logsagentpipeline.Component]) {
		}),

		fx.Provide(func(coreConfig coreconfig.Component) tagger.Params {
			if coreConfig.GetBool("apm_config.remote_tagger") {
				return tagger.NewNodeRemoteTaggerParamsWithFallback()
			}
			return tagger.NewTaggerParams()
		}),
		taggerimpl.Module(),
		fx.Provide(func(cfg traceconfig.Component) telemetry.TelemetryCollector {
			return telemetry.NewCollector(cfg.Object())
		}),

		// ctx is required to be supplied from here, as Windows needs to inject its own context
		// to allow the agent to work as a service.
		fx.Provide(func() context.Context { return ctx }), // fx.Supply(ctx) fails with a missing type error.
		fx.Supply(&traceagentcomp.Params{
			CPUProfile:  "",
			MemProfile:  "",
			PIDFilePath: "",
		}),
		tracecomp.Bundle(),
	)
	if err != nil {
		return err
	}
	return nil
}

func newForwarderParams(config coreconfig.Component, log corelog.Component) defaultforwarder.Params {
	return defaultforwarder.NewParams(config, log)
}
